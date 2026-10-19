package codexcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

// The test binary is a multicall Codex stub when production sets argv[0] to codex.
func TestMain(m *testing.M) {
	if os.Args[0] != "codex" {
		os.Exit(m.Run())
	}
	switch os.Args[1] {
	case "exec":
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"shim answer"}}`)
		fmt.Println(`{"type":"turn.completed"}`)
	case "debug":
		text := "MINION_TRUSTED_PROMPT_78"
		if _, err := os.Stat(".minion-codex-root"); err != nil {
			text += " MINION_UNTRUSTED_AGENTS_78 MINION_UNTRUSTED_SKILL_78"
		}
		_ = json.NewEncoder(os.Stdout).Encode([]any{map[string]any{"role": "user", "content": []any{map[string]string{"text": text}}}})
	case "app-server":
		decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
		for {
			var request struct {
				ID     *int
				Method string
			}
			if decoder.Decode(&request) != nil {
				return
			}
			if request.ID == nil {
				continue
			}
			var result any = map[string]any{}
			if request.Method == "model/list" {
				result = map[string]any{"data": []any{map[string]any{"model": "fixture", "isDefault": true, "defaultReasoningEffort": "high", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}}}}}
			}
			_ = encoder.Encode(map[string]any{"id": *request.ID, "result": result})
			if request.Method == "model/list" {
				_, _ = io.Copy(io.Discard, os.Stdin)
				return
			}
		}
	default:
		os.Exit(8)
	}
}

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

func TestCanonicalExecutablePreservesMulticallNameInRunAndPreflight(t *testing.T) {
	root := t.TempDir()
	target, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bin, "codex")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", t.TempDir())
	if result, err := Run(context.Background(), t.TempDir(), "inert shim fixture", false, "", config.CodexConfig{}); err != nil || result.Answer != "shim answer" {
		t.Fatalf("multicall Run: %+v %v", result, err)
	}
	if settings, err := Validate(context.Background(), config.CodexConfig{}); err != nil || settings.Model != "fixture" {
		t.Fatalf("multicall model preflight: %+v %v", settings, err)
	}
	if err := ValidateInstructionBoundary(context.Background()); err != nil {
		t.Fatalf("multicall instruction preflight: %v", err)
	}
}
