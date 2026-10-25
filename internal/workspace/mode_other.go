//go:build !windows

package workspace

import (
	"io/fs"
)

func sourceModeMatches(info fs.FileInfo, expected uint32) bool {
	return info.Mode() == fs.FileMode(expected&0777)
}

func changedSourceMode(info fs.FileInfo, _ uint32) uint32 {
	if info.Mode().Perm()&0100 != 0 {
		return Executable
	}
	return Regular
}
