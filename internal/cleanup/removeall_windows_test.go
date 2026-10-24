package cleanup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveAllWindowsRemovesJobTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "job")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "output.txt"), []byte("output"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("job directory remains: %v", err)
	}
}

func TestRemoveAllWindowsKeepsSymlinkTarget(t *testing.T) {
	parent, outside := t.TempDir(), t.TempDir()
	root := filepath.Join(parent, "job")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("Windows runner cannot create a directory symlink: %v", err)
	}
	if err := RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("job directory remains: %v", err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatalf("cleanup touched symlink target: %q, %v", data, err)
	}
}
