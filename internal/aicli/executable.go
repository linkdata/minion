package aicli

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/linkdata/minion/internal/workspace"
)

// ExecutablePath resolves a PATH command to its canonical absolute host file.
// Callers that require a logical argv[0] must set it separately.
func ExecutablePath(name string) (string, error) {
	binary, err := AbsoluteExecutable(name)
	if err != nil {
		return "", err
	}
	resolved, err := workspace.CanonicalPath(binary)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(resolved), ".exe") {
		return "", fmt.Errorf("%s needs a native .exe executable", name)
	}
	return resolved, nil
}

// AbsoluteExecutable resolves a PATH command without changing its launcher name.
func AbsoluteExecutable(name string) (string, error) {
	lookup := name
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		lookup += ".exe"
	}
	binary, err := exec.LookPath(lookup)
	if err != nil {
		if lookup != name && errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("%s needs a native .exe executable: %w", name, err)
		}
		return "", err
	}
	if !filepath.IsAbs(binary) {
		return "", fmt.Errorf("%s binary must have an absolute path", name)
	}
	if runtime.GOOS == "windows" {
		resolved, err := workspace.CanonicalPath(binary)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(filepath.Ext(binary), ".exe") || !strings.EqualFold(filepath.Ext(resolved), ".exe") {
			return "", fmt.Errorf("%s needs a native .exe executable", name)
		}
	}
	return binary, nil
}

// ExecutableOutside rejects a resolved executable inside an untrusted snapshot.
// It does not retry PATH after rejecting that target.
func ExecutableOutside(name, root string) (string, error) {
	path, err := ExecutablePath(name)
	if err != nil {
		return "", err
	}
	if workspace.Within(root, path) {
		return "", fmt.Errorf("%s executable %s is inside the untrusted source snapshot", name, path)
	}
	return path, nil
}
