//go:build windows

package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func canonicalPath(path string) (string, error) {
	volume := filepath.VolumeName(path)
	if volume == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	// Resolve before each parent component: cleaning first would erase a junction target.
	current := volume + string(os.PathSeparator)
	for _, part := range strings.FieldsFunc(path[len(volume):], func(r rune) bool { return r == '\\' || r == '/' }) {
		if part == "." || part == ".." {
			resolved, isDir, err := finalWindowsPath(current)
			if err != nil {
				return "", err
			}
			if !isDir {
				return "", syscall.ENOTDIR
			}
			current = resolved
			if part == ".." {
				current = filepath.Dir(current)
			}
			continue
		}
		if !os.IsPathSeparator(current[len(current)-1]) {
			current += string(os.PathSeparator)
		}
		current += part
	}
	resolved, isDir, err := finalWindowsPath(current)
	if err != nil {
		return "", err
	}
	if !isDir && os.IsPathSeparator(path[len(path)-1]) {
		return "", syscall.ENOTDIR
	}
	return resolved, nil
}

func finalWindowsPath(path string) (string, bool, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false, err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", false, err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return "", false, err
	}
	buf := make([]uint16, 256)
	const volumeNameGUID = 1 // GetFinalPathNameByHandle VOLUME_NAME_GUID.
	flags := uint32(0)
	for {
		n, err := windows.GetFinalPathNameByHandle(handle, &buf[0], uint32(len(buf)), flags)
		if err == windows.ERROR_PATH_NOT_FOUND && flags == 0 {
			flags = volumeNameGUID
			continue
		}
		if err != nil {
			return "", false, err
		}
		if n < uint32(len(buf)) {
			break
		}
		buf = make([]uint16, n+1)
	}
	resolved := windowsPathFromHandle(windows.UTF16ToString(buf))
	if !filepath.IsAbs(resolved) {
		return "", false, fmt.Errorf("path %q has no canonical DOS, UNC, or volume GUID path", path)
	}
	// Normalize 8.3 spelling so containment comparisons match other canonical paths.
	resolved, err = filepath.EvalSymlinks(resolved)
	return resolved, info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0, err
}

func windowsPathFromHandle(resolved string) string {
	if strings.HasPrefix(strings.ToUpper(resolved), `\\?\UNC\`) {
		resolved = `\\` + resolved[len(`\\?\UNC\`):]
	} else if strings.HasPrefix(resolved, `\\?\`) && filepath.IsAbs(resolved[len(`\\?\`):]) {
		resolved = resolved[len(`\\?\`):]
	}
	return resolved
}
