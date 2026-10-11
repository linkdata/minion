// Package workroot verifies that untrusted job files cannot fill the state filesystem.
package workroot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	DefaultMaxBytes int64 = 8 << 30
	minFreeBytes    int64 = 64 << 20
)

var ErrUnsafe = errors.New("work_root is not a bounded, separate filesystem")

// Check requires an existing mount at workRoot whose total capacity is within
// maxBytes. The state directory must be on another filesystem. Call before
// startup and each job so a missing mount cannot spill writes onto its parent.
func Check(workRoot, statePath string, maxBytes int64) error {
	if maxBytes <= 0 {
		return fmt.Errorf("%w: work_root_max_bytes must be positive", ErrUnsafe)
	}
	workRoot, err := filepath.EvalSymlinks(workRoot)
	if err != nil {
		return fmt.Errorf("%w: resolve work_root: %v", ErrUnsafe, err)
	}
	stateDir, err := existingStateDir(filepath.Dir(statePath))
	if err != nil {
		return fmt.Errorf("%w: resolve state directory: %v", ErrUnsafe, err)
	}
	work, err := filesystemDevice(workRoot)
	if err != nil {
		return fmt.Errorf("%w: work_root: %v", ErrUnsafe, err)
	}
	parent, err := filesystemDevice(filepath.Dir(workRoot))
	if err != nil {
		return fmt.Errorf("%w: work_root parent: %v", ErrUnsafe, err)
	}
	state, err := filesystemDevice(stateDir)
	if err != nil {
		return fmt.Errorf("%w: state directory: %v", ErrUnsafe, err)
	}
	if err := validateDevices(work, parent, state); err != nil {
		return err
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(workRoot, &stat); err != nil {
		return fmt.Errorf("%w: statfs work_root: %v", ErrUnsafe, err)
	}
	if err := validateCapacity(stat.Blocks, stat.Bsize, maxBytes); err != nil {
		return fmt.Errorf("%w: %s exceeds work_root_max_bytes (%d bytes) or has unknown capacity", ErrUnsafe, workRoot, maxBytes)
	}
	if err := validateFreeSpace(stat.Bavail, stat.Bsize); err != nil {
		return fmt.Errorf("%w: %s has less than %d bytes free", ErrUnsafe, workRoot, minFreeBytes)
	}
	return nil
}

// The state directory may not exist before the first run; state.Open creates it.
// Resolve the nearest existing ancestor to find the filesystem it will use.
func existingStateDir(dir string) (string, error) {
	for {
		_, err := os.Lstat(dir)
		if err == nil {
			return filepath.EvalSymlinks(dir)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", err
		}
		dir = parent
	}
}

func filesystemDevice(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("cannot identify filesystem for %s", path)
	}
	return uint64(stat.Dev), nil
}

func validateDevices(work, parent, state uint64) error {
	if work == parent {
		return fmt.Errorf("%w: work_root must be a mount point", ErrUnsafe)
	}
	if work == state {
		return fmt.Errorf("%w: work_root and state directory share a filesystem", ErrUnsafe)
	}
	return nil
}

func validateCapacity(blocks uint64, blockSize, maxBytes int64) error {
	if blockSize <= 0 || blocks == 0 || maxBytes <= 0 || blocks > uint64(maxBytes)/uint64(blockSize) {
		return ErrUnsafe
	}
	return nil
}

func validateFreeSpace(available uint64, blockSize int64) error {
	if blockSize <= 0 {
		return ErrUnsafe
	}
	required := uint64(minFreeBytes) / uint64(blockSize)
	if uint64(minFreeBytes)%uint64(blockSize) != 0 {
		required++
	}
	if available < required {
		return ErrUnsafe
	}
	return nil
}
