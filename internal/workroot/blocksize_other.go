//go:build !linux

package workroot

import "syscall"

func filesystemBlockSize(stat syscall.Statfs_t) int64 {
	return int64(stat.Bsize)
}
