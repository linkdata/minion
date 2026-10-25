package codexcli

import "github.com/linkdata/minion/internal/aicli"

// HomePath selects the native home without requiring it to exist.
// Without CODEX_HOME, an unavailable OS user home is refused.
func HomePath() (string, error) {
	return aicli.HomePath("CODEX_HOME", ".codex")
}
