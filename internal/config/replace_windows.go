package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var replaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func newConfigTemp(target string) (*os.File, error) {
	sd, err := windows.GetNamedSecurityInfo(extendedWindowsPath(target), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	if sd == nil {
		return nil, errors.New("config has no security descriptor")
	}
	abs, err := sd.ToAbsolute()
	if err != nil {
		return nil, err
	}
	if err := abs.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, err
	}
	protected, err := abs.ToSelfRelative()
	if err != nil {
		return nil, err
	}
	name := filepath.Join(filepath.Dir(target), ".config-"+rand.Text())
	path, err := windows.UTF16PtrFromString(extendedWindowsPath(name))
	if err != nil {
		return nil, err
	}
	attributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: protected,
	}
	handle, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	runtime.KeepAlive(protected)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), name), nil
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

func extendedWindowsPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}

func replaceConfigFile(source, target string) error {
	replaced, err := windows.UTF16PtrFromString(extendedWindowsPath(target))
	if err != nil {
		return err
	}
	replacement, err := windows.UTF16PtrFromString(extendedWindowsPath(source))
	if err != nil {
		return err
	}
	backupName := source + ".backup"
	backup, err := windows.UTF16PtrFromString(extendedWindowsPath(backupName))
	if err != nil {
		return err
	}
	result, _, callErr := syscall.SyscallN(replaceFileW.Addr(), uintptr(unsafe.Pointer(replaced)), uintptr(unsafe.Pointer(replacement)), uintptr(unsafe.Pointer(backup)), 0, 0, 0)
	if result == 0 {
		if errors.Is(callErr, windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT_2) {
			if restoreErr := windows.MoveFileEx(backup, replaced, 0); restoreErr != nil {
				return errors.Join(callErr, fmt.Errorf("restore original config from %q: %w", backupName, restoreErr))
			}
		}
		if callErr != 0 {
			return callErr
		}
		return errors.New("ReplaceFileW failed")
	}
	// The replacement has committed; backup removal cannot make the save fail.
	_ = os.Remove(backupName)
	return nil
}
