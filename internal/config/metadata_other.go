//go:build !linux && !darwin && !windows

package config

import (
	"errors"
	"os"
)

func newConfigTemp(string) (*os.File, error) {
	return nil, errors.New("atomic config updates are unsupported on this platform")
}

func cleanupConfigTemp(string) error { return nil }

func preserveConfigMetadata(string, *os.File, os.FileInfo) error {
	return errors.New("atomic config updates are unsupported on this platform")
}
