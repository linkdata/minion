//go:build !linux

package workspace

import (
	"errors"
	"os"
)

func openRegular(name string) (*os.File, error) {
	return nil, errors.New("safe workspace collection requires Linux")
}

func openDirectory(name string) (*os.File, error) {
	return nil, errors.New("safe workspace collection requires Linux")
}

func singleLink(os.FileInfo) bool { return false }
