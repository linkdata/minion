package testsupport

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
)

// FakeOnPath installs a complete script and returns its temporary bin directory.
func FakeOnPath(t *testing.T, name, script string) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

// ModelGit uses plain Git with isolated scratch paths and the model's Git environment.
func ModelGit(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	binary, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + scratch, "TMPDIR=" + scratch, "LC_ALL=C"}, aicli.ModelGitEnvironment()...)
	return cmd.CombinedOutput()
}
