package testsupport

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
)

// FakeOnPath installs a complete script and returns its temporary bin directory.
func FakeOnPath(t *testing.T, name, script string) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

// ShellQuote treats a fixture value as one literal POSIX shell argument.
func ShellQuote(value string) string {
	return aicli.ShellQuote(value)
}
