package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/testtemp"
)

// The test binary is a multicall Codex stub when production sets argv[0] to codex.
func TestMain(m *testing.M) {
	if len(os.Args) < 2 || strings.HasPrefix(os.Args[1], "-test.") {
		testtemp.Main(m)
	}
	if os.Args[0] != "codex" {
		fmt.Fprintln(os.Stderr, "codex stub invoked as", os.Args[0])
		os.Exit(7)
	}
	switch os.Args[1] {
	case "exec":
		for _, arg := range os.Args[2:] {
			if arg == "--minion-unsupported-capability-probe" {
				os.Exit(2)
			}
			if arg == "--help" {
				if os.Args[2] == "resume" {
					fmt.Println("Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]")
				} else {
					fmt.Println("Usage: codex exec [OPTIONS] [PROMPT]")
				}
				return
			}
		}
		fmt.Println(`{"type":"thread.started","thread_id":"root"}`)
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"shim answer"}}`)
		fmt.Println(`{"type":"turn.completed"}`)
	case "login":
		return
	case "debug":
		text := "MINION_TRUSTED_PROMPT_78"
		if _, err := os.Stat(".minion-codex-root"); err != nil {
			text += " MINION_UNTRUSTED_AGENTS_78 MINION_UNTRUSTED_SKILL_78"
		}
		_ = json.NewEncoder(os.Stdout).Encode([]any{map[string]any{"role": "user", "content": []any{map[string]string{"text": text}}}})
	case "app-server":
		for _, arg := range os.Args[2:] {
			if arg == "minion_unsupported_capability_probe=true" {
				os.Exit(2)
			}
		}
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

func TestMulticallStubRejectsWrongArgv0(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	// --help makes a broken guard exit the test runner before it can run tests recursively.
	cmd := exec.CommandContext(ctx, binary, "--help")
	cmd.Args[0] = "wrong-codex"
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || !strings.Contains(string(output), "codex stub invoked as wrong-codex") {
		t.Fatalf("wrong argv0 exit = %v, output = %q; want exit 7 and the argv0 diagnostic", err, output)
	}
}

func TestRunInvokesAndGrantsResolvedExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
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
	result, err := Run(t.Context(), t.TempDir(), "Inert executable fixture", false, "", config.CodexConfig{})
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
	if !strings.Contains(string(args), `sandbox_mode="read-only"`) || strings.Contains(string(args), "permissions.minion") {
		t.Fatalf("resolved executable did not use native read-only sandbox: %s", args)
	}
}

func TestCanonicalExecutablePreservesMulticallNameInRunAndPreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink executable fixture")
	}
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
	if result, err := Run(t.Context(), t.TempDir(), "inert shim fixture", false, "", config.CodexConfig{}); err != nil || result.Answer != "shim answer" {
		t.Fatalf("multicall Run: %+v %v", result, err)
	}
	if settings, err := Validate(t.Context(), config.CodexConfig{}); err != nil || settings.Model != "fixture" {
		t.Fatalf("multicall model preflight: %+v %v", settings, err)
	}
	if err := ValidateInstructionBoundary(t.Context()); err != nil {
		t.Fatalf("multicall instruction preflight: %v", err)
	}
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	if err := CheckLogin(t.Context(), config.CodexConfig{}); err != nil {
		t.Fatalf("multicall login preflight: %v", err)
	}
}
