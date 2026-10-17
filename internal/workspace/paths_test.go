package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestPrepareRootCreatesCanonicalDirectory(t *testing.T) {
	parent, linkParent := t.TempDir(), t.TempDir()
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkParent, "linked")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	root, err := PrepareRoot(filepath.Join(link, "new"))
	if err != nil || root != filepath.Join(parent, "new") {
		t.Fatalf("prepared root=%q,%v", root, err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("prepared root permissions=%v,%v", info, err)
	}
}

func TestCheckUnchangedBoundsEachFileToBaseline(t *testing.T) {
	root := privateRoot(t)
	base := []File{{Path: "source", Mode: Regular, Data: []byte("before")}}
	if err := Materialize(root, base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("longer content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckUnchanged(context.Background(), root, base); err == nil || !strings.Contains(err.Error(), "modified supplied source") || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("grown source validation=%v", err)
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

func deepSharedFiles() []File {
	prefix := strings.Repeat("a/", 2000)
	files := make([]File, 300)
	for i := range files {
		files[i] = File{Path: fmt.Sprintf("%sfile-%03d", prefix, i), Mode: Regular}
	}
	return files
}

func BenchmarkDeepSharedPaths(b *testing.B) {
	files := deepSharedFiles()
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
	b.Run("materialize", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			root, err := os.MkdirTemp("", "minion-deep-")
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			err = Materialize(root, files)
			b.StopTimer()
			cleanupErr := os.RemoveAll(root)
			if err != nil || cleanupErr != nil {
				b.Fatalf("materialize=%v, cleanup=%v", err, cleanupErr)
			}
			b.StartTimer()
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
