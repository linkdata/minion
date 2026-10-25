//go:build !linux && !darwin && !windows

package cleanup

import (
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveAll restores owner access to directories before removing a completed job tree.
func RemoveAll(path string) error {
	_ = filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(path, 0700)
		}
		return nil
	})
	return os.RemoveAll(path)
}
