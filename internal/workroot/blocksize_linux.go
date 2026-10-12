//go:build linux

package workroot

import "syscall"

func filesystemBlockSize(stat syscall.Statfs_t) int64 {
	// Linux counts blocks in Frsize units; older kernels may leave it zero.
	if stat.Frsize != 0 {
		return int64(stat.Frsize)
	}
	return int64(stat.Bsize)
}
