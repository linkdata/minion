package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var replaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func newConfigTemp(target string) (*os.File, error) {
	return os.CreateTemp(filepath.Dir(target), ".config-*")
}

func cleanupConfigTemp(name string) error {
	if err := os.Remove(name); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func preserveConfigMetadata(_ string, replacement *os.File, info os.FileInfo) error {
	return replacement.Chmod(info.Mode().Perm())
}

func replaceConfigFile(source, target string) error {
	toWindowsPath := func(path string) string {
		if strings.HasPrefix(path, `\\?\`) {
			return path
		}
		if strings.HasPrefix(path, `\\`) {
			return `\\?\UNC\` + path[2:]
		}
		return `\\?\` + path
	}
	replaced, err := windows.UTF16PtrFromString(toWindowsPath(target))
	if err != nil {
		return err
	}
	replacement, err := windows.UTF16PtrFromString(toWindowsPath(source))
	if err != nil {
		return err
	}
	result, _, callErr := syscall.SyscallN(replaceFileW.Addr(), uintptr(unsafe.Pointer(replaced)), uintptr(unsafe.Pointer(replacement)), 0, 0, 0, 0)
	if result == 0 {
		if callErr != 0 {
			return callErr
		}
		return errors.New("ReplaceFileW failed")
	}
	return nil
}
