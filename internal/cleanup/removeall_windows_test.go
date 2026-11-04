package cleanup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/linkdata/minion/internal/wintest"
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

func TestRemoveAllWindowsKeepsJunctionTarget(t *testing.T) {
	parent, outside := t.TempDir(), t.TempDir()
	job := filepath.Join(parent, "job")
	if err := os.Mkdir(job, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := wintest.CreateJunction(filepath.Join(job, "link"), outside); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(job); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(job); !os.IsNotExist(err) {
		t.Fatalf("job directory remains: %v", err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatalf("cleanup touched junction target: %q, %v", data, err)
	}
}

func TestRemoveAllWindowsReplacedRootKeepsOutside(t *testing.T) {
	parent, outside := t.TempDir(), t.TempDir()
	job := filepath.Join(parent, "job")
	if err := os.Mkdir(job, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job, "original"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(job, filepath.Join(parent, "renamed")); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := wintest.CreateJunction(job, outside); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(job); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(job); !os.IsNotExist(err) {
		t.Fatalf("replacement junction remains: %v", err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatalf("cleanup touched replacement target: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(parent, "renamed", "original")); err != nil || string(data) != "original" {
		t.Fatalf("cleanup touched displaced job: %q, %v", data, err)
	}
}

func TestRemoveAllWindowsReportsFailure(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(filepath.Join(parent, "child")); err == nil {
		t.Fatal("cleanup failure was suppressed")
	}
	if data, err := os.ReadFile(parent); err != nil || string(data) != "untouched" {
		t.Fatalf("failed cleanup changed parent file: %q, %v", data, err)
	}
}
