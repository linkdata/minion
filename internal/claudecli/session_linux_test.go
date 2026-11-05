package claudecli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestSessionRuntimeOutsideDeepWorkspace(t *testing.T) {
	tempRoot := testCanonicalTempDirLength(t, maxLinuxTempRootBytes)
	t.Setenv("TMPDIR", tempRoot)
	fakeExecutable(t, "claude", `cat >/dev/null
[ "$TMPDIR" = "$PWD" ] && [ "$CLAUDE_CODE_TMPDIR" = "$PWD/tmp" ] || exit 2
[ ${#PWD} -le `+fmt.Sprint(maxLinuxRuntimeDirBytes)+` ] || exit 3
printf '%s\n' '`+successfulResult+`'
`)
	workspace := filepath.Join(t.TempDir(), strings.Repeat("deep", 25), "source")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := session.Run(t.Context(), workspace, "task", false, "", config.ClaudeConfig{}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRejectsLongTempPathBeforeClaude(t *testing.T) {
	binary := fakeExecutable(t, "claude", `printf called > "$0.called"`)
	workspace := t.TempDir()
	tempRoot := testCanonicalTempDirLength(t, maxLinuxTempRootBytes+1)
	wantLimit := fmt.Sprintf("canonical path of at most %d bytes", maxLinuxTempRootBytes)
	t.Setenv("TMPDIR", tempRoot)
	if _, err := Validate(t.Context(), config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), wantLimit) || !strings.Contains(err.Error(), tempRoot) {
		t.Fatalf("long temp path passed startup validation: %v", err)
	}
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := session.Run(t.Context(), workspace, "task", false, "", config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), wantLimit) || !strings.Contains(err.Error(), tempRoot) {
		t.Fatalf("long temp path: %v", err)
	}
	if _, err := os.Stat(binary + ".called"); !os.IsNotExist(err) {
		t.Fatalf("Claude started despite long socket path: %v", err)
	}
	if session.runtimeRoot != "" {
		t.Fatal("long temp path created Claude scratch")
	}
}

func testCanonicalTempDirLength(t *testing.T, length int) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	padding := length - len(canonical) - 1
	if padding < 1 {
		t.Fatalf("base temp path %q is too long for %d bytes", canonical, length)
	}
	root := filepath.Join(canonical, strings.Repeat("x", padding))
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if len(root) != length {
		t.Fatalf("temp root length = %d, want %d", len(root), length)
	}
	return root
}
