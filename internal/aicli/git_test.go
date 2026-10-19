package aicli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelGitEnvironmentRejectsImplicitBareRepository(t *testing.T) {
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	env := append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "LC_ALL=C"}, ModelGitEnvironment()...)
	run := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
	bare := filepath.Join(root, "bare")
	if output, err := run("init", "--quiet", "--bare", "--template=", bare); err != nil {
		t.Fatalf("init bare: %s: %v", output, err)
	}
	if output, err := run("-C", bare, "rev-parse", "--is-bare-repository"); err == nil || !strings.Contains(output, "bare repository") {
		t.Fatalf("implicitly discovered bare repository was accepted: output=%q err=%v", output, err)
	}
	if output, err := run("--git-dir="+bare, "rev-parse", "--is-bare-repository"); err != nil || strings.TrimSpace(output) != "true" {
		t.Fatalf("explicit bare repository was refused: output=%q err=%v", output, err)
	}
	regular := filepath.Join(root, "regular")
	if output, err := run("init", "--quiet", "--template=", regular); err != nil {
		t.Fatalf("init regular: %s: %v", output, err)
	}
	if output, err := run("-C", regular, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(output) != "true" {
		t.Fatalf("ordinary model repository was refused: output=%q err=%v", output, err)
	}
}
