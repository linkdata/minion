//go:build windows

package aicli

import (
	"path/filepath"
	"testing"
)

func TestResolveSystemReadPathsFollowsNestedJunctionsWindows(t *testing.T) {
	physical, aliases := t.TempDir(), t.TempDir()
	current, visible := filepath.Join(aliases, "current"), filepath.Join(aliases, "visible")
	makeJunctionWindows(t, current, physical)
	makeJunctionWindows(t, visible, current)
	want, err := ResolvePath(physical)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveSystemReadPaths([]string{visible}); err != nil || len(got) != 1 || got[0] != want {
		t.Fatalf("junction grant=%q, %v; want %q", got, err, want)
	}
}
