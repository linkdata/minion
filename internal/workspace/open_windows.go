package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func singleLink(_ os.FileInfo, file *os.File) bool {
	if file == nil {
		return false
	}
	var info windows.ByHandleFileInformation
	return windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info) == nil && info.NumberOfLinks == 1
}

type windowsFileBasicInfo struct {
	CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
	FileAttributes                                          uint32
	_                                                       uint32
}

func changeTime(_ os.FileInfo, file *os.File) ([2]int64, error) {
	var info windowsFileBasicInfo
	err := windows.GetFileInformationByHandleEx(windows.Handle(file.Fd()), windows.FileBasicInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err == nil && (info.ChangeTime == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0) {
		err = errors.New("regular file change time unavailable or file is a reparse point")
	}
	return [2]int64{info.ChangeTime}, err
}

// openPath walks one existing component at a time under a pinned volume root.
// Comparing each link itself with the opened handle rejects junctions and symlink swaps.
func openPath(name string, directory bool) (file *os.File, resultErr error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(abs)
	if volume == "" {
		return nil, fmt.Errorf("path %q has no volume", name)
	}
	volumeRoot := volume + string(os.PathSeparator)
	rel, err := filepath.Rel(volumeRoot, abs)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return nil, fmt.Errorf("path %q is not below a volume root", name)
	}
	root, err := os.OpenRoot(volumeRoot)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	parts := strings.Split(rel, string(os.PathSeparator))
	for index, part := range parts {
		if !validWindowsComponent(part) {
			return nil, fmt.Errorf("unsafe Windows path component %q", part)
		}
		before, err := root.Lstat(part)
		if err != nil {
			return nil, err
		}
		last := index == len(parts)-1
		if before.Mode().IsRegular() && (!last || directory) {
			return nil, fmt.Errorf("path component %q is not a plain directory: %w", part, syscall.ENOTDIR)
		}
		if !last {
			if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("path component %q is not a plain directory", part)
			}
			next, err := root.OpenRoot(part)
			if err != nil {
				return nil, err
			}
			after, err := next.Stat(".")
			if err != nil || !os.SameFile(before, after) {
				return nil, errors.Join(fmt.Errorf("path component %q changed or is a link", part), err, next.Close())
			}
			if err := root.Close(); err != nil {
				return nil, errors.Join(err, next.Close())
			}
			root = next
			continue
		}
		if directory && !before.IsDir() || !directory && !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("path %q is not a plain %s", name, map[bool]string{true: "directory", false: "regular file"}[directory])
		}
		file, err := root.Open(part)
		if err != nil {
			return nil, err
		}
		after, err := file.Stat()
		if err != nil || !os.SameFile(before, after) || directory != after.IsDir() {
			return nil, errors.Join(fmt.Errorf("path %q changed or is a link", name), err, file.Close())
		}
		return file, nil
	}
	return nil, errors.New("path has no components")
}
