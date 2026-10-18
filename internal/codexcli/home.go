package codexcli

import "github.com/linkdata/minion/internal/aicli"

// HomePath selects the native home without requiring it to exist. An unset HOME
// without CODEX_HOME leaves rollout attribution unavailable; no passwd fallback is guessed.
func HomePath() (string, error) {
	return aicli.HomePath("CODEX_HOME", ".codex")
}
