package codexcli

import (
	"os/exec"
	"path/filepath"
)

// ExecutablePath resolves the exact host file granted to sandboxed commands.
func ExecutablePath() (string, error) {
	binary, err := exec.LookPath("codex")
	if err != nil {
		return "", err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(binary)
}
