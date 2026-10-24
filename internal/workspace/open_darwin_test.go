//go:build darwin

package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinAnchoredReadRejectsLinksAndSpecialFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "source")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadRegular(file, 6); err != nil || string(data) != "source" {
		t.Fatalf("regular read = %q, %v", data, err)
	}
	if err := os.Symlink(dir, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRegular(filepath.Join(root, "link", "file"), 6); err == nil {
		t.Fatal("followed symlink parent")
	}
	if err := os.Link(file, filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRegular(file, 6); err == nil {
		t.Fatal("accepted multiply linked file")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRegular(fifo, 6); err == nil {
		t.Fatal("accepted FIFO")
	}
}
