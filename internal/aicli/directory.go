package aicli

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/linkdata/minion/internal/workspace"
)

// ResolvePath canonicalizes existing ancestors without creating missing ordinary components.
func ResolvePath(path string) (string, error) {
	var missing []string
	for {
		resolved, err := workspace.PrepareRoot(path)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if _, statErr := os.Lstat(path); statErr == nil {
			return "", err
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent, name := filepath.Split(path)
		if name == "" {
			path = parent[:len(parent)-1]
			continue
		}
		if name == "." || name == ".." {
			return "", err
		}
		missing = append(missing, name)
		path = parent
		if path == "" {
			path = "."
		}
	}
}

// Directory resolves an existing, non-root directory to its canonical absolute path.
func Directory(path string) (string, error) {
	resolved, err := workspace.PrepareRoot(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || resolved == string(os.PathSeparator) {
		return "", errors.New("expected a non-root directory")
	}
	return resolved, nil
}
