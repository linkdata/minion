package action

import (
	"fmt"
	"slices"

	"github.com/linkdata/minion/internal/skills"
)

// ConfigureRole resolves the execution host's installed skill grants.
func (r *Runner) ConfigureRole(role RoleSettings) error {
	provider := role.Provider
	var selected []string
	seen := map[string]bool{}
	for i, source := range slices.Concat(role.Skills, role.AlwaysOnSkills) {
		required := i < len(role.Skills)
		path, installed, err := skills.InstalledPathFor(source, provider)
		if err != nil {
			if required {
				return fmt.Errorf("%s skill: %w", provider, err)
			}
			r.logf("skip unavailable %s always-on skill %s: %v", provider, source, err)
			continue
		}
		if !installed && required {
			return fmt.Errorf("%s skill is not installed: %s", provider, source)
		}
		if installed && !seen[path] {
			selected = append(selected, path)
			seen[path] = true
		}
	}
	if provider == "codex" {
		r.Codex.Skills = selected
	} else {
		r.Claude.Skills = selected
	}
	return nil
}
