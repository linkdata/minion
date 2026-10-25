package codexcli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/linkdata/minion/internal/aicli"
)

// HomePath selects the native home without requiring it to exist.
// Without CODEX_HOME, an unavailable OS user home is refused.
func HomePath() (string, error) {
	return aicli.HomePath("CODEX_HOME", ".codex")
}

// Codex loads these files independently of --ignore-user-config and project_doc_max_bytes.
func rejectGlobalInstructions(home string) error {
	for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
		if _, err := os.Lstat(filepath.Join(home, name)); err == nil {
			return fmt.Errorf("Codex home contains %s; select an instruction-free CODEX_HOME", name) //nolint:staticcheck // -- Codex is a proper noun.
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect Codex home %s: %w", name, err) //nolint:staticcheck // -- Codex is a proper noun.
		}
	}
	return nil
}
