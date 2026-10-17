package action

import (
	"fmt"

	"github.com/linkdata/minion/internal/skills"
)

// SelectSkills resolves required and optional installed skills using execution's grant rules.
func (r *Runner) SelectSkills(provider string, required, alwaysOn []string) ([]string, error) {
	var selected []string
	seen := map[string]bool{}
	for _, source := range required {
		path, installed, err := skills.InstalledPathFor(source, provider)
		if err != nil {
			return nil, fmt.Errorf("%s skill: %w", provider, err)
		}
		if !installed {
			return nil, fmt.Errorf("%s skill is not installed: %s", provider, source)
		}
		if !seen[path] {
			selected = append(selected, path)
			seen[path] = true
		}
	}
	for _, source := range alwaysOn {
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
	return selected, nil
}
