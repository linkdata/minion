//go:build !linux

package workspace

import (
	"errors"
	"os"
)

func openPath(string, bool) (*os.File, error) {
	return nil, errors.New("safe workspace collection requires Linux")
}

func singleLink(os.FileInfo) bool { return false }

func sameChangeTime(os.FileInfo, os.FileInfo) bool { return false }
