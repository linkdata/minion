//go:build darwin

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func singleLink(info os.FileInfo, _ *os.File) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func changeTime(info os.FileInfo, _ *os.File) ([2]int64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]int64{}, errors.New("file change time is unavailable")
	}
	return [2]int64{stat.Ctimespec.Sec, stat.Ctimespec.Nsec}, nil
}

// Pin each parent before opening its child, refusing symlinks even within the root.
func openPath(name string, directory bool) (file *os.File, resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = &os.PathError{Op: "openat", Path: name, Err: resultErr}
		}
	}()
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	if abs == "/" {
		return nil, errors.New("filesystem root is not a workspace")
	}
	parentFlags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	finalFlags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC | unix.O_NOCTTY
	var fd int
	for {
		fd, err = unix.Open("/", parentFlags, 0)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	for i, part := range parts {
		flags := finalFlags
		if i < len(parts)-1 {
			flags = parentFlags
		} else if directory {
			flags |= unix.O_DIRECTORY
		}
		var next int
		var openErr error
		for {
			next, openErr = unix.Openat(fd, part, flags, 0)
			if openErr != syscall.EINTR {
				break
			}
		}
		closeErr := unix.Close(fd)
		if openErr != nil {
			return nil, errors.Join(openErr, closeErr)
		}
		if closeErr != nil {
			return nil, errors.Join(closeErr, unix.Close(next))
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), name), nil
}
