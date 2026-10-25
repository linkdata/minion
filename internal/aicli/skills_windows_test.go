//go:build windows

package aicli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestMaterializeSkillsRejectsSourceJunctionWindows(t *testing.T) {
	source, err := workspace.CanonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("untrusted"), 0600); err != nil {
		t.Fatal(err)
	}
	aliases := t.TempDir()
	current, visible := filepath.Join(aliases, "current"), filepath.Join(aliases, "visible")
	makeJunctionWindows(t, current, source)
	makeJunctionWindows(t, visible, current)
	if paths, owned, err := MaterializeSkills(source, []string{filepath.Join(visible, "SKILL.md")}); err == nil || !strings.Contains(err.Error(), "inside the untrusted source snapshot") || len(paths) != 0 || owned != "" {
		t.Fatalf("source junction accepted: paths=%v owned=%q error=%v", paths, owned, err)
	}
}
