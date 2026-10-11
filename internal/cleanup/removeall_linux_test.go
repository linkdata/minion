//go:build linux

package cleanup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestRemoveAllDeepRestrictedTree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 26; i++ {
		name := strings.Repeat("d", 180)
		if err := syscall.Mkdirat(int(dir.Fd()), name, 0700); err != nil {
			t.Fatal(err)
		}
		fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = dir.Close()
		dir = os.NewFile(uintptr(fd), name)
	}
	t.Cleanup(func() {
		_ = dir.Chmod(0700)
		_ = dir.Close()
		_ = os.RemoveAll(root)
	})
	fd, err := syscall.Openat(int(dir.Fd()), "file", syscall.O_WRONLY|syscall.O_CREAT, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Close(fd)
	if err := dir.Chmod(0000); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll() = %v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime tree remains: %v", err)
	}
}
