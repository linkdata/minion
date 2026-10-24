package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsSourcePaths(t *testing.T) {
	for _, test := range []struct {
		name  string
		valid bool
	}{
		{"ordinary/file.go", true},
		{"NUL", false},
		{"dir/COM1", false},
		{"git~1/config", false},
		{"file.", false},
		{"file ", false},
		{"file:stream", false},
		{`dir\file`, false},
	} {
		if got := validSourcePath(test.name); got != test.valid {
			t.Errorf("validSourcePath(%q) = %t, want %t", test.name, got, test.valid)
		}
	}
}

func TestWindowsOpenPathRejectsLink(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "source")
	if err := os.WriteFile(name, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := openPath(name, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(name, link); err != nil {
		t.Skipf("Windows symlink privilege unavailable: %v", err)
	}
	if file, err := openPath(link, false); err == nil {
		_ = file.Close()
		t.Fatal("opened source through a symlink")
	}
}

func TestWindowsReadRejectsHardLink(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "source")
	if err := os.WriteFile(name, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(name, filepath.Join(root, "alias")); err != nil {
		t.Skipf("Windows hard links unavailable: %v", err)
	}
	if _, err := ReadRegular(name, MaxBytes); err == nil {
		t.Fatal("read file with multiple hard links")
	}
}

func TestWindowsUsesSelectedGitMode(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "tool")
	if err := os.WriteFile(name, []byte("content"), 0755); err != nil {
		t.Fatal(err)
	}
	file := File{Path: "tool", Mode: Executable, Data: []byte("content")}
	if err := CheckUnchanged(t.Context(), root, []File{file}); err != nil {
		t.Fatal(err)
	}
	changes, err := ReadChanges(t.Context(), root, []Change{{Path: "tool", Mode: Executable}})
	if err != nil || len(changes) != 1 || changes[0].Mode != Executable {
		t.Fatalf("selected executable mode lost: changes=%+v error=%v", changes, err)
	}
}
