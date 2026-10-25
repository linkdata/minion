//go:build !linux && !darwin && !windows

package workspace

import (
	"errors"
	"os"
)

func openPath(string, bool) (*os.File, error) {
	return nil, errors.New("safe workspace collection is unsupported on this platform")
}

func singleLink(os.FileInfo, *os.File) bool { return false }

func changeTime(os.FileInfo, *os.File) ([2]int64, error) {
	return [2]int64{}, errors.New("file change time is unavailable")
}
