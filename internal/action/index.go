package action

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- Git object identity, not a security signature.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/workspace"
)

// fixIndex keeps Git metadata outside every model grant. It has no remotes,
// repository configuration, credential environment, hooks or checkout filters.
type fixIndex struct {
	dir, source, binary, baseline string
	tracked                       map[string]int64
}

func newFixIndex(ctx context.Context, dir, source string, files []workspace.File) (*fixIndex, error) {
	binary, err := gitwork.ResolveGit()
	if err != nil {
		return nil, fmt.Errorf("offline fix index requires git: %w", err)
	}
	index := &fixIndex{dir: dir, source: source, binary: binary, tracked: make(map[string]int64, len(files))}
	for _, file := range files {
		index.tracked[file.Path] = int64(len(file.Data))
	}
	if _, err := index.run(ctx, 1024, nil, "init", "--quiet", "--template=", "--object-format=sha1"); err != nil {
		return nil, err
	}
	// info/attributes overrides untrusted .gitattributes even for forced baseline files.
	if err := gitwork.DisableAttributes(dir); err != nil {
		return nil, err
	}
	if _, err := index.run(ctx, 1024, nil, "add", "--all", "--force", "--", "."); err != nil {
		return nil, err
	}
	tree, err := index.run(ctx, 128, nil, "write-tree")
	if err != nil {
		return nil, err
	}
	index.baseline = strings.TrimSpace(string(tree))
	if !gitObjectID(index.baseline) {
		return nil, errors.New("offline git returned invalid tree identity")
	}
	return index, nil
}

func (index *fixIndex) changes(ctx context.Context) ([]workspace.Change, error) {
	if err := index.checkControlFiles(ctx); err != nil {
		return nil, err
	}
	total := int64(0)
	for name, size := range index.tracked {
		filename := filepath.Join(index.source, filepath.FromSlash(name))
		if err := workspace.CheckRoot(filepath.Dir(filename)); err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
				// Git selects the replacement; this baseline descendant no longer exists.
				continue
			}
			return nil, fmt.Errorf("tracked fix parent %q: %w", name, err)
		}
		info, err := os.Lstat(filename)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("tracked fix path %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			// Git diagnoses type changes; no data is opened here.
			continue
		}
		if info.Size() != size {
			if info.Size() > int64(workspace.MaxChangeBytes)-total {
				return nil, fmt.Errorf("tracked fix path %q exceeds changed byte limit", name)
			}
			total += info.Size()
		}
	}
	if _, err := index.run(ctx, 1024, nil, "read-tree", index.baseline); err != nil {
		return nil, err
	}
	// Bound new output before Git hashes it; ignored files and metadata remain excluded.
	untracked, err := index.run(ctx, (workspace.MaxChanges+1)*(workspace.MaxPathBytes+1), nil, "ls-files", "-z", "--others", "--exclude-standard", "--", ".", ":(exclude,icase,glob)**/.git", ":(exclude,icase,glob)**/.git/**")
	if err != nil {
		return nil, err
	}
	paths, err := gitwork.SplitNUL(untracked)
	if err != nil {
		return nil, fmt.Errorf("invalid offline git untracked paths: %w", err)
	}
	if len(paths) > workspace.MaxChanges {
		return nil, errors.New("fix result exceeds changed file limit")
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := string(path)
		if strings.HasSuffix(name, "/") {
			return nil, fmt.Errorf("untracked nested repository output %q must be removed or ignored", name)
		}
		if !workspace.ValidPath(name) {
			return nil, fmt.Errorf("fix output %q is not a portable regular-file path", name)
		}
		info, err := os.Lstat(filepath.Join(index.source, filepath.FromSlash(name)))
		if err != nil {
			return nil, fmt.Errorf("fix output %q: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() > int64(workspace.MaxChangeBytes)-total {
			return nil, fmt.Errorf("fix output %q is not regular or exceeds changed byte limit", name)
		}
		total += info.Size()
	}
	// shortcut: size prechecks can race and do not cap Git I/O; add isolated executor quotas (#96).
	if _, err := index.run(ctx, 1024, nil, "add", "--all", "--", ".", ":(exclude,icase,glob)**/.git", ":(exclude,icase,glob)**/.git/**"); err != nil {
		return nil, err
	}
	raw, err := index.run(ctx, (workspace.MaxChanges+1)*(workspace.MaxPathBytes+128), nil, "diff", "--cached", "--raw", "-z", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", index.baseline, "--")
	if err != nil {
		return nil, err
	}
	selected, objects, err := parseIndexChanges(raw)
	if err != nil {
		return nil, err
	}
	changes, err := workspace.ReadChanges(ctx, index.source, selected)
	if err != nil {
		return nil, err
	}
	// Bind returned bytes to the frozen index whose patch-size limit is checked below.
	for i, change := range changes {
		if !change.Delete && gitBlobID(change.Data) != objects[i] {
			return nil, fmt.Errorf("fix file %q changed after indexing", change.Path)
		}
	}
	if _, err := index.run(ctx, gitwork.MaxPatchBytes, nil, "diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--submodule=short", index.baseline, "--"); err != nil {
		return nil, err
	}
	return changes, nil
}

// Check control-file shapes before Git can open FIFOs while discovering output.
// Ignored directories are skipped unless they contain supplied tracked files.
// shortcut: live writers can race these checks; isolate and quiesce executors in #96.
func (index *fixIndex) checkControlFiles(ctx context.Context) error {
	trackedDirs := map[string]bool{}
	for name := range index.tracked {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			trackedDirs[parent] = true
		}
	}
	dirs, entries := []string{"."}, 0
	for len(dirs) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		absolute := filepath.Join(index.source, filepath.FromSlash(dir))
		for _, control := range []string{".gitignore", ".gitattributes"} {
			name := path.Join(dir, control)
			info, err := os.Lstat(filepath.Join(absolute, control))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("fix control file %q: %w", name, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("fix control file %q must be regular", name)
			}
			if _, tracked := index.tracked[name]; control == ".gitignore" && !tracked {
				input := []byte(":(top)" + name + "\x00")
				ignored, err := index.run(ctx, len(input), input, "check-ignore", "--no-index", "-z", "--stdin")
				if err != nil {
					return err
				}
				if len(ignored) != 0 {
					return fmt.Errorf("ignored untracked fix control file %q must be removed or tracked", name)
				}
			}
		}
		f, err := workspace.OpenDirectory(absolute)
		if err != nil {
			return fmt.Errorf("fix directory %q: %w", dir, err)
		}
		scanErr := func() error {
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				batch, err := f.ReadDir(128)
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				var candidates []string
				var input []byte
				for _, entry := range batch {
					entries++
					if entries > workspace.MaxEntries {
						return errors.New("fix directory scan exceeds entry limit")
					}
					if entry.Name() == ".git" && dir != "." && !trackedDirs[dir] {
						return fmt.Errorf("untracked nested repository output %q must be removed or ignored", dir+"/")
					}
					if !entry.IsDir() || strings.EqualFold(entry.Name(), ".git") {
						continue
					}
					name := path.Join(dir, entry.Name())
					if trackedDirs[name] {
						dirs = append(dirs, name)
						continue
					}
					candidates = append(candidates, name)
					input = append(input, ":(top)"...)
					input = append(input, name...)
					input = append(input, 0)
				}
				if len(candidates) != 0 {
					// No trailing slash: check-ignore must not open a child's unchecked .gitignore.
					ignored, err := index.run(ctx, len(input), input, "check-ignore", "--no-index", "-z", "--stdin")
					if err != nil {
						return err
					}
					skip := map[string]bool{}
					for _, name := range bytes.Split(ignored, []byte{0}) {
						skip[strings.TrimPrefix(string(name), ":(top)")] = true
					}
					for _, name := range candidates {
						if !skip[name] {
							dirs = append(dirs, name)
						}
					}
				}
				if errors.Is(err, io.EOF) {
					return nil
				}
			}
		}()
		if err := errors.Join(scanErr, f.Close()); err != nil {
			return err
		}
	}
	return nil
}

func parseIndexChanges(raw []byte) ([]workspace.Change, []string, error) {
	parts, err := gitwork.SplitNUL(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid offline git diff: %w", err)
	}
	if len(parts)%2 != 0 {
		return nil, nil, errors.New("invalid offline git diff")
	}
	if len(parts)/2 > workspace.MaxChanges {
		return nil, nil, errors.New("fix result exceeds changed file limit")
	}
	changes, objects := make([]workspace.Change, 0, len(parts)/2), make([]string, 0, len(parts)/2)
	for i := 0; i < len(parts); i += 2 {
		fields := strings.Fields(string(parts[i]))
		if len(fields) != 5 || len(fields[0]) != 7 || fields[0][0] != ':' || len(fields[1]) != 6 || !gitObjectID(fields[2]) || !gitObjectID(fields[3]) {
			return nil, nil, errors.New("invalid offline git change record")
		}
		mode, err := strconv.ParseUint(fields[1], 8, 32)
		if err != nil {
			return nil, nil, errors.New("invalid offline git file mode")
		}
		change := workspace.Change{Path: string(parts[i+1]), Mode: uint32(mode)}
		switch fields[4] {
		case "D":
			change.Delete, change.Mode = true, 0
		case "A", "M":
		default:
			return nil, nil, fmt.Errorf("unsupported offline git change at %q", change.Path)
		}
		changes, objects = append(changes, change), append(objects, fields[3])
	}
	return changes, objects, nil
}

func gitObjectID(value string) bool {
	return len(value) == 40 && workspace.ValidSHA(value)
}

func gitBlobID(data []byte) string {
	// #nosec G401 -- this reproduces the private index's SHA-1 object identifier.
	hash := sha1.New()
	_, _ = fmt.Fprintf(hash, "blob %d\x00", len(data))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func (index *fixIndex) run(ctx context.Context, limit int, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	gitArgs := append(gitwork.HardeningArgs(), "--git-dir="+index.dir, "--work-tree="+index.source,
		"-c", "credential.helper=", "-c", "protocol.allow=never")
	cmd := exec.CommandContext(ctx, index.binary, append(gitArgs, args...)...)
	processgroup.Configure(cmd)
	cmd.Dir = index.dir
	cmd.Env = []string{"PATH=" + gitwork.SafePath(os.Getenv("PATH")), "HOME=" + index.dir, "TMPDIR=" + index.dir, "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GIT_NO_REPLACE_OBJECTS=1"}
	cmd.WaitDelay = 2 * time.Second
	output, diagnostic := bounded.Buffer{Limit: limit, Cancel: cancel}, bounded.Buffer{Limit: 8 << 10}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &output, &diagnostic, bytes.NewReader(input)
	err := cmd.Run()
	if output.Exceeded {
		return nil, fmt.Errorf("offline git %s output exceeds action limit", args[0])
	}
	if err != nil {
		var exit *exec.ExitError
		if args[0] == "check-ignore" && errors.As(err, &exit) && exit.ExitCode() == 1 && ctx.Err() == nil {
			return output.Bytes(), nil
		}
		if contextErr := ctx.Err(); contextErr != nil {
			err = errors.Join(err, contextErr)
		} else {
			err = gitwork.ClassifyPathError(args[0], diagnostic.String(), err)
		}
		return nil, fmt.Errorf("offline git %s: %w: %q", args[0], err, strings.TrimSpace(diagnostic.String()))
	}
	return output.Bytes(), nil
}
