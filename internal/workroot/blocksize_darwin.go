//go:build darwin

package workroot

import "syscall"

func filesystemStats(stat syscall.Statfs_t) (blocks, available uint64, blockSize int64) {
	return stat.Blocks, stat.Bavail, int64(stat.Bsize)
}
