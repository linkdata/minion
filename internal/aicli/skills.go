package aicli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/linkdata/minion/internal/skills"
)

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
