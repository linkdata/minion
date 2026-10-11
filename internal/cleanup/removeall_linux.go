//go:build linux

package cleanup

import (
	"errors"
	"os"
	"syscall"
)

// RemoveAll restores owner access to directories before removing a completed job tree.
func RemoveAll(path string) error {
	entry, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if entry.IsDir() {
		if err := os.Chmod(path, 0700); err == nil {
			if root, err := os.Open(path); err == nil {
				restoreDirs(root)
				_ = root.Close()
			}
		}
	}
	return os.RemoveAll(path)
}

// Use directory descriptors so nested paths can exceed PATH_MAX. Do not follow
// symlinks into directories outside the job tree.
func restoreDirs(dir *os.File) {
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return
	}
	for _, entry := range entries {
		fd, err := syscall.Openat(int(dir.Fd()), entry.Name(), syscall.O_PATH|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		var stat syscall.Stat_t
		statErr := syscall.Fstat(fd, &stat)
		_ = syscall.Close(fd)
		if statErr != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			continue
		}
		if err := syscall.Fchmodat(int(dir.Fd()), entry.Name(), 0700, 0); err != nil {
			continue
		}
		fd, err = syscall.Openat(int(dir.Fd()), entry.Name(), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		child := os.NewFile(uintptr(fd), entry.Name())
		restoreDirs(child)
		_ = child.Close()
	}
}
