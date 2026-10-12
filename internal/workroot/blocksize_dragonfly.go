//go:build dragonfly

package workroot

import "syscall"

func filesystemStats(stat syscall.Statfs_t) (blocks, available uint64, blockSize int64) {
	return uint64(max(0, stat.Blocks)), uint64(max(0, stat.Bavail)), stat.Bsize
}
