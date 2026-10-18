package claudecli

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/workspace"
)

// authentication keeps the key outside every tool-readable directory. A fresh
// config directory also prevents stored gateway credentials overriding the key.
func authentication(parent, key string) (dir string, err error) {
	if key == "" {
		return
	}
	// Sandbox grant checks compare canonical absolute paths.
	if parent, err = workspace.CanonicalPath(parent); err != nil {
		return
	}
	if dir, err = os.MkdirTemp(parent, ".minion-claude-auth-"); err == nil {
		if err = os.WriteFile(filepath.Join(dir, "api-key"), []byte(key), 0o600); err == nil {
			return
		}
		err = errors.Join(err, os.RemoveAll(dir))
		dir = ""
	}
	return
}

func apiKeyHelper(dir string) string {
	return "/bin/cat -- " + aicli.ShellQuote(filepath.Join(dir, "api-key"))
}
