//go:build darwin

package cleanup

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// RemoveAll restores owner access to model-locked directories before retrying.
func RemoveAll(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	parent, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		restoreDarwinDir(parent, filepath.Base(path))
		_ = unix.Close(parent)
	}
	return os.RemoveAll(path)
}

func restoreDarwinDir(parent int, name string) {
	var stat unix.Stat_t
	if unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return
	}
	if unix.Fchmodat(parent, name, 0700, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	dir := os.NewFile(uintptr(fd), name)
	defer func() { _ = dir.Close() }()
	for {
		names, err := dir.Readdirnames(1024)
		for _, child := range names {
			restoreDarwinDir(fd, child)
		}
		if err != nil {
			return
		}
	}
}
