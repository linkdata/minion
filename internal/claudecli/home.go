package claudecli

import (
	"os"

	"github.com/linkdata/minion/internal/aicli"
)

// HomePath selects the stored native configuration directory, allowing a missing
// directory. Without CLAUDE_CONFIG_DIR, an unavailable OS user home is refused.
func HomePath() (string, error) {
	return aicli.HomePath("CLAUDE_CONFIG_DIR", ".claude")
}

func configDirectory(authDir string) (string, string, error) {
	if authDir != "" {
		return authDir, "", nil
	}
	home, err := HomePath()
	if err != nil {
		return "", "", err
	}
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		return home, home, nil
	}
	return "", home, nil
}
