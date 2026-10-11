package cleanup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveAll restores owner access to directories before removing a completed job tree.
func RemoveAll(path string) error {
	err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(path, 0700)
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.RemoveAll(path)
}
