package testsupport

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
)

// LinkTestExecutable gives a native helper the current test binary's contents.
func LinkTestExecutable(t *testing.T, path string) {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	linkErr := os.Link(source, path)
	if linkErr == nil {
		return
	}
	from, err := os.Open(source)
	if err != nil {
		t.Fatal(errors.Join(linkErr, err))
	}
	defer from.Close()
	to, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		t.Fatal(errors.Join(linkErr, err))
	}
	_, copyErr := io.Copy(to, from)
	if err := errors.Join(copyErr, to.Close()); err != nil {
		t.Fatal(errors.Join(linkErr, err))
	}
}

// FakeOnPath installs a complete script and returns its temporary bin directory.
func FakeOnPath(t *testing.T, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture is unavailable on native Windows")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

// GitEnvironment isolates scratch paths and disables ambient Git configuration.
func GitEnvironment(scratch string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + scratch, "TMPDIR=" + scratch, "LC_ALL=C"}, aicli.ModelGitEnvironment()...)
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
	cmd.Env = GitEnvironment(scratch)
	return cmd.CombinedOutput()
}
