package aicli

import (
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
	if runtime.GOOS == "windows" && (strings.EqualFold(filepath.Ext(binary), ".cmd") || strings.EqualFold(filepath.Ext(binary), ".bat")) {
		return "", fmt.Errorf("%s needs a native executable; batch launchers cannot be run directly", name)
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
