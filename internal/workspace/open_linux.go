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
	fd, err := syscall.Open("/", flags|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	for i, part := range parts {
		if part == "" {
			continue
		}
		partFlags := flags
		if i < len(parts)-1 || directory {
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
