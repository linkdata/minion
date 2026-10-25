package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/workroot"
)

func isolateHostPaths(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		t.Setenv("HOMEDRIVE", "")
		t.Setenv("HOMEPATH", "")
	}
	t.Setenv("PATH", t.TempDir())
	for _, key := range []string{"XDG_CONFIG_HOME", "GH_CONFIG_DIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG"} {
		t.Setenv(key, "")
	}
}

func TestWorkRootPreflightAllowsOrdinaryDirectory(t *testing.T) {
	if err := checkWorkRootPath(t.TempDir()); err != nil {
		t.Fatalf("ordinary work directory was refused: %v", err)
	}
}

func TestWorkRootPreflightRequiresWorkspaceBackend(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := checkWorkRootPath(file)
	if !errors.Is(err, workroot.ErrUnsafe) || runtime.GOOS == "windows" && !strings.Contains(err.Error(), "not a plain directory") || runtime.GOOS != "windows" && !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("startup did not enforce the workspace backend: %v", err)
	}
}

func TestWorkRootPreflightPreservesMissingPathCause(t *testing.T) {
	err := checkWorkRootPath(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, workroot.ErrUnsafe) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing work root lost safety classification or cause: %v", err)
	}
}
