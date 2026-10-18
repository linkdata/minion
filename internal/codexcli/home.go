package codexcli

import (
	"fmt"
	"os"

	"github.com/linkdata/minion/internal/aicli"
)

// HomePath selects the native home without requiring it to exist. An unset HOME
// without CODEX_HOME leaves rollout attribution unavailable; no passwd fallback is guessed.
func HomePath() (string, error) {
	path := os.Getenv("CODEX_HOME")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil
		}
		path = home + string(os.PathSeparator) + ".codex"
	}
	resolved, err := aicli.ResolvePath(path)
	if err != nil {
		return "", fmt.Errorf("resolve CODEX_HOME: %w", err)
	}
	return resolved, nil
}
