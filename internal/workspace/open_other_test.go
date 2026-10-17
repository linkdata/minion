//go:build !linux

package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyRequiresLinuxBeforeChangingFiles(t *testing.T) {
	for _, change := range []Change{
		{Path: "existing", Mode: Regular, Data: []byte("after")},
		{Path: "existing", Delete: true},
		{Path: "added", Mode: Regular, Data: []byte("new")},
	} {
		root := t.TempDir()
		filename := filepath.Join(root, "existing")
		if err := os.WriteFile(filename, []byte("before"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Apply(root, []Change{change}); err == nil || !strings.Contains(err.Error(), "requires Linux") {
			t.Fatalf("unsupported import did not report its platform requirement: %v", err)
		}
		if data, err := os.ReadFile(filename); err != nil || string(data) != "before" {
			t.Fatalf("unsupported import changed existing data: %q,%v", data, err)
		}
		if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
			t.Fatalf("unsupported import changed root layout: %v,%v", entries, err)
		}
	}
}
