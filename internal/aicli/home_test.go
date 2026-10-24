package aicli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func setNativeHome(t *testing.T, value string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("HOMEDRIVE", "")
		t.Setenv("HOMEPATH", "")
		t.Setenv("USERPROFILE", value)
		return "USERPROFILE"
	}
	t.Setenv("HOME", value)
	return "HOME"
}

func TestHomePathKeepsNativeSelectionAndMissingDirectories(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	homeEnv := setNativeHome(t, root)
	t.Setenv("MINION_TEST_AI_HOME", "")
	if got, err := HomePath("MINION_TEST_AI_HOME", ".fixture"); err != nil || got != filepath.Join(root, ".fixture") {
		t.Fatalf("default native home=%q, %v", got, err)
	}
	t.Setenv(homeEnv, "")
	if got, err := HomePath("MINION_TEST_AI_HOME", ".fixture"); err == nil || got != "" {
		t.Fatalf("guessed an unavailable native home=%q, %v", got, err)
	}
	if err := os.Unsetenv(homeEnv); err != nil {
		t.Fatal(err)
	}
	if got, err := HomePath("MINION_TEST_AI_HOME", ".fixture"); err == nil || got != "" {
		t.Fatalf("guessed an unset native home=%q, %v", got, err)
	}
	target := filepath.Join(root, "real", "child")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Logf("Windows symlink creation unavailable: %v", err)
			return
		}
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

func TestHomePathErrorsIdentifySelectedPath(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "derived", true: "explicit"}[explicit], func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, ".fixture")
			if err := os.Symlink(filepath.Join(root, "missing"), path); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("Windows symlink creation unavailable: %v", err)
				}
				t.Fatal(err)
			}
			setNativeHome(t, root)
			t.Setenv("MINION_TEST_AI_HOME", "")
			if explicit {
				t.Setenv("MINION_TEST_AI_HOME", path)
			}
			got, err := HomePath("MINION_TEST_AI_HOME", ".fixture")
			if got != "" || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), path) {
				t.Fatalf("selected path or cause lost: home=%q error=%v", got, err)
			}
		})
	}
}

func TestHomePathRejectsRelativeDefaultHome(t *testing.T) {
	setNativeHome(t, "relative-home")
	t.Setenv("MINION_TEST_AI_HOME", "")
	if got, err := HomePath("MINION_TEST_AI_HOME", ".fixture"); err == nil || got != "" || !strings.Contains(err.Error(), "native home must be absolute") {
		t.Fatalf("relative default accepted: %q,%v", got, err)
	}
	explicit, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINION_TEST_AI_HOME", explicit)
	if got, err := HomePath("MINION_TEST_AI_HOME", ".fixture"); err != nil || got != explicit {
		t.Fatalf("explicit native directory lost: %q,%v", got, err)
	}
}
