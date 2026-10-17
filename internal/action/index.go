package action

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- Git object identity, not a security signature.
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/workspace"
)

// fixIndex keeps Git metadata outside every model grant. It has no remotes,
// repository configuration, credential environment, hooks or checkout filters.
type fixIndex struct {
	dir, source, binary, baseline string
}

func newFixIndex(ctx context.Context, dir, source string) (*fixIndex, error) {
	binary, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("offline fix index requires git: %w", err)
	}
	// execerrdot=0 can make LookPath return a relative executable without an error.
	if !filepath.IsAbs(binary) {
		return nil, errors.New("offline git binary must have an absolute path")
	}
	index := &fixIndex{dir: dir, source: source, binary: binary}
	if _, err := index.run(ctx, 1024, "init", "--quiet", "--template=", "--object-format=sha1"); err != nil {
		return nil, err
	}
	// info/attributes overrides untrusted .gitattributes even for forced baseline files.
	if err := os.MkdirAll(filepath.Join(dir, "info"), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "info", "attributes"), []byte(workspace.GitAttributes), 0600); err != nil {
		return nil, err
	}
	if _, err := index.run(ctx, 1024, "add", "--all", "--force", "--", "."); err != nil {
		return nil, err
	}
	tree, err := index.run(ctx, 128, "write-tree")
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
	if _, err := index.run(ctx, 1024, "read-tree", index.baseline); err != nil {
		return nil, err
	}
	// Bound new output before Git hashes it; ignored files and metadata remain excluded.
	untracked, err := index.run(ctx, (workspace.MaxChanges+1)*(workspace.MaxPathBytes+1), "ls-files", "-z", "--others", "--exclude-standard", "--", ".", ":(exclude,icase,glob)**/.git", ":(exclude,icase,glob)**/.git/**")
	if err != nil {
		return nil, err
	}
	if len(untracked) != 0 {
		if untracked[len(untracked)-1] != 0 {
			return nil, errors.New("invalid offline git untracked paths")
		}
		paths := bytes.Split(untracked[:len(untracked)-1], []byte{0})
		if len(paths) > workspace.MaxChanges {
			return nil, errors.New("fix result exceeds changed file limit")
		}
		total := 0
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name := string(path)
			if strings.HasSuffix(name, "/") {
				return nil, fmt.Errorf("nested repository output %q must be removed or ignored", name)
			}
			if !workspace.ValidPath(name) {
				return nil, fmt.Errorf("fix output %q is not a portable regular-file path", name)
			}
			data, err := workspace.ReadRegular(filepath.Join(index.source, filepath.FromSlash(name)), int64(workspace.MaxChangeBytes-total))
			if err != nil {
				return nil, fmt.Errorf("fix output %q: %w", name, err)
			}
			total += len(data)
		}
	}
	// shortcut: tracked edits are hashed before result limits; add I/O quotas with isolated executors (#96).
	if _, err := index.run(ctx, 1024, "add", "--all", "--", ".", ":(exclude,icase,glob)**/.git", ":(exclude,icase,glob)**/.git/**"); err != nil {
		return nil, err
	}
	raw, err := index.run(ctx, (workspace.MaxChanges+1)*(workspace.MaxPathBytes+128), "diff", "--cached", "--raw", "-z", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", index.baseline, "--")
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
			return nil, errors.New("fix file changed after indexing")
		}
	}
	if _, err := index.run(ctx, gitwork.MaxPatchBytes, "diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--submodule=short", index.baseline, "--"); err != nil {
		return nil, err
	}
	return changes, nil
}

func parseIndexChanges(raw []byte) ([]workspace.Change, []string, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	if raw[len(raw)-1] != 0 {
		return nil, nil, errors.New("invalid offline git diff")
	}
	parts := bytes.Split(raw[:len(raw)-1], []byte{0})
	if len(parts)%2 != 0 || len(parts)/2 > workspace.MaxChanges {
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
		case "A", "M", "T":
			if change.Mode != workspace.Regular && change.Mode != workspace.Executable {
				return nil, nil, fmt.Errorf("fix changed a non-regular file %q", change.Path)
			}
		default:
			return nil, nil, fmt.Errorf("unsupported offline git change at %q", change.Path)
		}
		changes, objects = append(changes, change), append(objects, fields[3])
	}
	return changes, objects, nil
}

func gitObjectID(value string) bool {
	return len(value) == 40 && strings.Trim(value, "0123456789abcdef") == ""
}

func gitBlobID(data []byte) string {
	// #nosec G401 -- this reproduces the private index's SHA-1 object identifier.
	hash := sha1.New()
	_, _ = fmt.Fprintf(hash, "blob %d\x00", len(data))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func (index *fixIndex) run(ctx context.Context, limit int, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	gitArgs := append(gitwork.HardeningArgs(), "--git-dir="+index.dir, "--work-tree="+index.source,
		"-c", "credential.helper=", "-c", "core.fileMode=true", "-c", "protocol.allow=never")
	cmd := exec.CommandContext(ctx, index.binary, append(gitArgs, args...)...)
	cmd.Dir, cmd.Env = index.dir, index.environment()
	cmd.WaitDelay = 2 * time.Second
	output, diagnostic := bounded.Buffer{Limit: limit, Cancel: cancel}, bounded.Buffer{Limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = &output, &diagnostic
	err := cmd.Run()
	if output.Exceeded {
		return nil, errors.New("offline git output exceeds action limit")
	}
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			err = errors.Join(err, contextErr)
		} else {
			err = gitwork.ClassifyPathError(args[0], diagnostic.String(), err)
		}
		return nil, fmt.Errorf("offline git %s: %w: %q", args[0], err, strings.TrimSpace(diagnostic.String()))
	}
	return output.Bytes(), nil
}

func (index *fixIndex) environment() []string {
	var paths []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(dir) {
			paths = append(paths, dir)
		}
	}
	if len(paths) == 0 {
		paths = []string{"/usr/bin", "/bin"}
	}
	return []string{"PATH=" + strings.Join(paths, string(os.PathListSeparator)), "HOME=" + index.dir, "TMPDIR=" + index.dir, "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GIT_NO_REPLACE_OBJECTS=1"}
}
