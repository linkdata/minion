package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestPrepareRootCanonicalizesExistingDirectory(t *testing.T) {
	parent, linkParent := t.TempDir(), t.TempDir()
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkParent, "linked")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(parent, "new"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := PrepareRoot(filepath.Join(link, "new"))
	if err != nil || root != filepath.Join(parent, "new") {
		t.Fatalf("prepared root=%q,%v", root, err)
	}
}

func TestPrepareRootDoesNotCreateMissingDirectory(t *testing.T) {
	for _, root := range []string{"", " \t\n"} {
		if _, err := PrepareRoot(root); err == nil || err.Error() != "work root is required" {
			t.Fatalf("blank root error=%v", err)
		}
	}
	name := filepath.Join(t.TempDir(), "missing", "nested")
	if _, err := PrepareRoot(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root error=%v", err)
	}
	if _, err := os.Lstat(filepath.Dir(name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PrepareRoot created a directory: %v", err)
	}
}

func TestPrepareRootResolvesRelativeParentsThroughPWD(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(root, "actual")
	nested := filepath.Join(actual, "nested")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", alias)
	if got, err := PrepareRoot(".."); err != nil || got != actual {
		t.Fatalf("relative parent resolved=%q want=%q error=%v", got, actual, err)
	}
}

func TestSharedAncestorValidationPreservesLayoutRules(t *testing.T) {
	for _, test := range []struct {
		name   string
		paths  []string
		folded bool
		want   string
	}{
		{"exact shared ancestor", []string{"Root/a/one", "Root/a/two", "Root/b/three"}, false, ""},
		{"exact different spelling", []string{"Root/a/one", "root/a/two"}, false, ""},
		{"folded shared ancestor", []string{"Root/a/one", "Root/a/two", "Root/b/three"}, true, ""},
		{"cached folded directory", []string{"Root/a/one", "Root/a/two", "root/a/three"}, true, "directory spelling collision"},
		{"higher folded directory", []string{"Root/a/one", "root/b/two"}, true, "directory spelling collision"},
		{"unicode folded directory", []string{"ſ/a/one", "s/b/two"}, true, "directory spelling collision"},
		{"exact file parent", []string{"Root/a/one", "Root/a/two", "Root/a"}, false, "file/directory collision"},
		{"folded file parent", []string{"Root/a/one", "Root/a/two", "root/A"}, true, "file/directory collision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validatePaths(test.paths, test.folded)
			if (err == nil) != (test.want == "") || (err != nil && !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("layout validation=%v, want %q", err, test.want)
			}
		})
	}
}

func TestSourceLayoutBoundsFilesAndDistinctAncestors(t *testing.T) {
	files := make([]File, MaxFiles)
	paths := make([]string, len(files))
	for i := range files {
		// Each file has nine private ancestors: exactly MaxEntries total.
		paths[i] = fmt.Sprintf("d%05d/a/b/c/d/e/f/g/h/file", i)
		files[i] = File{Path: paths[i], Mode: Regular}
	}
	for _, extra := range []bool{false, true} {
		if extra {
			files[0].Path = "extra/" + files[0].Path
		}
		for _, err := range []error{
			ValidateSnapshot(Snapshot{Head: strings.Repeat("a", 40), Files: files}),
			ValidateFixPaths(files, nil),
		} {
			if (err != nil) != extra || (err != nil && !strings.Contains(err.Error(), "entry limit")) {
				t.Fatalf("extra ancestor=%t validation=%v", extra, err)
			}
		}
	}
	if err := ValidateChangedPaths(paths, []Change{{Path: "new-file", Mode: Regular}}); err == nil || !strings.Contains(err.Error(), "entry limit") {
		t.Fatalf("final layout exceeded entry bound: %v", err)
	}
	for i := range files {
		files[i].Path = fmt.Sprintf("shared/a/b/c/d/e/f/g/h/i/file-%05d", i)
	}
	if err := ValidateSnapshot(Snapshot{Head: strings.Repeat("a", 40), Files: files}); err != nil {
		t.Fatalf("shared ancestors counted repeatedly: %v", err)
	}
}

func BenchmarkDeepSharedPaths(b *testing.B) {
	prefix := strings.Repeat("a/", 2000)
	files := make([]File, 300)
	for i := range files {
		files[i] = File{Path: fmt.Sprintf("%sfile-%03d", prefix, i), Mode: Regular}
	}
	b.Run("snapshot", func(b *testing.B) {
		snapshot := Snapshot{Head: strings.Repeat("a", 40), Files: files}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := ValidateSnapshot(snapshot); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("fix layout", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := ValidateFixPaths(files, nil); err != nil {
				b.Fatal(err)
			}
		}
	})

}

func TestFoldPathMatchesUnicodeSimpleCaseFolding(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		name := string(r)
		key := FoldPath(name)
		if !strings.EqualFold(name, key) {
			t.Fatalf("folded non-equivalent rune: %U => %q", r, key)
		}
		// Every orbit edge must keep its key; EqualFold checks the canonical
		// spelling independently without storing a million-rune lookup table.
		next := string(unicode.SimpleFold(r))
		if strings.EqualFold(name, next) && FoldPath(next) != key {
			t.Fatalf("equivalent runes have distinct keys: %U => %q", r, next)
		}
	}
}
