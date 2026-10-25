package workspace

import "io/fs"

// Windows file mode does not carry Git's executable bit. The private Git index
// supplies that bit; reject a read-only file instead of inventing a mode change.
func sourceModeMatches(info fs.FileInfo, _ uint32) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0200 != 0
}

func changedSourceMode(info fs.FileInfo, expected uint32) uint32 {
	if sourceModeMatches(info, expected) {
		return expected
	}
	return 0
}
