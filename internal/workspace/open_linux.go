//go:build linux

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func openRegular(name string) (*os.File, error) {
	return openPath(name, false)
}

func openDirectory(name string) (*os.File, error) {
	return openPath(name, true)
}

// Pin each parent directory before opening its child; a worker may still rename paths.
func openPath(name string, directory bool) (*os.File, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	// syscall omits O_PATH on amd64; Linux uses this value on every supported architecture.
	const oPath = 0x200000
	parentFlags := oPath | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	rootFlags := parentFlags
	if abs == "/" {
		rootFlags = flags | syscall.O_DIRECTORY
	}
	fd, err := syscall.Open("/", rootFlags, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	for i, part := range parts {
		if part == "" {
			continue
		}
		partFlags := flags
		if i < len(parts)-1 {
			partFlags = parentFlags
		} else if directory {
			partFlags |= syscall.O_DIRECTORY
		}
		next, openErr := syscall.Openat(fd, part, partFlags, 0)
		closeErr := syscall.Close(fd)
		if openErr != nil {
			return nil, errors.Join(openErr, closeErr)
		}
		if closeErr != nil {
			return nil, errors.Join(closeErr, syscall.Close(next))
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), name), nil
}
