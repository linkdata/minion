//go:build windows

package aicli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestValidateModelGitAccessChecksJunctionTargetWindows(t *testing.T) {
	git, err := exec.LookPath("git.exe")
	if err != nil {
		t.Fatal(err)
	}
	gitDir, err := workspace.CanonicalPath(filepath.Dir(git))
	if err != nil {
		t.Fatal(err)
	}
	target, err := workspace.CanonicalPath(git)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(git, "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	helper, err := ResolvePath(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatal(err)
	}
	aliases := t.TempDir()
	alias := filepath.Join(aliases, "git-bin")
	makeJunctionWindows(t, alias, gitDir)
	t.Setenv("PATH", alias+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got, err := AbsoluteExecutable("git"); err != nil || got != filepath.Join(alias, "git.exe") {
		t.Fatalf("Git launcher=%q, %v", got, err)
	}
	if err := ValidateModelGitAccess(t.Context(), "Codex", []string{gitDir, filepath.Dir(target), helper}); err != nil {
		t.Fatalf("granted junction target refused: %v", err)
	}
	if err := ValidateModelGitAccess(t.Context(), "Codex", []string{aliases}); err == nil || !strings.Contains(err.Error(), "outside Codex model read grants") {
		t.Fatalf("junction outside grants accepted: %v", err)
	}
}
