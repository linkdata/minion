package aicli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Directory resolves an existing, non-root directory to its canonical absolute path.
func Directory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("directory is empty")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		resolved, err = filepath.Abs(resolved)
	}
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
