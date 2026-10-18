package aicli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/workspace"
)

// SkillInstructions tells the model to read the configured trusted skill files.
func SkillInstructions(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var text strings.Builder
	text.WriteString("Use these configured skills for this task. Read each SKILL.md before work and follow its instructions:\n")
	for _, path := range paths {
		fmt.Fprintf(&text, "- %q\n", path)
	}
	return text.String() + "\n"
}

// MaterializeSkills expands built-in skills beside the canonical source root and refuses source-owned skills.
// The caller owns embeddedDir, including on error, and must check its cleanup.
func MaterializeSkills(source string, selected []string) (paths []string, embeddedDir string, err error) {
	for _, skill := range selected {
		if content, builtin := skills.BuiltinContent(skill); builtin {
			name, err := skills.Name(skill)
			if err != nil {
				return nil, embeddedDir, err
			}
			if embeddedDir == "" {
				embeddedDir, err = os.MkdirTemp(filepath.Dir(source), ".minion-skills-")
				if err != nil {
					return nil, embeddedDir, err
				}
			}
			skill = filepath.Join(embeddedDir, name, "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
				return nil, embeddedDir, err
			}
			if err := os.WriteFile(skill, content, 0600); err != nil {
				return nil, embeddedDir, err
			}
		}
		if !filepath.IsAbs(skill) || filepath.Base(skill) != "SKILL.md" {
			return nil, embeddedDir, fmt.Errorf("skill %q must be an absolute SKILL.md path", skill)
		}
		resolved, err := filepath.EvalSymlinks(skill)
		if err != nil {
			return nil, embeddedDir, err
		}
		if workspace.Within(source, resolved) {
			return nil, embeddedDir, fmt.Errorf("skill is inside the untrusted source snapshot: %s", skill)
		}
		paths = append(paths, resolved)
	}
	return paths, embeddedDir, nil
}
