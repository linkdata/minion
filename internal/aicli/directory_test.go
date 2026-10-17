package aicli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirectoryRequiresAnExistingNonRootDirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", " ", string(os.PathSeparator), file, filepath.Join(root, "missing")} {
		if _, err := Directory(path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Directory(alias); err != nil || got != want {
		t.Fatalf("resolved=%q want=%q err=%v", got, want, err)
	}
}
