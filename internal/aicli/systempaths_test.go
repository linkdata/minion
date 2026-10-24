package aicli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestResolveSystemReadPaths(t *testing.T) {
	required := t.TempDir()
	missing := filepath.Join(required, "missing")
	file := filepath.Join(required, "system-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSystemReadPaths([]string{required, missing, file})
	if err != nil {
		t.Fatal(err)
	}
	wantRequired, err := filepath.EvalSymlinks(required)
	if err != nil {
		t.Fatal(err)
	}
	wantFile, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{wantRequired, wantFile}) {
		t.Fatalf("resolved paths = %q, want required and existing optional path", got)
	}
	if _, err := ResolveSystemReadPaths([]string{missing, required}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing required path: got %v, want os.ErrNotExist", err)
	}
	if _, err := ResolveSystemReadPaths([]string{required, "\x00"}); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid optional path was omitted: %v", err)
	}
	t.Run("symlinks", func(t *testing.T) {
		alias := filepath.Join(required, "alias")
		if err := os.Symlink(file, alias); err != nil {
			t.Skipf("symlink fixture unavailable: %v", err)
		}
		got, err := ResolveSystemReadPaths([]string{required, alias})
		if err != nil || !slices.Equal(got, []string{wantRequired, wantFile}) {
			t.Fatalf("symlink grant = %q, %v; want canonical file", got, err)
		}
		dangling := filepath.Join(required, "dangling")
		if err := os.Symlink(missing, dangling); err != nil {
			t.Fatal(err)
		}
		got, err = ResolveSystemReadPaths([]string{required, dangling})
		if err != nil || !slices.Equal(got, []string{wantRequired}) {
			t.Fatalf("dangling optional grant = %q, %v; want only required path", got, err)
		}
	})
}
