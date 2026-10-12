//go:build linux

package workroot

import (
	"syscall"
	"testing"
)

func TestFilesystemBlockSize(t *testing.T) {
	for _, tt := range []struct {
		name string
		stat syscall.Statfs_t
		want int64
	}{
		{"fragment size", syscall.Statfs_t{Bsize: 4096, Frsize: 512}, 512},
		{"larger fragment size", syscall.Statfs_t{Bsize: 4096, Frsize: 8192}, 8192},
		{"legacy fallback", syscall.Statfs_t{Bsize: 4096}, 4096},
		{"invalid fragment size", syscall.Statfs_t{Bsize: 4096, Frsize: -1}, -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := filesystemBlockSize(tt.stat); got != tt.want {
				t.Fatalf("filesystem block size = %d, want %d", got, tt.want)
			}
		})
	}
}
