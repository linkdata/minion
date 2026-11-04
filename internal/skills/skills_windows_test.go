//go:build windows

package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/linkdata/minion/internal/wintest"
	"github.com/linkdata/minion/internal/workspace"
)

func TestUserHomeResolvesJunctionWindows(t *testing.T) {
	physical := t.TempDir()
	alias := filepath.Join(t.TempDir(), "home")
	if err := wintest.CreateJunction(alias, physical); err != nil {
		t.Fatal(err)
	}
	t.Setenv("USERPROFILE", alias)
	want, err := workspace.CanonicalPath(physical)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := userHome(); err != nil || got != want {
		t.Fatalf("home=%q, %v; want %q", got, err, want)
	}
	if got, err := os.UserHomeDir(); err != nil || got != alias {
		t.Fatalf("junction fixture did not select USERPROFILE: %q, %v", got, err)
	}
}
