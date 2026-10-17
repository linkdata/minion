package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckUnchangedBoundsEachFileToBaseline(t *testing.T) {
	root := privateRoot(t)
	base := []File{{Path: "source", Mode: Regular, Data: []byte("before")}}
	if err := Materialize(root, base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("longer content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckUnchanged(context.Background(), root, base); err == nil || !strings.Contains(err.Error(), "byte limit") {
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
