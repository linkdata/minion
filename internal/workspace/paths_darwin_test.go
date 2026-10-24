//go:build darwin

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWithinCaseInsensitiveFilesystemAlias(t *testing.T) {
	parent := t.TempDir()
	grant := filepath.Join(parent, "Grant")
	if err := os.Mkdir(grant, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "gRaNt")
	grantInfo, err := os.Stat(grant)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("case-sensitive volume")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(grantInfo, aliasInfo) {
		t.Skip("case-sensitive volume")
	}
	if rel, err := filepath.Rel(grant, alias); err != nil || filepath.IsLocal(rel) {
		t.Fatalf("fixture did not exercise case-sensitive filepath.Rel: %q, %v", rel, err)
	}
	for _, name := range []string{alias, filepath.Join(alias, "future", "auth.json")} {
		if !Within(grant, name) {
			t.Fatalf("filesystem alias escaped grant: %q", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(grant, "future")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("containment created the future credential directory: %v", err)
	}
	if Within(grant, filepath.Join(parent, "gRaNt-sibling")) {
		t.Fatal("sibling counted as inside grant")
	}
}

func TestWithinAbsentDarwinGrant(t *testing.T) {
	parent := t.TempDir()
	if Within(filepath.Join(parent, "absent-grant"), filepath.Join(parent, "unrelated")) {
		t.Fatal("unrelated path counted inside an absent grant")
	}
}
