package action

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkRootIsolationRejectsResolvedCodexTarget(t *testing.T) {
	root := t.TempDir()
	work, bin := filepath.Join(root, "work"), filepath.Join(root, "bin")
	for _, directory := range []string{work, bin} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(work, "actual-codex")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bin, "codex")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{}
	if err := runner.CheckWorkRootIsolation(work, "codex"); err == nil || !strings.Contains(err.Error(), "work root overlaps") || !strings.Contains(err.Error(), want) {
		t.Fatalf("outside symlink hid executable inside work root: %v", err)
	}
	if err := runner.CheckWorkRootIsolation(work, "claude"); err != nil {
		t.Fatalf("unused Codex executable affected Claude admission: %v", err)
	}
}
