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
