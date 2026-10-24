package aicli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
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
			var expectedExternal string
			if invalid {
				external = "relative/SKILL.md"
			} else {
				if err := os.WriteFile(external, []byte("trusted skill"), 0600); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(t.TempDir(), "skill-alias")
				symlinkOrSkip(t, filepath.Dir(external), alias)
				external = filepath.Join(alias, "SKILL.md")
				var err error
				expectedExternal, err = filepath.EvalSymlinks(external)
				if err != nil {
					t.Fatal(err)
				}
			}
			paths, owned, err := MaterializeSkills(source, []string{skills.BuiltinAntiPromptInjection, external})
			if (err != nil) != invalid || owned == "" {
				t.Fatalf("owned=%q err=%v", owned, err)
			}
			if !invalid && (len(paths) != 2 || paths[1] != expectedExternal) {
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
				if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
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

func TestMaterializeSkillsRefusesResolvedSourcePaths(t *testing.T) {
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(source, "SKILL.md")
	if err := os.WriteFile(inside, []byte("untrusted instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "source-alias")
	symlinkOrSkip(t, source, alias)
	for _, selected := range []string{inside, filepath.Join(alias, "SKILL.md")} {
		paths, owned, err := MaterializeSkills(source, []string{selected})
		if err == nil || !strings.Contains(err.Error(), "inside the untrusted source snapshot") || len(paths) != 0 || owned != "" {
			t.Fatalf("source skill %q: paths=%v owned=%q err=%v", selected, paths, owned, err)
		}
	}
}
