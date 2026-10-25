package gitwork

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestImportCaseOnlyRenameWithIgnoreCase(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.checkout.path, "config", "core.ignoreCase", "true")
	if err := fixture.checkout.recordConfig(); err != nil {
		t.Fatal(err)
	}
	changes := []workspace.Change{
		{Path: "README.md", Delete: true},
		{Path: "readme.md", Mode: workspace.Regular, Data: []byte("renamed\n")},
	}
	if err := fixture.checkout.Import(t.Context(), fixture.base, changes); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fixture.checkout.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "README.md" {
			t.Fatal("old path remains on disk")
		}
	}
	if data, err := os.ReadFile(filepath.Join(fixture.checkout.path, "readme.md")); err != nil || string(data) != "renamed\n" {
		t.Fatalf("renamed file=%q error=%v", data, err)
	}
	if staged := fixture.git(fixture.checkout.path, "diff", "--cached", "--name-status", "--no-renames"); staged != "D\tREADME.md\nA\treadme.md" {
		t.Fatalf("staged paths=%q", staged)
	}
}
