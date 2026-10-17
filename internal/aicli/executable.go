package aicli

import (
	"errors"
	"os/exec"
	"path/filepath"

	"github.com/linkdata/minion/internal/workspace"
)

// ExecutablePath resolves a PATH command to its canonical absolute host file.
// Callers that require a logical argv[0] must set it separately.
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

// ExecutableOutside excludes an untrusted snapshot from executable lookup.
func ExecutableOutside(name, root string) (string, error) {
	path, err := ExecutablePath(name)
	if err == nil && workspace.Within(root, path) {
		err = errors.New("executable is inside the untrusted source snapshot")
	}
	return path, err
}
