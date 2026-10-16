// Package workspace transports bounded source files without Git metadata.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	MaxFiles               = 10_000
	MaxBytes               = 64 << 20
	MaxChanges             = 100
	MaxChangeBytes         = 1 << 20
	MaxContextBytes        = 1 << 20
	MaxEntries             = 100_000
	MaxPathBytes           = 4096
	GitAttributes          = "* -text -ident -filter -working-tree-encoding !diff\n"
	Regular         uint32 = 0100644
	Executable      uint32 = 0100755
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
	Head             string   `json:"head"`
	Base             string   `json:"base,omitempty"`
	MergeBase        string   `json:"merge_base,omitempty"`
	Files            []File   `json:"files"`
	BaseFiles        []File   `json:"base_files,omitempty"`
	Diff             []byte   `json:"diff,omitempty"`
	DiffIncomplete   bool     `json:"diff_incomplete"`
	ChangedPaths     []string `json:"changed_paths,omitempty"`
	History          []byte   `json:"history,omitempty"`
	HistoryRequested bool     `json:"history_requested"`
	HistoryComplete  bool     `json:"history_complete"`
	Omitted          []string `json:"omitted,omitempty"`
}

func ValidPath(name string) bool {
	return ValidSourcePath(name) && !strings.Contains(name, "\\") && filepath.IsLocal(name)
}

// ValidSourcePath accepts Git's case-sensitive slash paths, including literal backslashes.
func ValidSourcePath(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > MaxPathBytes || !utf8.ValidString(name) || strings.ContainsRune(name, '\x00') || path.Clean(name) != name || path.IsAbs(name) || strings.HasPrefix(name, "../") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if len(part) > 255 || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

func ValidSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func ValidateSnapshot(s Snapshot) error {
	if !ValidSHA(s.Head) || (s.Base != "" && !ValidSHA(s.Base)) || (s.MergeBase != "" && !ValidSHA(s.MergeBase)) || (s.Base == "") != (s.MergeBase == "") {
		return errors.New("snapshot has invalid source commits")
	}
	if (s.Base == "" && (len(s.Diff) != 0 || len(s.BaseFiles) != 0 || s.DiffIncomplete || len(s.ChangedPaths) != 0)) || (s.HistoryComplete && len(s.History) == 0) {
		return errors.New("snapshot context lacks source identity or history coverage")
	}
	if !s.HistoryRequested && (len(s.History) != 0 || s.HistoryComplete) {
		return errors.New("snapshot supplied unrequested history")
	}
	if len(s.Diff) > MaxContextBytes || len(s.History) > MaxContextBytes || len(s.Omitted) > MaxFiles {
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
	seen := map[string]bool{}
	for _, name := range s.Omitted {
		if !ValidSourcePath(name) || seen[name] {
			return fmt.Errorf("invalid omitted source path %q", name)
		}
		seen[name] = true
	}
	if s.Base == "" && len(s.Omitted) != 0 {
		paths := make([]string, 0, len(s.Files)+len(s.Omitted))
		for _, file := range s.Files {
			paths = append(paths, file.Path)
		}
		paths = append(paths, s.Omitted...)
		if err := validateSourcePaths(paths); err != nil {
			return err
		}
	}
	if len(s.ChangedPaths) > MaxFiles || (s.DiffIncomplete && len(s.Diff) != 0) {
		return errors.New("snapshot changed paths or incomplete diff is invalid")
	}
	seen = map[string]bool{}
	for _, name := range s.ChangedPaths {
		if !ValidSourcePath(name) || seen[name] {
			return errors.New("snapshot changed path is unsafe or duplicated")
		}
		seen[name] = true
	}
	return nil
}

func validateFiles(files []File) error {
	if len(files) > MaxFiles {
		return errors.New("source exceeds file limit")
	}
	paths := make([]string, len(files))
	total := 0
	for i, file := range files {
		if !ValidSourcePath(file.Path) || (file.Mode != Regular && file.Mode != Executable) {
			return fmt.Errorf("invalid source file %q", file.Path)
		}
		paths[i] = file.Path
		if len(file.Data) > MaxBytes-total {
			return errors.New("source exceeds byte limit")
		}
		total += len(file.Data)
	}
	return validateSourcePaths(paths)
}

func validateSourcePaths(paths []string) error {
	seen := make(map[string]bool, len(paths))
	for _, name := range paths {
		if seen[name] {
			return fmt.Errorf("duplicate source path %q", name)
		}
		seen[name] = true
	}
	for _, name := range paths {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return fmt.Errorf("source file/directory collision at %q", name)
			}
		}
	}
	return nil
}

func validatePaths(paths []string) error {
	seen := make(map[string]bool, len(paths))
	directories := make(map[string]string)
	for _, name := range paths {
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("duplicate source path %q", name)
		}
		seen[key] = true
	}
	for _, name := range paths {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			key := strings.ToLower(parent)
			if seen[key] {
				return fmt.Errorf("source file/directory collision at %q", name)
			}
			if spelling, exists := directories[key]; exists && spelling != parent {
				return fmt.Errorf("source directory spelling collision at %q", name)
			}
			directories[key] = parent
		}
	}
	return nil
}

func checkRoot(root string, private bool) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (private && info.Mode().Perm()&0077 != 0) {
		return fmt.Errorf("workspace root must be a private directory: %s (%s)", root, info.Mode())
	}
	return nil
}

func Materialize(root string, files []File) error {
	if err := validateFiles(files); err != nil {
		return err
	}
	if err := checkRoot(root, true); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(1)
	err = errors.Join(readErr, dir.Close())
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) != 0 {
		return errors.New("workspace root is not empty")
	}
	directories := map[string]bool{".": true}
	for _, file := range files {
		name := filepath.Join(root, filepath.FromSlash(file.Path))
		parent := "."
		for _, part := range strings.Split(path.Dir(file.Path), "/") {
			parent = path.Join(parent, part)
			if !directories[parent] {
				if err := os.Mkdir(filepath.Join(root, filepath.FromSlash(parent)), 0700); err != nil {
					return err
				}
				directories[parent] = true
			}
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(file.Mode&0777))
		if err != nil {
			return err
		}
		_, writeErr := f.Write(file.Data)
		if err := errors.Join(writeErr, f.Close()); err != nil {
			return err
		}
		if err := os.Chmod(name, fs.FileMode(file.Mode&0777)); err != nil {
			return err
		}
	}
	return nil
}

// Changes compares regular files only; generated Git metadata or links are refused.
func Changes(ctx context.Context, root string, base []File) ([]Change, error) {
	if err := validateFiles(base); err != nil {
		return nil, err
	}
	if err := checkRoot(root, false); err != nil {
		return nil, err
	}
	prior := make(map[string]File, len(base))
	for _, file := range base {
		prior[file.Path] = file
	}
	var changes []Change
	var paths []string
	total := 0
	err := walk(ctx, root, func(name string, entry fs.DirEntry) error {
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !ValidSourcePath(rel) {
			return fmt.Errorf("unsafe workspace path %q", rel)
		}
		if entry.IsDir() {
			return nil
		}
		paths = append(paths, rel)
		if len(paths) > MaxFiles {
			return errors.New("workspace exceeds file limit")
		}
		data, info, err := readFile(name, int64(MaxBytes-total))
		if err != nil {
			return err
		}
		total += len(data)
		mode := Regular
		if info.Mode().Perm()&0111 != 0 {
			mode = Executable
		}
		old, exists := prior[rel]
		delete(prior, rel)
		if !exists || old.Mode != mode || !bytes.Equal(old.Data, data) {
			changes = append(changes, Change{Path: rel, Mode: mode, Data: data})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := validateSourcePaths(paths); err != nil {
		return nil, err
	}
	for name := range prior {
		changes = append(changes, Change{Path: name, Delete: true})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, ValidateChanges(changes)
}

// ReadChanges reads only selected paths; ignored build output is never traversed.
// Selection carries paths and deletions, with optional expected regular-file modes.
func ReadChanges(ctx context.Context, root string, selected []Change) ([]Change, error) {
	checked := append([]Change(nil), selected...)
	for i := range checked {
		if len(checked[i].Data) != 0 {
			return nil, errors.New("change selection contains file data")
		}
		if !checked[i].Delete && checked[i].Mode == 0 {
			checked[i].Mode = Regular
		}
	}
	if err := ValidateChanges(checked); err != nil {
		return nil, err
	}
	dir, err := openDirectory(root)
	if err != nil {
		return nil, err
	}
	if err := dir.Close(); err != nil {
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
			return nil, err
		}
		mode := Regular
		if info.Mode().Perm()&0111 != 0 {
			mode = Executable
		}
		if selection.Mode != 0 && selection.Mode != mode {
			return nil, errors.New("selected change mode differs from file")
		}
		changes = append(changes, Change{Path: selection.Path, Mode: mode, Data: data})
		total += len(data)
	}
	return changes, ValidateChanges(changes)
}

// walk visits bounded entries without following directory symlinks.
func walk(ctx context.Context, root string, visit func(string, fs.DirEntry) error) error {
	root = filepath.Clean(root)
	dirs := []string{root}
	entries := 0
	for len(dirs) > 0 {
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		err := func() (resultErr error) {
			f, err := openDirectory(dir)
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
	seen := map[string][]Change{}
	var paths []string
	total := 0
	for _, change := range changes {
		key := strings.ToLower(change.Path)
		prior := seen[key]
		if !ValidPath(change.Path) || len(prior) > 1 || (len(prior) == 1 && (prior[0].Path == change.Path || prior[0].Delete == change.Delete)) {
			return fmt.Errorf("unsafe or duplicate changed path %q", change.Path)
		}
		seen[key] = append(prior, change)
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
	return validatePaths(paths)
}

// ValidateChangedPaths checks the final layout of an already validated base and changes.
func ValidateChangedPaths(base []File, changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	files := make(map[string]bool, len(base)+len(changes))
	for _, file := range base {
		files[file.Path] = true
	}
	for _, change := range changes {
		if change.Delete {
			if !files[change.Path] {
				return fmt.Errorf("deleted path %q does not match source", change.Path)
			}
			delete(files, change.Path)
		} else {
			files[change.Path] = true
		}
	}
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	return validatePaths(paths)
}

func safeParents(root, name string) error {
	for parent := filepath.Dir(name); parent != root; parent = filepath.Dir(parent) {
		if parent == filepath.Dir(parent) {
			return errors.New("changed path escapes workspace")
		}
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

func Apply(root string, changes []Change) error {
	root = filepath.Clean(root)
	if err := ValidateChanges(changes); err != nil {
		return err
	}
	if err := checkRoot(root, false); err != nil {
		return err
	}
	// Delete first, including empty ancestors, so file/directory changes work.
	ordered := append([]Change(nil), changes...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i].Path) > len(ordered[j].Path) })
	for _, change := range ordered {
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
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(name, change.Data, fs.FileMode(change.Mode&0777)); err != nil {
			return err
		}
		if err := os.Chmod(name, fs.FileMode(change.Mode&0777)); err != nil {
			return err
		}
	}
	return nil
}

// ReadRegular opens one bounded regular file without following any path symlinks.
func ReadRegular(name string, limit int64) ([]byte, error) {
	data, _, err := readFile(name, limit)
	return data, err
}

func readFile(name string, limit int64) (data []byte, info fs.FileInfo, resultErr error) {
	if limit < 0 || limit > MaxBytes {
		return nil, nil, errors.New("invalid regular file byte limit")
	}
	f, err := openRegular(name)
	if err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	opened, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() || opened.Size() < 0 || opened.Size() > limit {
		return nil, nil, errors.New("file is not regular or exceeds byte limit")
	}
	data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit || int64(len(data)) != opened.Size() || after.Size() != opened.Size() || after.Mode() != opened.Mode() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, nil, errors.New("file changed or exceeded limit while reading")
	}
	return data, opened, nil
}
