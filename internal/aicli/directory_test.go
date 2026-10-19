package aicli

import (
	"errors"
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
	if _, err := Directory(string(os.PathSeparator)); err == nil || err.Error() != "expected a non-root directory: "+string(os.PathSeparator) {
		t.Fatalf("root error lost its path: %v", err)
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

func TestDirectoryResolvesSymlinksBeforeParentComponents(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(root, "actual")
	for _, dir := range []string{filepath.Join(actual, "nested"), filepath.Join(actual, "directory"), filepath.Join(root, "file")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{filepath.Join(actual, "file"), filepath.Join(root, "directory")} {
		if err := os.WriteFile(file, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(actual, "nested"), alias); err != nil {
		t.Fatal(err)
	}
	// Join would clean away the parent component before symlink resolution.
	if got, err := Directory(alias + "/../directory"); err != nil || got != filepath.Join(actual, "directory") {
		t.Fatalf("directory resolved=%q error=%v", got, err)
	}
	file := alias + "/../file"
	if _, err := Directory(file); err == nil || err.Error() != "expected a non-root directory: "+filepath.Join(actual, "file") {
		t.Fatalf("accepted the actual file or lost its canonical path: %v", err)
	}
}

func TestResolvePathKeepsFilesystemSemanticsWithoutCreating(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(root, "actual")
	if err := os.MkdirAll(filepath.Join(actual, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(actual, "nested"), alias); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "absent"), dangling); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, alias)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, path, want string
	}{
		{"existing parent", alias + "/..", actual},
		{"missing suffix", alias + "/../new/home", filepath.Join(actual, "new", "home")},
		{"relative missing suffix", relative + "/../new/home", filepath.Join(actual, "new", "home")},
		{"missing suffix dot", root + "/missing/./x", filepath.Join(root, "missing", "x")},
		{"missing suffix trailing dot", root + "/missing/.", filepath.Join(root, "missing")},
		{"dangling link", dangling + "/home", ""},
		{"unknown parent", root + "/absent/../target", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolvePath(test.path)
			if (err == nil) != (test.want != "") || got != test.want {
				t.Fatalf("resolved=%q want=%q error=%v", got, test.want, err)
			}
		})
	}
	for _, path := range []string{filepath.Join(actual, "new"), filepath.Join(root, "absent")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("resolution created %q: %v", path, err)
		}
	}
}
