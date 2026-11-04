package gitwork

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/wintest"
	"github.com/linkdata/minion/internal/workspace"
)

func TestWindowsImportAnchorsOrdinaryAndHostileTargets(t *testing.T) {
	t.Run("ordinary", func(t *testing.T) {
		fixture := newFixture(t)
		change := []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("reviewed\n")}}
		if err := fixture.checkout.Import(t.Context(), fixture.base, change); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(filepath.Join(fixture.checkout.path, "README.md")); err != nil || string(data) != "reviewed\n" {
			t.Fatalf("ordinary import=%q, %v", data, err)
		}
		if count, err := fixture.checkout.stagedPathCount(t.Context()); err != nil || count != 1 {
			t.Fatalf("ordinary imported paths=%d, %v", count, err)
		}
	})
	t.Run("hard link", func(t *testing.T) {
		fixture := newFixture(t)
		name := filepath.Join(fixture.checkout.path, "README.md")
		alias := filepath.Join(t.TempDir(), "outside")
		wintest.RequireHardLink(t, name, alias)
		change := []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("reviewed\n")}}
		if err := fixture.checkout.Import(t.Context(), fixture.base, change); err == nil || !strings.Contains(err.Error(), "exactly one hard link") {
			t.Fatalf("hardlinked import accepted: %v", err)
		}
		for _, path := range []string{name, alias} {
			if data, err := os.ReadFile(path); err != nil || string(data) != "fixture\n" {
				t.Fatalf("hardlinked source changed: %q, %v", data, err)
			}
		}
		if count, err := fixture.checkout.stagedPathCount(t.Context()); err != nil || count != 0 {
			t.Fatalf("refused import staged paths=%d, %v", count, err)
		}
	})
	t.Run("junction parent", func(t *testing.T) {
		fixture := newFixture(t)
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "existing"), []byte("outside"), 0600); err != nil {
			t.Fatal(err)
		}
		parent := filepath.Join(fixture.checkout.path, "linked")
		if err := wintest.CreateJunction(parent, outside); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"linked/existing", "linked/new"} {
			change := []workspace.Change{{Path: path, Mode: workspace.Regular, Data: []byte("reviewed")}}
			if err := fixture.checkout.Import(t.Context(), fixture.base, change); err == nil {
				t.Fatalf("junction import %q accepted", path)
			}
		}
		if data, err := os.ReadFile(filepath.Join(outside, "existing")); err != nil || string(data) != "outside" {
			t.Fatalf("outside existing file changed: %q, %v", data, err)
		}
		if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
			t.Fatalf("outside new file created: %v", err)
		}
		if count, err := fixture.checkout.stagedPathCount(t.Context()); err != nil || count != 0 {
			t.Fatalf("refused junction import staged paths=%d, %v", count, err)
		}
	})
}
