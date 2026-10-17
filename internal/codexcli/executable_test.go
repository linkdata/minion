package codexcli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestRunInvokesAndGrantsResolvedExecutable(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "actual-codex")
	script := `#!/bin/sh
printf '%s\n' "$0" > "$0.invoked"
printf '%s\n' "$@" > "$0.args"
printf '%s\n' '{"type":"thread.started","thread_id":"symlink-test"}' '{"type":"item.completed","item":{"type":"agent_message","text":"resolved executable"}}' '{"type":"turn.completed"}'
`
	if err := os.WriteFile(target, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(bin, "codex")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", t.TempDir())
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ExecutablePath()
	if err != nil || resolved != want {
		t.Fatalf("resolved executable=%q, %v; want %q", resolved, err, want)
	}
	result, err := Run(context.Background(), t.TempDir(), "Inert executable fixture", false, "", config.CodexConfig{})
	if err != nil || result.Answer != "resolved executable" {
		t.Fatalf("stub execution=%+v, %v", result, err)
	}
	invoked, err := os.ReadFile(target + ".invoked")
	if err != nil || strings.TrimSpace(string(invoked)) != want {
		t.Fatalf("invoked path=%q, %v; want resolved file %q", invoked, err, want)
	}
	args, err := os.ReadFile(target + ".args")
	if err != nil {
		t.Fatal(err)
	}
	var filesystem string
	for _, arg := range strings.Split(string(args), "\n") {
		if strings.HasPrefix(arg, "permissions.minion.filesystem=") {
			filesystem = arg
			break
		}
	}
	if !strings.Contains(filesystem, strconv.Quote(want)+`="read"`) || strings.Contains(filesystem, strconv.Quote(alias)+`="read"`) {
		t.Fatalf("sandbox grants do not match invoked resolved file: %s", filesystem)
	}
}
