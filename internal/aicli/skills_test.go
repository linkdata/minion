package aicli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/linkdata/minion/internal/skills"
)

func TestMaterializeSkillsKeepsPartialCleanupOwnership(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "mixed paths", true: "partial error"}[invalid], func(t *testing.T) {
			parent := t.TempDir()
			external := filepath.Join(t.TempDir(), "SKILL.md")
			if invalid {
				external = "relative/SKILL.md"
			}
			paths, owned, err := MaterializeSkills(parent, []string{skills.BuiltinAntiPromptInjection, external})
			if (err != nil) != invalid || owned == "" {
				t.Fatalf("owned=%q err=%v", owned, err)
			}
			defer os.RemoveAll(owned)
			if !invalid && (len(paths) != 2 || paths[1] != external) {
				t.Fatalf("paths=%v", paths)
			}
			builtin := filepath.Join(owned, "anti-prompt-injection", "SKILL.md")
			want, _ := skills.BuiltinContent(skills.BuiltinAntiPromptInjection)
			got, err := os.ReadFile(builtin)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("built-in content=%q err=%v", got, err)
			}
			for _, path := range []string{owned, filepath.Dir(builtin), builtin} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm()&0077 != 0 {
					t.Fatalf("nonprivate materialization %q: %v %v", path, info, err)
				}
			}
			if err := os.RemoveAll(owned); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatalf("caller cleanup failed: %v %v", entries, err)
			}
		})
	}
}
