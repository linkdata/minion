package aicli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/workspace"
)

// ValidateModelGitAccess checks the selected host Git against a provider's model-command grants.
func ValidateModelGitAccess(ctx context.Context, provider string, systemReads []string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	binary, err := AbsoluteExecutable("git")
	if err != nil {
		return fmt.Errorf("resolve Git for %s model commands: %w", provider, err)
	}
	target, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return fmt.Errorf("resolve Git target for %s model commands: %w", provider, err)
	}
	roots, err := ResolveSystemReadPaths(systemReads)
	if err != nil {
		return fmt.Errorf("resolve %s model read grants: %w", provider, err)
	}
	// The model environment removes inherited Git overrides. Probe that same
	// default helper location, since Git for Windows may launch it separately.
	var env []string
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(strings.ToUpper(key), "GIT_") || strings.EqualFold(key, "PATH") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "PATH="+SafePath(os.Getenv("PATH")))
	env = append(env, ModelGitEnvironment()...)
	output, err := Probe(ctx, binary, "", []string{"--exec-path"}, filepath.Dir(binary), env, "", "")
	if err != nil {
		return fmt.Errorf("locate Git helpers for %s model commands: %w", provider, err)
	}
	execPath := strings.TrimRight(string(output), "\r\n")
	if !filepath.IsAbs(execPath) {
		return fmt.Errorf("Git helper path %q is not absolute", execPath)
	}
	execPath, err = ResolvePath(execPath)
	if err != nil {
		return fmt.Errorf("resolve Git helper path: %w", err)
	}
	// Model PATH resolves directory aliases, but leaves the executable name intact.
	lookupDir, err := filepath.EvalSymlinks(filepath.Dir(binary))
	if err != nil {
		return fmt.Errorf("resolve Git lookup directory for %s model commands: %w", provider, err)
	}
	if !gitPathReadable(filepath.Join(lookupDir, filepath.Base(binary)), roots) {
		return fmt.Errorf("Git path %q is outside %s model read grants; install Git under a granted system path", binary, provider)
	}
	for _, path := range []string{target, execPath} {
		if !gitPathReadable(path, roots) {
			return fmt.Errorf("Git path %q is outside %s model read grants; install Git under a granted system path", path, provider)
		}
	}
	return nil
}

func gitPathReadable(path string, roots []string) bool {
	for _, root := range roots {
		if workspace.Within(root, path) {
			return true
		}
	}
	return false
}
