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

func TestRemoveAllRestrictedTreeDoesNotFollowSymlinks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root, outside := filepath.Join(t.TempDir(), "runtime"), t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0700); err != nil {
		t.Fatal(err)
	}
	const content = "outside bytes must survive"
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(locked, "link")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(locked, 0700)
		_ = os.Chmod(outside, 0700)
	})
	for _, dir := range []string{locked, outside} {
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(root); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("fixture did not reach permission repair: %v", err)
	}
	if err := RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restricted tree remains: %v", err)
	}
	info, err := os.Stat(outside)
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("cleanup changed outside directory mode: %v, %v", info, err)
	}
	if got, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(got) != content {
		t.Fatalf("cleanup changed outside bytes: %q, %v", got, err)
	}
}

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
	var lockedParent *os.File
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
		if i == 22 {
			copyFD, err := syscall.Dup(fd)
			if err != nil {
				t.Fatal(err)
			}
			lockedParent = os.NewFile(uintptr(copyFD), name)
		}
	}
	t.Cleanup(func() {
		_ = dir.Chmod(0700)
		_ = dir.Close()
		_ = lockedParent.Chmod(0700)
		_ = lockedParent.Close()
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
	if err := lockedParent.Chmod(0000); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err == nil {
		t.Skip("plain RemoveAll bypassed directory permissions")
	}
	if err := RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll() = %v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime tree remains: %v", err)
	}
}
