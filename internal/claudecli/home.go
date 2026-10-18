package claudecli

import "github.com/linkdata/minion/internal/aicli"

// HomePath selects the stored native configuration directory, allowing a missing
// directory. An unavailable HOME without CLAUDE_CONFIG_DIR is refused.
func HomePath() (string, error) {
	return aicli.HomePath("CLAUDE_CONFIG_DIR", ".claude")
}
