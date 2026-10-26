//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

// The test executable doubles as the public execute-mode child and a native
// Codex probe. This keeps the Windows acceptance path free of shell wrappers.
func TestMain(m *testing.M) {
	if strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") == "codex" {
		windowsModeCodexProbe()
		return
	}
	if os.Getenv("MINION_WINDOWS_EXECUTE_CHILD") == "1" && len(os.Args) == 2 && os.Args[1] == "-mode=execute" {
		flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
		os.Exit(run())
	}
	os.Exit(m.Run())
}

func windowsModeCodexProbe() {
	if len(os.Args) < 2 {
		os.Exit(99)
	}
	switch os.Args[1] {
	case "login":
		return
	case "debug":
		text := "MINION_TRUSTED_PROMPT_78"
		if strings.Contains(strings.Join(os.Args, " "), "skills.include_instructions=true") {
			text += " MINION_UNTRUSTED_AGENTS_78 MINION_UNTRUSTED_SKILL_78"
		}
		_ = json.NewEncoder(os.Stdout).Encode([]any{map[string]any{"role": "user", "content": []any{map[string]string{"text": text}}}})
	case "app-server":
		if strings.Contains(strings.Join(os.Args, " "), "minion_unsupported_capability_probe=true") {
			fmt.Fprintln(os.Stderr, "unknown config key: minion_unsupported_capability_probe")
			os.Exit(2)
		}
		decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
		for {
			var request struct {
				ID     *int   `json:"id"`
				Method string `json:"method"`
			}
			if decoder.Decode(&request) != nil {
				return
			}
			if request.ID == nil {
				continue
			}
			var result any = map[string]any{}
			if request.Method == "model/list" {
				result = map[string]any{"data": []any{map[string]any{"model": "main-1", "isDefault": true, "defaultReasoningEffort": "high", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}}}}}
			}
			_ = encoder.Encode(map[string]any{"id": *request.ID, "result": result})
			if request.Method == "model/list" {
				_, _ = io.Copy(io.Discard, os.Stdin)
				return
			}
		}
	case "exec":
		for _, argument := range os.Args[2:] {
			if argument == "--minion-unsupported-capability-probe" {
				fmt.Fprintln(os.Stderr, "unexpected argument --minion-unsupported-capability-probe")
				os.Exit(2)
			}
			if argument == "--help" {
				fmt.Println("Usage: codex exec [OPTIONS] [PROMPT]")
				return
			}
		}
	}
	os.Exit(99) // This fixture never starts a paid model call.
}

func TestWindowsPublicExecuteModeUsesSelectedHTTPSWorker(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex.exe"), data, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("MINION_WINDOWS_EXECUTE_CHILD", "1")
	cfg := selectedServiceTestConfig(t)
	cfg.Codex.APIKey = ""
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	owner := newOrchestrator(cfg, nil, nil, logger, nil)
	watcher := &monitor.Monitor{Config: cfg}
	now := time.Now().UTC()
	store := &state.Store{Data: state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)}}
	if code := runOrchestrateMode(t.Context(), owner, watcher, store, false, logger, binary, 40*time.Second); code != 0 {
		t.Fatalf("public orchestrate/execute mode returned %d: %s", code, logs.String())
	}
	if watcher.Handler != owner || !watcher.SelectedCommands["comment"] || watcher.SelectedCommands["review"] {
		t.Fatalf("selected child did not hold an authenticated pull: handler=%v commands=%v", watcher.Handler, watcher.SelectedCommands)
	}
}
