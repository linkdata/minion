package codexcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
)

func TestWindowsSessionRunWithNativeFakeCodex(t *testing.T) {
	// TestMain turns the copied Go test binary into an inert Codex JSONL stub.
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex.exe"), data, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", t.TempDir())

	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result, err := session.Run(ctx, t.TempDir(), "inert native Windows run", false, "", config.CodexConfig{})
	if err != nil || result.Answer != "shim answer" || session.threadID != "root" {
		t.Fatalf("native Windows Session.Run: result=%+v thread=%q err=%v", result, session.threadID, err)
	}
}
