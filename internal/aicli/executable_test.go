package aicli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutablePathCanonicalizesAliasAndPreservesLookupErrors(t *testing.T) {
	bin := t.TempDir()
	target := filepath.Join(t.TempDir(), "real-cli")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bin, "test-cli")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ExecutablePath("test-cli"); err != nil || got != want || !filepath.IsAbs(got) {
		t.Fatalf("canonical executable=%q error=%v want=%q", got, err, want)
	}
	if got, err := AbsoluteExecutable("test-cli"); err != nil || got != filepath.Join(bin, "test-cli") {
		t.Fatalf("launcher alias=%q error=%v", got, err)
	}
	if _, err := ExecutablePath("missing-cli"); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("lookup cause lost: %v", err)
	}
	if got, err := ExecutableOutside("test-cli", filepath.Dir(want)); err == nil || got != "" {
		t.Fatalf("executable inside the untrusted root returned %q, %v", got, err)
	}
	if got, err := ExecutableOutside("test-cli", filepath.Dir(want)+"-other"); err != nil || got != want {
		t.Fatalf("separate executable refused: %q %v", got, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODEBUG", "execerrdot=1")
	t.Setenv("PATH", rel)
	if _, err := ExecutablePath("test-cli"); !errors.Is(err, exec.ErrDot) {
		t.Fatalf("relative PATH command accepted: %v", err)
	}
	t.Setenv("GODEBUG", "execerrdot=0")
	if _, err := ExecutablePath("test-cli"); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("relative PATH accepted with execerrdot disabled: %v", err)
	}
}
