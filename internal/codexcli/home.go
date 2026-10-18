package codexcli

import "github.com/linkdata/minion/internal/aicli"

// HomePath selects the native home without requiring it to exist. An unavailable
// HOME without CODEX_HOME is refused rather than guessing a passwd home.
func HomePath() (string, error) {
	return aicli.HomePath("CODEX_HOME", ".codex")
}
