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
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	temporary = ""
	for _, directory := range append([]string{filepath.Dir(path)}, extraDirs...) {
		file, err := os.Open(directory)
		if err != nil {
			return err
		}
		var syncErr error
		// shortcut: Windows os.Open lacks write access for directory Sync; revisit if crash durability is required.
		if runtime.GOOS != "windows" {
			syncErr = file.Sync()
		}
		if err := errors.Join(syncErr, file.Close()); err != nil {
			return err
		}
	}
	return nil
}
