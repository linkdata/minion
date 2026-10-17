package aicli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/linkdata/minion/internal/skills"
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

// MaterializeSkills expands built-in skills and checks configured SKILL.md paths.
// The caller owns embeddedDir, including on error, and must check its cleanup.
func MaterializeSkills(parent string, selected []string) (paths []string, embeddedDir string, err error) {
	for _, skill := range selected {
		if content, builtin := skills.BuiltinContent(skill); builtin {
			name, err := skills.Name(skill)
			if err != nil {
				return nil, embeddedDir, err
			}
			if embeddedDir == "" {
				embeddedDir, err = os.MkdirTemp(parent, ".minion-skills-")
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
		paths = append(paths, skill)
	}
	return paths, embeddedDir, nil
}
