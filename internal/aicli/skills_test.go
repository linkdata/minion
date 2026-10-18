package aicli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/skills"
)

func TestSkillInstructionsQuotesPaths(t *testing.T) {
	if got := SkillInstructions(nil); got != "" {
		t.Fatalf("empty skills produced instructions: %q", got)
	}
	path := "/trusted/line\n\"quoted\"/SKILL.md"
	got := SkillInstructions([]string{path})
	if !strings.Contains(got, "- "+strconv.Quote(path)+"\n") || strings.Contains(got, path) {
		t.Fatalf("skill path was not quoted: %q", got)
	}
}

func TestMaterializeSkillsKeepsPartialCleanupOwnership(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "mixed paths", true: "partial error"}[invalid], func(t *testing.T) {
			parent := t.TempDir()
			source := filepath.Join(parent, "source")
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
			external := filepath.Join(t.TempDir(), "SKILL.md")
			if invalid {
				external = "relative/SKILL.md"
			} else if err := os.WriteFile(external, []byte("trusted skill"), 0600); err != nil {
				t.Fatal(err)
			}
			paths, owned, err := MaterializeSkills(source, []string{skills.BuiltinAntiPromptInjection, external})
			if (err != nil) != invalid || owned == "" {
				t.Fatalf("owned=%q err=%v", owned, err)
			}
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
			if err != nil || len(entries) != 1 || entries[0].Name() != "source" || !entries[0].IsDir() {
				t.Fatalf("caller cleanup failed: %v %v", entries, err)
			}
		})
	}
}
