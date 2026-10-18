package aicli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHomePathKeepsNativeSelectionAndMissingDirectories(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("MINION_TEST_AI_HOME", "")
	if got, err := HomePath("MINION_TEST_AI_HOME", ".fixture"); err != nil || got != filepath.Join(root, ".fixture") {
		t.Fatalf("default native home=%q, %v", got, err)
	}
	t.Setenv("HOME", "")
	if got, err := HomePath("MINION_TEST_AI_HOME", ".fixture"); err != nil || got != "" {
		t.Fatalf("guessed an unavailable native home=%q, %v", got, err)
	}
	target := filepath.Join(root, "real", "child")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINION_TEST_AI_HOME", alias+"/../not-created")
	want := filepath.Join(root, "real", "not-created")
	if got, err := HomePath("MINION_TEST_AI_HOME", ".fixture"); err != nil || got != want {
		t.Fatalf("native symlink/.. home=%q, %v; want %q", got, err, want)
	}
	if _, err := os.Stat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("home resolution created a directory: %v", err)
	}
}
