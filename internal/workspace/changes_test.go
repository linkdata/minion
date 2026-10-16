package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixLayoutIncludesOmittedFilesAndAncestorAliases(t *testing.T) {
	for _, names := range [][2]string{{"link/a", "Link"}, {"LINK", "link/b"}, {"ſ/a", "s"}, {"Docs/a", "docs/b"}, {"File", "file"}} {
		files := []File{{Path: names[0], Mode: Regular}}
		if err := ValidateFixPaths(files, []string{names[1]}); err == nil || (!strings.Contains(err.Error(), "collision") && !strings.Contains(err.Error(), "duplicate source path")) {
			t.Fatalf("accepted folded file/omission layout %q: %v", names, err)
		}
	}
	if err := ValidateFixPaths([]File{{Path: "folder/a", Mode: Regular}}, []string{"folder/link"}); err != nil {
		t.Fatalf("refused disjoint omitted path in the same directory: %v", err)
	}
	if err := ValidateFixPaths(nil, []string{"Link", "link"}); err == nil || !strings.Contains(err.Error(), "duplicate source path") {
		t.Fatalf("accepted folded omission pair: %v", err)
	}
}

func TestCheckUnchangedRefusesUnsafeWorkerNames(t *testing.T) {
	for _, test := range []struct {
		name      string
		directory bool
	}{{string([]byte{'x', 0xff}), false}, {string([]byte{'x', 0xff}), true}, {"new\\output", false}, {".gIt", true}} {
		label := strings.ToValidUTF8(test.name, "invalid")
		if test.directory {
			label += "-directory"
		}
		t.Run(label, func(t *testing.T) {
			root := t.TempDir()
			name := filepath.Join(root, test.name)
			if test.directory {
				if err := os.MkdirAll(name, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(name, []byte("worker output"), 0600); err != nil {
				t.Fatal(err)
			}
			want := "unsafe"
			if test.name == "new\\output" {
				want = "modified supplied source"
			}
			if err := CheckUnchanged(context.Background(), root, nil); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("unsafe worker name %q validation=%v", test.name, err)
			}
		})
	}
}
