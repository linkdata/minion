package action

import (
	"fmt"

	"github.com/linkdata/minion/internal/skills"
)

// ConfigureRole resolves the execution host's installed skill grants.
func (r *Runner) ConfigureRole(role RoleSettings) error {
	provider := role.Provider
	var selected []string
	seen := map[string]bool{}
	for _, source := range role.Skills {
		path, installed, err := skills.InstalledPathFor(source, provider)
		if err != nil {
			return fmt.Errorf("%s skill: %w", provider, err)
		}
		if !installed {
			return fmt.Errorf("%s skill is not installed: %s", provider, source)
		}
		if !seen[path] {
			selected = append(selected, path)
			seen[path] = true
		}
	}
	for _, source := range role.AlwaysOnSkills {
		path, installed, err := skills.InstalledPathFor(source, provider)
		if err != nil {
			r.logf("skip unavailable %s always-on skill %s: %v", provider, source, err)
			continue
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
