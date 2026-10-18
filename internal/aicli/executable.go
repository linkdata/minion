package aicli

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/linkdata/minion/internal/workspace"
)

// ExecutablePath resolves a PATH command to its canonical absolute host file.
// Callers that require a logical argv[0] must set it separately.
func ExecutablePath(name string) (string, error) {
	binary, err := AbsoluteExecutable(name)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(binary)
}

// AbsoluteExecutable resolves a PATH command without changing its launcher name.
func AbsoluteExecutable(name string) (string, error) {
	binary, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(binary) {
		return "", fmt.Errorf("%s binary must have an absolute path", name)
	}
	return binary, nil
}

// ExecutableOutside excludes an untrusted snapshot from executable lookup.
func ExecutableOutside(name, root string) (string, error) {
	path, err := ExecutablePath(name)
	if err != nil {
		return "", err
	}
	if workspace.Within(root, path) {
		return "", errors.New("executable is inside the untrusted source snapshot")
	}
	return path, nil
}
