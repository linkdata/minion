//go:build windows

package aicli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafePathExcludesRelativeAndWritableDirectoriesWindows(t *testing.T) {
	writable, trusted := t.TempDir(), t.TempDir()
	input := strings.Join([]string{".", "relative", writable, filepath.Join(writable, "bin"), trusted}, string(os.PathListSeparator))
	want, err := ResolvePath(trusted)
	if err != nil {
		t.Fatal(err)
	}
	if got := SafePath(input, writable); got != want {
		t.Fatalf("SafePath(%q) = %q, want %q", input, got, want)
	}
	if got := SafePath("relative", writable); got != os.DevNull {
		t.Fatalf("relative-only PATH used command lookup: %q", got)
	}
}

func TestSafePathRejectsWritableJunctionWindows(t *testing.T) {
	writable, trusted, aliases := t.TempDir(), t.TempDir(), t.TempDir()
	current := filepath.Join(aliases, "current")
	visible := filepath.Join(aliases, "visible")
	makeJunctionWindows(t, current, writable)
	makeJunctionWindows(t, visible, current)
	want, err := ResolvePath(trusted)
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{visible, trusted}, string(os.PathListSeparator))
	if got := SafePath(input, writable); got != want {
		t.Fatalf("writable junction PATH=%q, want %q", got, want)
	}
}
