//go:build linux

package cleanup

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Go 1.23's syscall package does not expose this Linux open flag.
const oPath = 0x200000

// RemoveAll restores owner access if a completed job left locked directories.
func RemoveAll(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	if parent, err := os.Open(filepath.Dir(path)); err == nil {
		restoreDir(int(parent.Fd()), filepath.Base(path))
		_ = parent.Close()
	}
	return os.RemoveAll(path)
}

// Use directory descriptors to reach paths beyond PATH_MAX. The O_PATH fd pins
// the inode while chmod runs, so a replaced entry cannot redirect the chmod.
func restoreDir(parent int, name string) {
	fd, err := syscall.Openat(parent, name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		_ = syscall.Close(fd)
		return
	}
	if err := os.Chmod("/proc/self/fd/"+strconv.Itoa(fd), 0700); err != nil {
		_ = syscall.Close(fd)
		return
	}
	dirFD, err := syscall.Openat(fd, ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	_ = syscall.Close(fd)
	if err != nil {
		return
	}
	dir := os.NewFile(uintptr(dirFD), name)
	defer dir.Close()
	for {
		names, err := dir.Readdirnames(1024)
		for _, name := range names {
			restoreDir(int(dir.Fd()), name)
		}
		if err != nil {
			return
		}
	}
}
