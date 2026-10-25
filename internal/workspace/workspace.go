// Package workspace provides bounded source transfer, path and identity validation, and private record helpers.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"
)

const (
	MaxFiles        = 10_000
	MaxBytes        = 64 << 20
	MaxChanges      = 100
	MaxChangeBytes  = 1 << 20
	MaxContextBytes = 1 << 20
	MaxEntries      = 100_000
	// MaxPathBytes is the exclusive relative transfer path limit; absolute paths also obey host limits.
	MaxPathBytes        = 4096
	Regular      uint32 = 0100644
	Executable   uint32 = 0100755
)

type File struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Data []byte `json:"data"`
}

type Change struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Delete bool   `json:"delete,omitempty"`
}

type Snapshot struct {
	Head           string   `json:"head"`
	Base           string   `json:"base,omitempty"`
	MergeBase      string   `json:"merge_base,omitempty"`
	Files          []File   `json:"files"`
	BaseFiles      []File   `json:"base_files,omitempty"`
	Diff           []byte   `json:"diff,omitempty"`
	DiffIncomplete bool     `json:"diff_incomplete"`
	ChangedPaths   []string `json:"changed_paths,omitempty"`
	Repository     []byte   `json:"repository"`
	Omitted        []string `json:"omitted,omitempty"`
	HeadOmitted    []string `json:"head_omitted,omitempty"`
}

func ValidPath(name string) bool {
	return !strings.Contains(name, "\\") && ValidCheckoutPath(name)
}

// ValidCheckoutPath applies Git's protected metadata names before materialization.
// Git's NTFS checks treat backslashes as separators; its HFS checks do not.
func ValidCheckoutPath(name string) bool {
	if !validSourcePath(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if hfsGitMetadataAlias(part) {
			return false
		}
		if slices.ContainsFunc(strings.Split(part, "\\"), ntfsGitMetadataAlias) {
			return false
		}
	}
	return true
}

func ntfsGitMetadataAlias(part string) bool {
	name, _, _ := strings.Cut(part, ":")
	name = strings.TrimRight(name, " .")
	return strings.EqualFold(name, ".git") || strings.EqualFold(name, "git~1")
}

// HFS metadata checks use slash components even when a name contains backslashes.
func hfsGitMetadataAlias(part string) bool {
	hfs := strings.Map(func(r rune) rune {
		switch {
		case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e,
			r >= 0x206a && r <= 0x206f, r == 0xfeff:
			return -1
		}
		return r
	}, part)
	return strings.EqualFold(hfs, ".git")
}

// FoldPath returns the same key for Unicode simple-case-fold equivalents.
// It does not normalize Unicode spellings.
func FoldPath(name string) string {
	return strings.Map(func(r rune) rune {
		key := r
		for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
			if folded < key {
				key = folded
			}
		}
		return key
	}, name)
}

// validSourcePath accepts Git's slash paths; only Unix can retain literal backslashes.
// Writable/import paths additionally reject HFS/NTFS aliases of Git metadata.
func validSourcePath(name string) bool {
	if !fs.ValidPath(name) || name == "." || len(name) >= MaxPathBytes || strings.ContainsRune(name, '\x00') || !validPlatformPath(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if len(part) > 255 || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

// ValidSHA reports whether value is a lowercase SHA-1 object ID.
func ValidSHA(value string) bool {
	return len(value) == 40 && strings.Trim(value, "0123456789abcdef") == ""
}

func ValidateSnapshot(s Snapshot) error {
	if !ValidSHA(s.Head) || (s.Base != "" && !ValidSHA(s.Base)) || (s.MergeBase != "" && !ValidSHA(s.MergeBase)) || (s.Base == "") != (s.MergeBase == "") {
		return errors.New("snapshot has invalid source commits")
	}
	if s.Base == "" && (len(s.Diff) != 0 || len(s.BaseFiles) != 0 || s.DiffIncomplete || len(s.ChangedPaths) != 0) {
		return errors.New("snapshot context lacks source identity")
	}
	if len(s.Diff) > MaxContextBytes || len(s.Repository) > MaxBytes || len(s.Omitted) > MaxFiles || len(s.HeadOmitted) > MaxFiles {
		return errors.New("snapshot context exceeds limits")
	}
	total := 0
	for _, files := range [][]File{s.Files, s.BaseFiles} {
		if err := validateFiles(files); err != nil {
			return err
		}
		for _, file := range files {
			total += len(file.Data)
		}
	}
	if total > MaxBytes {
		return errors.New("snapshot files exceed byte limit")
	}
	if name, invalid := invalidSourcePath(s.Omitted); invalid {
		return fmt.Errorf("invalid omitted source path %q", name)
	}
	if len(s.HeadOmitted) != 0 {
		if name, invalid := invalidSourcePath(s.HeadOmitted); invalid {
			return fmt.Errorf("invalid head omitted source path %q", name)
		}
		omitted := make(map[string]bool, len(s.Omitted))
		for _, name := range s.Omitted {
			omitted[name] = true
		}
		paths := make([]string, 0, len(s.Files)+len(s.HeadOmitted))
		for _, file := range s.Files {
			paths = append(paths, file.Path)
		}
		for _, name := range s.HeadOmitted {
			if !omitted[name] {
				return fmt.Errorf("head omitted source path %q is not omitted", name)
			}
			paths = append(paths, name)
		}
		if err := validatePaths(paths, false); err != nil {
			return err
		}
	}
	if s.Base == "" && len(s.Omitted) != 0 {
		paths := make([]string, 0, len(s.Files)+len(s.Omitted))
		for _, file := range s.Files {
			paths = append(paths, file.Path)
		}
		paths = append(paths, s.Omitted...)
		if err := validatePaths(paths, false); err != nil {
			return err
		}
	}
	if len(s.ChangedPaths) > MaxFiles || (s.DiffIncomplete && len(s.Diff) != 0) {
		return errors.New("snapshot changed paths or incomplete diff is invalid")
	}
	if _, invalid := invalidSourcePath(s.ChangedPaths); invalid {
		return errors.New("snapshot changed path is unsafe or duplicated")
	}
	return nil
}

func invalidSourcePath(paths []string) (string, bool) {
	seen := make(map[string]bool, len(paths))
	for _, name := range paths {
		if !validSourcePath(name) || seen[name] {
			return name, true
		}
		seen[name] = true
	}
	return "", false
}

func validateFiles(files []File) error {
	if len(files) > MaxFiles {
		return errors.New("source exceeds file limit")
	}
	paths := make([]string, len(files))
	total := 0
	for i, file := range files {
		if !validSourcePath(file.Path) || (file.Mode != Regular && file.Mode != Executable) {
			return fmt.Errorf("invalid source file %q", file.Path)
		}
		paths[i] = file.Path
		if len(file.Data) > MaxBytes-total {
			return errors.New("source exceeds byte limit")
		}
		total += len(file.Data)
	}
	return validatePaths(paths, false)
}

func validatePaths(paths []string, folded bool) error {
	key := func(name string) string { return name }
	if folded {
		key = FoldPath
	}
	seen := make(map[string]bool, len(paths))
	directories := make(map[string]string)
	for _, name := range paths {
		nameKey := key(name)
		if seen[nameKey] {
			return fmt.Errorf("duplicate source path %q", name)
		}
		seen[nameKey] = true
	}
	for _, name := range paths {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			parentKey := key(parent)
			if seen[parentKey] {
				return fmt.Errorf("source file/directory collision at %q", name)
			}
			if spelling, exists := directories[parentKey]; exists {
				if spelling != parent {
					return fmt.Errorf("source directory spelling collision at %q", name)
				}
				break
			}
			directories[parentKey] = parent
			if len(seen)+len(directories) > MaxEntries {
				return errors.New("source layout exceeds entry limit")
			}
		}
	}
	return nil
}

// ValidateFixPaths checks a validated snapshot's stricter writable layout, including omissions.
func ValidateFixPaths(files []File, omitted []string) error {
	paths := make([]string, len(files), len(files)+len(omitted))
	for i, file := range files {
		if !ValidPath(file.Path) {
			return fmt.Errorf("unsafe fix source path %q", file.Path)
		}
		paths[i] = file.Path
	}
	paths = append(paths, omitted...)
	return validatePaths(paths, true)
}

// CanonicalPath resolves an existing path to its canonical absolute path.
func CanonicalPath(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(root) {
		if runtime.GOOS == "windows" && filepath.VolumeName(root) != "" {
			return "", fmt.Errorf("resolve path: drive-relative path %q is unsupported", root)
		}
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve path: %w", err)
		}
		// Preserve ".." until canonicalPath resolves each component in order.
		if runtime.GOOS == "windows" && os.IsPathSeparator(root[0]) {
			root = filepath.VolumeName(cwd) + root
		} else {
			root = cwd + string(os.PathSeparator) + root
		}
	}
	root, err := canonicalPath(root)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	return root, nil
}

// Within compares canonical absolute paths; callers resolve symlinks first.
// On Windows, existing canonical paths preserve case-sensitive directory distinctions.
// On macOS, existing ancestor identity also catches case-insensitive aliases and missing suffixes.
func Within(root, name string) bool {
	if runtime.GOOS == "windows" {
		rootVolume, nameVolume := filepath.VolumeName(root), filepath.VolumeName(name)
		if rootVolume == "" || !strings.EqualFold(rootVolume, nameVolume) || !filepath.IsAbs(root) || !filepath.IsAbs(name) {
			return false
		}
		rootPath := strings.TrimRight(filepath.FromSlash(root[len(rootVolume):]), `\`)
		namePath := filepath.FromSlash(name[len(nameVolume):])
		if rootPath == "" {
			return namePath == "" || strings.HasPrefix(namePath, `\`)
		}
		if namePath == rootPath || strings.HasPrefix(namePath, rootPath+`\`) {
			return true
		}
		if !equalFoldAncestor(rootPath, namePath) {
			return false
		}
		_, err := os.Stat(root)
		return err != nil // Existing canonical roots keep exact case; missing or unverifiable roots fail closed.
	}
	rel, err := filepath.Rel(root, name)
	if err == nil && filepath.IsLocal(rel) {
		return true
	}
	if runtime.GOOS != "darwin" {
		return false
	}
	rootInfo, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return equalFoldAncestor(root, name)
	}
	if err != nil {
		return true // An unverified grant cannot be treated as separate.
	}
	for {
		info, err := os.Stat(name)
		if err == nil && os.SameFile(rootInfo, info) {
			return true
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return true
		}
		parent := filepath.Dir(name)
		if parent == name {
			return false
		}
		name = parent
	}
}

// shortcut: absent-root case folding may reject distinct future paths on a case-sensitive volume; revisit if needed.
func equalFoldAncestor(root, name string) bool {
	for {
		if strings.EqualFold(root, name) {
			return true
		}
		parent := filepath.Dir(name)
		if parent == name {
			return false
		}
		name = parent
	}
}

// OpenDirectory anchors collection without following any symlink component.
func OpenDirectory(root string) (*os.File, error) {
	return openPath(root, true)
}

// CheckRoot verifies that safe, anchored collection is available before a model runs.
func CheckRoot(root string) error {
	dir, err := OpenDirectory(root)
	if err != nil {
		return err
	}
	return dir.Close()
}

// Materialize requires a root and parents not concurrently writable by untrusted processes.
func Materialize(root string, files []File) error {
	if err := validateFiles(files); err != nil {
		return err
	}
	return materialize(root, files)
}

// MaterializeChanges creates a complete candidate with exclusive file creation.
// Source and change bounds apply separately, so a candidate can grow by the change limit.
// It requires the same trusted root and parents as Materialize.
func MaterializeChanges(root string, base []File, changes []Change) error {
	files, err := changedFiles(base, changes)
	if err != nil {
		return err
	}
	return materialize(root, files)
}

func changedFiles(base []File, changes []Change) ([]File, error) {
	if err := validateFiles(base); err != nil {
		return nil, err
	}
	if err := ValidateChanges(changes); err != nil {
		return nil, err
	}
	paths := make([]string, len(base))
	files := make(map[string]File, len(base)+len(changes))
	for i, file := range base {
		if !ValidPath(file.Path) {
			return nil, fmt.Errorf("unsafe fix source path %q", file.Path)
		}
		paths[i] = file.Path
		files[file.Path] = file
	}
	paths, err := changedPaths(paths, changes)
	if err != nil {
		return nil, err
	}
	for _, change := range changes {
		if !change.Delete {
			files[change.Path] = File{Path: change.Path, Mode: change.Mode, Data: change.Data}
		}
	}
	final := make([]File, len(paths))
	for i, name := range paths {
		final[i] = files[name]
	}
	return final, nil
}

func materialize(root string, files []File) error {
	if err := CheckRoot(root); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(1)
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if err := errors.Join(readErr, dir.Close()); err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("workspace root is not empty")
	}
	// Creating each spelling once refuses aliases on casefolding filesystems.
	directories := map[string]bool{}
	for _, file := range files {
		name := filepath.Join(root, filepath.FromSlash(file.Path))
		for i, char := range file.Path {
			if char != '/' || directories[file.Path[:i]] {
				continue
			}
			if err := os.Mkdir(filepath.Join(root, filepath.FromSlash(file.Path[:i])), 0700); err != nil {
				return err
			}
			directories[file.Path[:i]] = true
		}
		mode := fs.FileMode(file.Mode & 0777)
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		if err := writeFile(f, file.Data, mode); err != nil {
			return err
		}
	}
	return nil
}

// CheckUnchanged verifies exact materialized source bytes and permissions without following links.
func CheckUnchanged(ctx context.Context, root string, base []File) error {
	if err := validateFiles(base); err != nil {
		return err
	}
	return checkUnchanged(ctx, root, base, false)
}

// CheckChangesUnchanged verifies a candidate with separate source/change bounds,
// excluding only the top-level .git directory, verified separately by its owner.
func CheckChangesUnchanged(ctx context.Context, root string, base []File, changes []Change) error {
	files, err := changedFiles(base, changes)
	if err != nil {
		return err
	}
	return checkUnchanged(ctx, root, files, true)
}

func checkUnchanged(ctx context.Context, root string, base []File, repository bool) error {
	prior := make(map[string]File, len(base))
	for _, file := range base {
		prior[file.Path] = file
	}
	err := walk(ctx, root, repository, func(name string, entry fs.DirEntry) error {
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !validSourcePath(rel) {
			return fmt.Errorf("unsafe workspace path %q", rel)
		}
		if entry.IsDir() {
			// Empty directories carry no versioned source state; still inspect their entries.
			return nil
		}
		old, exists := prior[rel]
		if !exists {
			return fmt.Errorf("workspace changed: added %q", rel)
		}
		data, info, err := readFile(name, int64(len(old.Data)))
		if err != nil {
			return fmt.Errorf("workspace changed: cannot verify %q: %w", rel, err)
		}
		delete(prior, rel)
		if !sourceModeMatches(info, old.Mode) || !bytes.Equal(old.Data, data) {
			return fmt.Errorf("workspace changed: changed %q", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(prior) != 0 {
		return fmt.Errorf("workspace changed: deleted %q", slices.Min(slices.Collect(maps.Keys(prior))))
	}
	return nil
}

// ReadChanges reads only selected paths; ignored build output is never traversed.
// Selection carries paths and deletions, with required expected regular-file modes.
func ReadChanges(ctx context.Context, root string, selected []Change) ([]Change, error) {
	if err := ValidateChanges(selected); err != nil {
		return nil, err
	}
	if err := CheckRoot(root); err != nil {
		return nil, err
	}
	changes := make([]Change, 0, len(selected))
	total := 0
	for _, selection := range selected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if selection.Delete {
			changes = append(changes, selection)
			continue
		}
		name := filepath.Join(root, filepath.FromSlash(selection.Path))
		data, info, err := readFile(name, int64(MaxChangeBytes-total))
		if err != nil {
			return nil, fmt.Errorf("changed path %q: %w", selection.Path, err)
		}
		mode := changedSourceMode(info, selection.Mode)
		if selection.Mode != mode {
			return nil, fmt.Errorf("selected change mode differs from file at %q", selection.Path)
		}
		changes = append(changes, Change{Path: selection.Path, Mode: mode, Data: data})
		total += len(data)
	}
	return changes, nil
}

// walk visits bounded source entries without following directory symlinks.
// Repository mode excludes only the separately verified top-level .git directory.
func walk(ctx context.Context, root string, repository bool, visit func(string, fs.DirEntry) error) error {
	root = filepath.Clean(root)
	dirs := []string{root}
	entries := 0
	for len(dirs) > 0 {
		last := len(dirs) - 1
		dir := dirs[last]
		dirs[last] = ""
		dirs = dirs[:last]
		err := func() (resultErr error) {
			f, err := openPath(dir, true)
			if err != nil {
				return err
			}
			defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				batch, readErr := f.ReadDir(128)
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					return readErr
				}
				for _, entry := range batch {
					if repository && dir == root && entry.Name() == ".git" && entry.IsDir() {
						continue
					}
					entries++
					if entries > MaxEntries {
						return errors.New("workspace scan exceeds entry limit")
					}
					name := filepath.Join(dir, entry.Name())
					if err := visit(name, entry); err != nil {
						return err
					}
					if entry.IsDir() {
						dirs = append(dirs, name)
					}
				}
				if errors.Is(readErr, io.EOF) {
					return nil
				}
			}
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

func ValidateChanges(changes []Change) error {
	if len(changes) > MaxChanges {
		return errors.New("result exceeds changed file limit")
	}
	type identity struct {
		path    string
		deleted bool
		count   uint8
	}
	seen := make(map[string]identity, len(changes))
	var paths []string
	total := 0
	for _, change := range changes {
		key := FoldPath(change.Path)
		prior := seen[key]
		if !ValidPath(change.Path) || prior.count > 1 || (prior.count == 1 && (prior.path == change.Path || prior.deleted == change.Delete)) {
			return fmt.Errorf("unsafe or duplicate changed path %q", change.Path)
		}
		seen[key] = identity{path: change.Path, deleted: change.Delete, count: prior.count + 1}
		if change.Delete {
			if change.Mode != 0 || len(change.Data) != 0 {
				return fmt.Errorf("deleted file %q has content or mode", change.Path)
			}
		} else {
			if change.Mode != Regular && change.Mode != Executable {
				return fmt.Errorf("invalid changed mode at %q", change.Path)
			}
			paths = append(paths, change.Path)
			if len(change.Data) > MaxChangeBytes-total {
				return errors.New("result exceeds changed byte limit")
			}
			total += len(change.Data)
		}
	}
	return validatePaths(paths, true)
}

// ValidateChangedPaths checks the final layout of an already validated base and changes.
func ValidateChangedPaths(base []string, changes []Change) error {
	_, err := changedPaths(base, changes)
	return err
}

func changedPaths(base []string, changes []Change) ([]string, error) {
	files := make(map[string]bool, len(base)+len(changes))
	for _, name := range base {
		files[name] = true
	}
	for _, change := range changes {
		if change.Delete {
			if !files[change.Path] {
				return nil, fmt.Errorf("deleted path %q does not match source", change.Path)
			}
			delete(files, change.Path)
		} else {
			files[change.Path] = true
		}
	}
	paths := slices.Sorted(maps.Keys(files))
	return paths, validatePaths(paths, true)
}

func safeParents(root, name string) error {
	for parent := filepath.Dir(name); parent != root; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("changed path parent %q is not a directory", parent)
		}
	}
	return nil
}

// Apply requires a root and parents not concurrently writable by untrusted processes.
func Apply(root string, changes []Change) error {
	root = filepath.Clean(root)
	if err := ValidateChanges(changes); err != nil {
		return err
	}
	if err := CheckRoot(root); err != nil {
		return err
	}
	// Delete first, including empty ancestors, so file/directory changes work.
	for _, change := range changes {
		if !change.Delete {
			continue
		}
		name := filepath.Join(root, filepath.FromSlash(change.Path))
		if err := safeParents(root, name); err != nil {
			return err
		}
		info, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("deleted path %q does not match source", change.Path)
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("deleted path %q is not regular", change.Path)
		}
		if err := os.Remove(name); err != nil {
			return err
		}
		for parent := filepath.Dir(name); parent != root; parent = filepath.Dir(parent) {
			if os.Remove(parent) != nil {
				break
			}
		}
	}
	for _, change := range changes {
		if change.Delete {
			continue
		}
		name := filepath.Join(root, filepath.FromSlash(change.Path))
		if err := safeParents(root, name); err != nil {
			return err
		}
		if info, err := os.Lstat(name); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("changed path %q is not regular", change.Path)
			}
			opened, err := openPath(name, false)
			if err != nil {
				return err
			}
			current, statErr := opened.Stat()
			valid := statErr == nil && os.SameFile(info, current) && singleLink(current, opened)
			if err := errors.Join(statErr, opened.Close()); err != nil {
				return err
			}
			if !valid {
				return fmt.Errorf("changed path %q must have exactly one hard link", change.Path)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		mode := fs.FileMode(change.Mode & 0777)
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if err := writeFile(f, change.Data, mode); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(f *os.File, data []byte, mode fs.FileMode) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	return errors.Join(err, f.Close())
}

// ReadRegular opens one singly linked regular file without following path symlinks.
// limit must be between 0 and MaxBytes, inclusive.
func ReadRegular(name string, limit int64) ([]byte, error) {
	data, _, err := readFile(name, limit)
	return data, err
}

func readFile(name string, limit int64) (data []byte, info fs.FileInfo, resultErr error) {
	if limit < 0 || limit > MaxBytes {
		return nil, nil, errors.New("invalid regular file byte limit")
	}
	f, err := openPath(name, false)
	if err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	opened, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, nil, errors.New("file is not regular")
	}
	if opened.Size() > limit {
		return nil, nil, errors.New("file exceeds byte limit")
	}
	if !singleLink(opened, f) {
		return nil, nil, errors.New("file must have exactly one hard link")
	}
	beforeChange, err := changeTime(opened, f)
	if err != nil {
		return nil, nil, err
	}
	data = make([]byte, opened.Size()+1)
	n, err := io.ReadFull(f, data)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, nil, err
	}
	data = data[:n]
	after, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	afterChange, err := changeTime(after, f)
	if err != nil {
		return nil, nil, err
	}
	if err := checkReadStability(opened, after, len(data), f, beforeChange == afterChange); err != nil {
		return nil, nil, err
	}
	return data, opened, nil
}

func checkReadStability(opened, after fs.FileInfo, bytesRead int, file *os.File, unchangedTime bool) error {
	if !singleLink(after, file) {
		return errors.New("file hard links changed while reading")
	}
	if int64(bytesRead) != opened.Size() || after.Size() != opened.Size() || after.Mode() != opened.Mode() || !after.ModTime().Equal(opened.ModTime()) || !unchangedTime {
		return errors.New("file changed or exceeded limit while reading")
	}
	return nil
}
