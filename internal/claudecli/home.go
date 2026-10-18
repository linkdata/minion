package claudecli

import (
	"fmt"
	"os"

	"github.com/linkdata/minion/internal/aicli"
)

// HomePath selects the stored native configuration directory, allowing a missing
// directory. An unset HOME without CLAUDE_CONFIG_DIR does not guess a passwd home.
func HomePath() (string, error) {
	path := os.Getenv("CLAUDE_CONFIG_DIR")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil
		}
		path = home + string(os.PathSeparator) + ".claude"
	}
	resolved, err := aicli.ResolvePath(path)
	if err != nil {
		return "", fmt.Errorf("resolve CLAUDE_CONFIG_DIR: %w", err)
	}
	return resolved, nil
}
