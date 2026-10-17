package aicli

import (
	"os/exec"
	"path/filepath"
)

// ExecutablePath resolves a PATH command to its canonical absolute host file.
// Callers preserve the CLI's expected argv[0] separately.
func ExecutablePath(name string) (string, error) {
	binary, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(binary)
}
