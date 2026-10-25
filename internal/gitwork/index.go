package gitwork

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- Git object identity, not a security signature.
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/executil"
	"github.com/linkdata/minion/internal/workspace"
)

const (
	excludeGit         = ":(exclude,icase,glob)**/.git"
	excludeGitContents = ":(exclude,icase,glob)**/.git/**"
)

// OfflineIndex runs offline Git against explicitly selected metadata and source.
// NewOfflineIndex refuses metadata overlapping the model source.
// MaterializeRepository reuses the command runner for metadata in source/.git.
type OfflineIndex struct {
	dir, source, binary, baseline string
	tracked                       map[string]int64
	executable                    map[string]bool
	controls                      map[string][sha256.Size]byte
}

// NewOfflineIndex creates private Git metadata for a materialized source snapshot.
// The caller supplies canonical absolute directories, with metadata outside source and model grants.
func NewOfflineIndex(ctx context.Context, dir, source string, files []workspace.File) (*OfflineIndex, error) {
	if workspace.Within(source, dir) || workspace.Within(dir, source) {
		return nil, errors.New("offline index and source directories overlap")
	}
	binary, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		return nil, fmt.Errorf("offline fix index requires git: %w", err)
	}
	index := &OfflineIndex{dir: dir, source: source, binary: binary, tracked: make(map[string]int64, len(files)), executable: make(map[string]bool), controls: make(map[string][sha256.Size]byte)}
	for _, file := range files {
		index.tracked[file.Path] = int64(len(file.Data))
		if file.Mode == workspace.Executable {
			index.executable[file.Path] = true
		}
		switch path.Base(file.Path) {
		case ".gitignore", ".gitattributes":
			index.controls[file.Path] = sha256.Sum256(file.Data)
		}
	}
	if _, err := index.run(ctx, 1024, nil, "init", "--quiet", "--template=", "--object-format=sha1"); err != nil {
		return nil, err
	}
	// info/attributes overrides untrusted .gitattributes even for forced baseline files.
	if err := disableAttributes(dir); err != nil {
		return nil, err
	}
	if _, err := index.run(ctx, 1024, nil, "add", "--all", "--force", "--", "."); err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		var executablePaths strings.Builder
		for _, file := range files {
			if file.Mode == workspace.Executable {
				executablePaths.WriteString(file.Path)
				executablePaths.WriteByte(0)
			}
		}
		if executablePaths.Len() != 0 {
			if _, err := index.run(ctx, 1024, []byte(executablePaths.String()), "update-index", "--chmod=+x", "-z", "--stdin"); err != nil {
				return nil, fmt.Errorf("restore executable source modes: %w", err)
			}
		}
	}
	tree, err := index.run(ctx, 128, nil, "write-tree")
	if err != nil {
		return nil, err
	}
	index.baseline = strings.TrimSpace(string(tree))
	if !workspace.ValidSHA(index.baseline) {
		return nil, errors.New("offline git returned invalid tree identity")
	}
	return index, nil
}

// Changes captures bounded regular-file changes using the candidate's current ignore rules.
func (index *OfflineIndex) Changes(ctx context.Context) ([]workspace.Change, error) {
	if err := index.checkControlFiles(ctx); err != nil {
		return nil, err
	}
	total := int64(0)
	for _, name := range slices.Sorted(maps.Keys(index.tracked)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		size := index.tracked[name]
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
		if info.IsDir() {
			if _, err := os.Lstat(filepath.Join(filename, ".git")); err == nil {
				return nil, fmt.Errorf("tracked fix path %q contains nested Git metadata", name)
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("tracked fix path %q nested Git metadata: %w", name, err)
			}
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
	// Rebuild baseline entries without cached worktree stats before each capture.
	if _, err := index.run(ctx, 1024, nil, "read-tree", index.baseline); err != nil {
		return nil, err
	}
	ignoreCase, err := index.run(ctx, 16, nil, "config", "--bool", "--default=false", "core.ignoreCase")
	if err != nil {
		return nil, err
	}
	var renamedExecutable map[string]bool
	var stale []byte
	if strings.TrimSpace(string(ignoreCase)) == "true" || runtime.GOOS == "windows" {
		// Git keeps the old index spelling for case-only renames until it is removed.
		// Windows also needs the old mode before Git stages a newly spelled file.
		stale, err = index.caseRenamedPaths(ctx)
		if err != nil {
			return nil, err
		}
		if len(stale) != 0 {
			if _, err := index.run(ctx, 1024, stale, "update-index", "--force-remove", "-z", "--stdin"); err != nil {
				return nil, err
			}
			if runtime.GOOS == "windows" {
				// Git cannot infer the executable bit from a newly spelled Windows file.
				renamedExecutable = make(map[string]bool)
				for _, old := range bytes.Split(stale, []byte{0}) {
					if index.executable[string(old)] {
						renamedExecutable[workspace.FoldPath(string(old))] = true
					}
				}
			}
		}
	}
	// Bound new output before Git hashes it; ignored files and metadata remain excluded.
	untracked, err := index.run(ctx, maxChangedPathListBytes, nil, "ls-files", "-z", "--others", "--exclude-standard", "--", ".", excludeGit, excludeGitContents)
	if err != nil {
		if errors.Is(err, ErrCheckoutTooLarge) {
			return nil, fmt.Errorf("%w: %v", ErrTooManyFiles, err)
		}
		return nil, err
	}
	paths, err := splitNUL(untracked)
	if err != nil {
		return nil, fmt.Errorf("invalid offline git untracked paths: %w", err)
	}
	if len(paths) > workspace.MaxChanges {
		return nil, fmt.Errorf("%w: fix result exceeds changed file limit", ErrTooManyFiles)
	}
	var executablePaths []byte
	for _, record := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := string(record)
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
		if renamedExecutable[workspace.FoldPath(name)] {
			executablePaths = append(executablePaths, record...)
			executablePaths = append(executablePaths, 0)
		}
	}
	// shortcut: size prechecks can race and do not cap Git I/O; use VM or container storage limits if hard bounds become required.
	if _, err := index.run(ctx, 1024, nil, "add", "--all", "--", ".", excludeGit, excludeGitContents); err != nil {
		return nil, err
	}
	if len(executablePaths) != 0 {
		if _, err := index.run(ctx, 1024, executablePaths, "update-index", "--chmod=+x", "-z", "--stdin"); err != nil {
			return nil, fmt.Errorf("restore renamed executable source modes: %w", err)
		}
	}
	raw, err := index.run(ctx, maxChangedPathListBytes, nil, "diff", "--cached", "--raw", "-z", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", index.baseline, "--")
	if err != nil {
		if errors.Is(err, ErrCheckoutTooLarge) {
			return nil, fmt.Errorf("%w: %v", ErrTooManyFiles, err)
		}
		return nil, err
	}
	selected, objects, err := parseIndexChanges(raw)
	if err != nil {
		return nil, err
	}
	if len(stale) == 0 && strings.TrimSpace(string(ignoreCase)) != "true" {
		for _, change := range selected {
			if change.Delete {
				stale, err = index.caseRenamedPaths(ctx)
				if err != nil {
					return nil, err
				}
				break
			}
		}
	}
	if len(stale) != 0 {
		staged := make(map[string]bool, len(selected))
		for _, change := range selected {
			if !change.Delete {
				staged[workspace.FoldPath(change.Path)] = true
			}
		}
		for _, old := range bytes.Split(stale[:len(stale)-1], []byte{0}) {
			if !staged[workspace.FoldPath(string(old))] {
				return nil, fmt.Errorf("case-renamed fix path %q is not staged under its new spelling (possibly ignored)", old)
			}
		}
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
	if _, err := index.run(ctx, MaxPatchBytes, nil, append(patchDiffArgs(), index.baseline, "--")...); err != nil {
		if errors.Is(err, ErrCheckoutTooLarge) {
			return nil, ErrDiffTooLarge
		}
		return nil, err
	}
	return changes, nil
}

// caseRenamedPaths finds tracked names whose on-disk spelling changed under Git's case folding.
func (index *OfflineIndex) caseRenamedPaths(ctx context.Context) ([]byte, error) {
	type wantedNames struct {
		exact, folded map[string]bool
	}
	wanted := map[string]wantedNames{}
	for name := range index.tracked {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parent := "."
		for _, part := range strings.Split(name, "/") {
			key := workspace.FoldPath(parent)
			names := wanted[key]
			if names.exact == nil {
				names = wantedNames{exact: map[string]bool{}, folded: map[string]bool{}}
			}
			names.exact[part] = true
			names.folded[workspace.FoldPath(part)] = true
			wanted[key] = names
			parent = path.Join(parent, part)
		}
	}
	directories := map[string]map[string][]string{}
	var stale []byte
	for _, name := range slices.Sorted(maps.Keys(index.tracked)) {
		actual := "."
		found := true
		for _, part := range strings.Split(name, "/") {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			names, cached := directories[actual]
			if !cached {
				filename := filepath.Join(index.source, filepath.FromSlash(actual))
				f, err := workspace.OpenDirectory(filename)
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
					found = false
					break
				}
				if err != nil {
					return nil, fmt.Errorf("inspect fix directory %q: %w", actual, err)
				}
				names = map[string][]string{}
				need := wanted[workspace.FoldPath(actual)]
				remaining := len(need.exact)
				for {
					if err := ctx.Err(); err != nil {
						_ = f.Close()
						return nil, err
					}
					entries, readErr := f.ReadDir(128)
					for _, entry := range entries {
						key := workspace.FoldPath(entry.Name())
						if need.folded[key] && (need.exact[entry.Name()] || len(names[key]) < 2) {
							names[key] = append(names[key], entry.Name())
						}
						if need.exact[entry.Name()] {
							remaining--
						}
					}
					if readErr != nil && !errors.Is(readErr, io.EOF) {
						_ = f.Close()
						return nil, fmt.Errorf("inspect fix directory %q: %w", actual, readErr)
					}
					if remaining == 0 || errors.Is(readErr, io.EOF) {
						break
					}
				}
				if err := f.Close(); err != nil {
					return nil, fmt.Errorf("inspect fix directory %q: %w", actual, err)
				}
				directories[actual] = names
			}
			matches := names[workspace.FoldPath(part)]
			if len(matches) == 0 {
				found = false
				break
			}
			spelling := ""
			for _, match := range matches {
				if match == part {
					spelling = part
					break
				}
			}
			if spelling == "" {
				if len(matches) != 1 {
					return nil, fmt.Errorf("ambiguous case-folded fix path %q", name)
				}
				spelling = matches[0]
			}
			actual = path.Join(actual, spelling)
		}
		if found && actual != name {
			stale = append(stale, name...)
			stale = append(stale, 0)
		}
	}
	return stale, nil
}

// Check control-file shapes before Git can open FIFOs while discovering output.
// Ignored directories are skipped unless they contain supplied tracked files.
// shortcut: live writers can race these checks; isolate and quiesce executors in #96.
func (index *OfflineIndex) checkControlFiles(ctx context.Context) error {
	return index.checkControlFilesWithLimit(ctx, workspace.MaxEntries)
}

func (index *OfflineIndex) checkControlFilesWithLimit(ctx context.Context, limit int) error {
	trackedDirs := map[string]bool{}
	trackedChildren := map[string][]string{}
	for name := range index.tracked {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if trackedDirs[parent] {
				break
			}
			trackedChildren[path.Dir(parent)] = append(trackedChildren[path.Dir(parent)], parent)
			trackedDirs[parent] = true
		}
	}
	type directory struct {
		parent, name string
		ignored      bool
	}
	dirs, entries := []directory{{name: "."}}, 0
	changedControlBytes := int64(0)
	for len(dirs) != 0 {
		var next []directory
		var candidates []directory
		var input []byte
		controls := map[string]bool{}
		flush := func() error {
			if len(input) == 0 {
				return nil
			}
			// No trailing slash: Git must not open a child's unchecked .gitignore.
			ignored, err := index.run(ctx, len(input), input, "check-ignore", "--no-index", "-z", "--stdin")
			if err != nil {
				return err
			}
			names, err := splitNUL(ignored)
			if err != nil {
				return fmt.Errorf("invalid offline git ignore output: %w", err)
			}
			skip := map[string]bool{}
			for _, name := range names {
				candidate := strings.TrimPrefix(string(name), ":(top)")
				if controls[candidate] {
					return fmt.Errorf("ignored untracked fix control file %q must be removed or tracked", candidate)
				}
				skip[candidate] = true
			}
			for _, candidate := range candidates {
				name := path.Join(candidate.parent, candidate.name)
				if !skip[name] || trackedDirs[name] {
					candidate.ignored = skip[name]
					next = append(next, candidate)
				}
			}
			candidates, input = nil, nil
			clear(controls)
			return nil
		}
		enqueue := func(name string) error {
			input = append(input, ":(top)"...)
			input = append(input, name...)
			input = append(input, 0)
			if len(candidates)+len(controls) == 128 {
				return flush()
			}
			return nil
		}
		for _, current := range dirs {
			if err := ctx.Err(); err != nil {
				return err
			}
			dir := path.Join(current.parent, current.name)
			if len(dir) >= workspace.MaxPathBytes {
				return fmt.Errorf("fix directory %q exceeds path limit", dir)
			}
			absolute := filepath.Join(index.source, filepath.FromSlash(dir))
			for _, control := range []string{".gitignore", ".gitattributes"} {
				name := path.Join(dir, control)
				filename := filepath.Join(absolute, control)
				info, err := os.Lstat(filename)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return fmt.Errorf("fix control file %q: %w", name, err)
				}
				if !info.Mode().IsRegular() || info.Size() > workspace.MaxBytes {
					return fmt.Errorf("fix control file %q must be regular and bounded to %d bytes", name, workspace.MaxBytes)
				}
				unchanged := false
				if baseline, tracked := index.controls[name]; tracked && info.Size() == index.tracked[name] {
					data, err := workspace.ReadRegular(filename, workspace.MaxBytes)
					if err != nil {
						return fmt.Errorf("fix control file %q: %w", name, err)
					}
					unchanged = sha256.Sum256(data) == baseline
				}
				if !unchanged {
					if info.Size() > int64(workspace.MaxChangeBytes)-changedControlBytes {
						return fmt.Errorf("fix control file %q exceeds changed byte limit", name)
					}
					changedControlBytes += info.Size()
				}
				if _, tracked := index.tracked[name]; control == ".gitignore" && !tracked && !current.ignored {
					controls[name] = true
					if err := enqueue(name); err != nil {
						return err
					}
				}
			}
			if current.ignored {
				// Git cannot export untracked descendants of an excluded directory.
				for _, name := range trackedChildren[dir] {
					info, err := os.Lstat(filepath.Join(index.source, filepath.FromSlash(name)))
					if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
						continue
					}
					if err != nil {
						return fmt.Errorf("tracked fix directory %q: %w", name, err)
					}
					if info.IsDir() {
						next = append(next, directory{parent: dir, name: path.Base(name), ignored: true})
					}
				}
				continue
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
					for _, entry := range batch {
						entries++
						if entries > limit {
							return errors.New("fix directory scan exceeds entry limit")
						}
						if entry.Name() == ".git" && dir != "." && !trackedDirs[dir] {
							return fmt.Errorf("untracked nested repository output %q must be removed or ignored", dir+"/")
						}
						if !entry.IsDir() || strings.EqualFold(entry.Name(), ".git") {
							continue
						}
						name := path.Join(dir, entry.Name())
						candidates = append(candidates, directory{parent: dir, name: entry.Name()})
						if err := enqueue(name); err != nil {
							return err
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
		if err := flush(); err != nil {
			return err
		}
		dirs = next
	}
	return nil
}

func parseIndexChanges(raw []byte) ([]workspace.Change, []string, error) {
	parts, err := splitNUL(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid Git index diff: %w", err)
	}
	if len(parts)%2 != 0 {
		return nil, nil, errors.New("invalid Git index diff")
	}
	if len(parts)/2 > workspace.MaxChanges {
		return nil, nil, fmt.Errorf("%w: fix result exceeds changed file limit", ErrTooManyFiles)
	}
	changes, objects := make([]workspace.Change, 0, len(parts)/2), make([]string, 0, len(parts)/2)
	for i := 0; i < len(parts); i += 2 {
		fields := strings.Fields(string(parts[i]))
		if len(fields) != 5 || len(fields[0]) != 7 || fields[0][0] != ':' || len(fields[1]) != 6 || !workspace.ValidSHA(fields[2]) || !workspace.ValidSHA(fields[3]) {
			return nil, nil, errors.New("invalid Git index change record")
		}
		mode, err := strconv.ParseUint(fields[1], 8, 32)
		if err != nil {
			return nil, nil, errors.New("invalid Git index file mode")
		}
		change := workspace.Change{Path: string(parts[i+1]), Mode: uint32(mode)}
		switch fields[4] {
		case "D":
			change.Delete, change.Mode = true, 0
		case "A", "M":
		default:
			return nil, nil, fmt.Errorf("unsupported Git index change at %q", change.Path)
		}
		changes, objects = append(changes, change), append(objects, fields[3])
	}
	return changes, objects, nil
}

func gitBlobID(data []byte) string {
	// #nosec G401 -- this reproduces the private index's SHA-1 object identifier.
	hash := sha1.New()
	_, _ = fmt.Fprintf(hash, "blob %d\x00", len(data))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func (index *OfflineIndex) run(ctx context.Context, limit int, input []byte, args ...string) ([]byte, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	gitArgs := []string{"--git-dir=" + index.dir, "--work-tree=" + index.source,
		"-c", "credential.helper=", "-c", "protocol.allow=never"}
	env := append([]string{"PATH=" + aicli.SafePath(os.Getenv("PATH")), "HOME=" + index.dir, "TMPDIR=" + index.dir}, hardeningEnv()...)
	dir := index.dir
	// A metadata directory inside source would scope path commands to .git/.
	if workspace.Within(index.source, dir) {
		dir = index.source
	}
	cmd := gitCommand(runCtx, index.binary, dir, env, append(gitArgs, args...)...)
	output, diagnostic := bounded.Buffer{Limit: limit, Cancel: cancel}, bounded.Buffer{Limit: bounded.DiagnosticBytes}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &output, &diagnostic, bytes.NewReader(input)
	err := executil.Run(cmd)
	contextErr := ctx.Err()
	if output.Exceeded && contextErr == nil && !errors.Is(err, exec.ErrWaitDelay) {
		return nil, fmt.Errorf("offline git %s output exceeds action limit: %w", args[0], ErrCheckoutTooLarge)
	}
	if err != nil || contextErr != nil {
		var exit *exec.ExitError
		if args[0] == "check-ignore" && errors.As(err, &exit) && exit.ExitCode() == 1 && contextErr == nil {
			return output.Bytes(), nil
		}
		return nil, gitFailure("offline git", args[0], diagnostic.String(), contextErr, err)
	}
	return output.Bytes(), nil
}
