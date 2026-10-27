package state

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/linkdata/minion/internal/workspace"
)

// WriteAcceptedResult atomically replaces the accepted result sidecar.
// The caller serializes this operation with any active action callbacks.
func (s *Store) WriteAcceptedResult(data []byte) error {
	if s.syncDir != "" {
		return workspace.WriteAtomic(s.path+".result", data, s.syncDir)
	}
	return workspace.WriteAtomic(s.path+".result", data)
}

// A visible sidecar is not a durable receipt until its directory entry has
// been synced. WriteAtomic can leave the renamed file present on sync failure.
func (s *Store) syncAcceptedResultDir() error {
	if s.syncDir != "" {
		return workspace.SyncDirectories(filepath.Dir(s.path), s.syncDir)
	}
	return workspace.SyncDirectories(filepath.Dir(s.path))
}

// ReadAcceptedResult reads a bounded accepted result, returning [os.ErrNotExist] when absent.
// The caller serializes this operation with any active action callbacks.
func (s *Store) ReadAcceptedResult(maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return nil, errors.New("accepted result size limit must be nonnegative")
	}
	file, err := os.Open(s.path + ".result")
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	limit := maxBytes
	if limit < math.MaxInt64 {
		limit++
	}
	data, readErr := io.ReadAll(io.LimitReader(file, limit))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("accepted result exceeds %d bytes", maxBytes)
	}
	if err := s.syncAcceptedResultDir(); err != nil {
		return nil, fmt.Errorf("sync accepted result directory: %w", err)
	}
	return data, nil
}

// HasAcceptedResult reports whether a result receipt exists without reading its large body.
// A present but invalid receipt is checked by the publication handler before use.
func (s *Store) HasAcceptedResult() (bool, error) {
	info, err := os.Lstat(s.path + ".result")
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("accepted result is not a regular file")
	}
	if err := s.syncAcceptedResultDir(); err != nil {
		return false, fmt.Errorf("sync accepted result directory: %w", err)
	}
	return true, nil
}

// ClearAcceptedResult removes the accepted result sidecar and syncs its directory.
// The caller serializes this operation with any active action callbacks.
func (s *Store) ClearAcceptedResult() error {
	if err := os.Remove(s.path + ".result"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return workspace.SyncDirectories(filepath.Dir(s.path))
}
