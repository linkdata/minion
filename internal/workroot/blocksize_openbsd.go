//go:build openbsd

package workroot

import "syscall"

func filesystemStats(stat syscall.Statfs_t) (blocks, available uint64, blockSize int64) {
	return stat.F_blocks, uint64(max(0, stat.F_bavail)), int64(stat.F_bsize)
}
