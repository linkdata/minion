package workspace

import (
	"errors"
	"os"
	"path/filepath"
)

// WriteAtomic replaces a private record, then syncs the supplied directories.
// The caller validates paths and serializes updates; sync failure can follow replacement.
func WriteAtomic(path string, data []byte, syncDirs ...string) (resultErr error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".record-*")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(file.Name()); !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	for _, directory := range syncDirs {
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
