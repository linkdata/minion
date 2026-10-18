package claudecli

import "github.com/linkdata/minion/internal/aicli"

// HomePath selects the stored native configuration directory, allowing a missing
// directory. An unset HOME without CLAUDE_CONFIG_DIR does not guess a passwd home.
func HomePath() (string, error) {
	return aicli.HomePath("CLAUDE_CONFIG_DIR", ".claude")
}
