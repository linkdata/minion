//go:build !windows

package aicli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafePathExcludesRelativeAndWritableDirectories(t *testing.T) {
	canonical := func(path string) string {
		resolved, err := ResolvePath(path)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	writable := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(writable, alias); err != nil {
		t.Fatal(err)
	}
	trustedAlias := filepath.Join(writable, "trusted-bin")
	if err := os.Symlink("/usr/bin", trustedAlias); err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{"/usr/bin", "", ".", "tools", filepath.Join(writable, "tools"), trustedAlias, filepath.Join(alias, "later"), "/bin"}, string(os.PathListSeparator))
	if got, want := SafePath(input, writable), strings.Join([]string{canonical("/usr/bin"), canonical("/usr/bin"), canonical("/bin")}, string(os.PathListSeparator)); got != want {
		t.Fatalf("SafePath(%q) = %q, want %q", input, got, want)
	}
	if got := SafePath(".:tools"); got != strings.Join([]string{canonical("/usr/local/bin"), canonical("/usr/bin"), canonical("/bin")}, string(os.PathListSeparator)) {
		t.Fatalf("relative-only PATH used current directory: %q", got)
	}
	if got := SafePath(".", "/"); got != os.DevNull {
		t.Fatalf("blocked PATH used executable lookup: %q", got)
	}
}

func TestSafePathPinsSymlinkTraversalOutsideWritableRoot(t *testing.T) {
	writable := t.TempDir()
	trusted := t.TempDir()
	for _, dir := range []string{filepath.Join(trusted, "a", "b"), filepath.Join(trusted, "bin")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(writable, "link")
	if err := os.Symlink(filepath.Join(trusted, "a", "b"), link); err != nil {
		t.Fatal(err)
	}
	input := link + "/../../bin"
	resolved, err := ResolvePath(input)
	if err != nil {
		t.Fatal(err)
	}
	if got := SafePath(input, writable); got != resolved {
		t.Fatalf("SafePath(%q) = %q, want canonical %q", input, got, resolved)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(writable, "a", "b"), filepath.Join(writable, "bin")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(writable, "a", "b"), link); err != nil {
		t.Fatal(err)
	}
	if got := SafePath(input, writable); got != SafePath("", writable) {
		t.Fatalf("retargeted writable alias was accepted: %q", got)
	}
}
