package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// WriteAtomic replaces a private record, then syncs its directory and extra ancestors except on Windows.
// The caller validates paths and serializes updates; sync failure can follow replacement.
func WriteAtomic(path string, data []byte, extraDirs ...string) (resultErr error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".record-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() {
		if temporary != "" {
			if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	writeErr := file.Chmod(0o600)
	if writeErr == nil {
		_, writeErr = file.Write(data)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	// Root.Rename requests POSIX replacement semantics on Windows for open prior records.
	err = root.Rename(filepath.Base(temporary), filepath.Base(path))
	if err != nil {
		op := "rename"
		if linkErr, ok := err.(*os.LinkError); ok {
			op, err = linkErr.Op, linkErr.Err
		}
		err = &os.LinkError{Op: op, Old: temporary, New: path, Err: err}
	}
	if err := errors.Join(err, root.Close()); err != nil {
		return err
	}
	temporary = ""
	// shortcut: Windows directory Sync lacks write access; revisit if crash durability is required.
	if runtime.GOOS == "windows" {
		return nil
	}
	for _, directory := range append([]string{filepath.Dir(path)}, extraDirs...) {
		file, err := os.Open(directory)
		if err != nil {
			return err
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	return nil
}
