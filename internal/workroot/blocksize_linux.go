//go:build linux

package workroot

import "syscall"

func filesystemStats(stat syscall.Statfs_t) (blocks, available uint64, blockSize int64) {
	// Linux counts blocks in Frsize units; older kernels may leave it zero.
	if stat.Frsize != 0 {
		blockSize = int64(stat.Frsize)
	} else {
		blockSize = int64(stat.Bsize)
	}
	return stat.Blocks, stat.Bavail, blockSize
}
