//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/workspace"
)

// The test executable doubles as the public execute-mode child and a native
// Codex or Git fixture. This keeps the Windows acceptance path free of shell wrappers.
func TestMain(m *testing.M) {
	switch strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") {
	case "codex":
		windowsModeCodexProbe()
		return
	case "git":
		windowsModeGitProxy()
		return
	}
	if os.Getenv("MINION_WINDOWS_EXECUTE_CHILD") == "1" && len(os.Args) == 2 && os.Args[1] == "-mode=execute" {
		flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
		os.Exit(run())
	}
	os.Exit(m.Run())
}

type windowsModeGitProxyConfig struct {
	RealGit string `json:"real_git"`
	Rewrite string `json:"rewrite"`
}

func windowsModeGitProxy() {
	binary, err := os.Executable()
	if err != nil {
		os.Exit(99)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(binary), "git-proxy.json"))
	if err != nil {
		os.Exit(99)
	}
	var proxy windowsModeGitProxyConfig
	if json.Unmarshal(data, &proxy) != nil || proxy.RealGit == "" || proxy.Rewrite == "" {
		os.Exit(99)
	}
	args := append([]string{"-c", "protocol.https.allow=never", "-c", "protocol.http.allow=never", "-c", "protocol.ssh.allow=never", "-c", "protocol.git.allow=never", "-c", "include.path=" + filepath.ToSlash(proxy.Rewrite)}, os.Args[1:]...)
	cmd := exec.Command(proxy.RealGit, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
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
		return
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
				result = map[string]any{"data": []any{map[string]any{"model": "main-1", "isDefault": true, "defaultReasoningEffort": "high", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}, map[string]any{"reasoningEffort": "low"}}}}}
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
				if len(os.Args) > 2 && os.Args[2] == "resume" {
					fmt.Println("Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]")
				} else {
					fmt.Println("Usage: codex exec [OPTIONS] [PROMPT]")
				}
				return
			}
		}
		binary, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(99)
		}
		calls, err := os.OpenFile(filepath.Join(filepath.Dir(binary), "model-calls"), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil { // An unarmed fixture must not stand in for a model call.
			os.Exit(99)
		}
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(99)
		}
		answer := "No findings."
		for _, argument := range os.Args[2:] {
			roots, ok := strings.CutPrefix(argument, "sandbox_workspace_write.writable_roots=")
			if !ok {
				continue
			}
			var writable []string
			if json.Unmarshal([]byte(roots), &writable) != nil || len(writable) != 1 {
				os.Exit(99)
			}
			path := filepath.Join(writable[0], "source.txt")
			if data, err := os.ReadFile(path); err != nil || string(data) != "source\n" {
				os.Exit(99)
			}
			if err := os.WriteFile(path, []byte("fixed\n"), 0600); err != nil {
				os.Exit(99)
			}
			answer = "Fixed source."
		}
		if _, err := io.WriteString(calls, "model\n"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(99)
		}
		if err := calls.Close(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(99)
		}
		fmt.Println(`{"type":"thread.started","thread_id":"root"}`)
		fmt.Printf("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":%q}}\n", answer)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":2}}`)
		return
	}
	os.Exit(99) // This fixture never starts a paid model call.
}

func offlineModeWorkerPath(t *testing.T, binary string) string {
	t.Helper()
	t.Setenv("MINION_WINDOWS_EXECUTE_CHILD", "1")
	return binary
}

func offlineModeGitProxy(t *testing.T, realGit, rewrite string) {
	t.Helper()
	bin := t.TempDir()
	testsupport.LinkTestExecutable(t, filepath.Join(bin, "git.exe"))
	// Git readiness strips fixture environment variables before calling git.exe.
	data, err := json.Marshal(windowsModeGitProxyConfig{RealGit: realGit, Rewrite: rewrite})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git-proxy.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func offlineModeCodex(t *testing.T, modelCalls string) {
	t.Helper()
	bin := filepath.Dir(modelCalls)
	testsupport.LinkTestExecutable(t, filepath.Join(bin, "codex.exe"))
	if err := os.WriteFile(modelCalls, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWindowsPublicExecuteModeUsesSelectedHTTPSWorker(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	testsupport.LinkTestExecutable(t, filepath.Join(bin, "codex.exe"))
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
	store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
	if code := runOrchestrateMode(t.Context(), owner, watcher, store, false, logger, binary, 40*time.Second); code != 0 {
		t.Fatalf("public orchestrate/execute mode returned %d: %s", code, logs.String())
	}
	if watcher.Handler != owner || !watcher.SelectedCommands["comment"] || watcher.SelectedCommands["review"] {
		t.Fatalf("selected child did not hold an authenticated pull: handler=%v commands=%v", watcher.Handler, watcher.SelectedCommands)
	}
}

type windowsAcceptCheckpoints struct{}

func (windowsAcceptCheckpoints) BeforeCall(action.CallCheckpoint) error { return nil }
func (windowsAcceptCheckpoints) AfterCall(action.CallCheckpoint) error  { return nil }
func (windowsAcceptCheckpoints) Review(action.ReviewCheckpoint) error   { return nil }

func TestWindowsPublicExecuteModeReturnsSelectedFix(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	modelCalls := filepath.Join(bin, "model-calls")
	offlineModeCodex(t, modelCalls)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("MINION_WINDOWS_EXECUTE_CHILD", "1")
	for _, name := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_API_KEY"} {
		t.Setenv(name, "")
	}
	cfg := selectedServiceTestConfig(t)
	cfg.Commands = map[string]config.CommandMapping{
		"fix": {Action: "fix", Skills: []string{skills.BuiltinAntiPromptInjection}},
	}
	cfg.Repositories[0].Actions = []string{"fix"}
	cfg.Codex.APIKey = ""
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	owner := newOrchestrator(cfg, nil, nil, logger, nil)
	watcher := &monitor.Monitor{Config: cfg}
	done, closeWorker, err := attachSelectedLocalWorker(t.Context(), owner, watcher, binary, []string{"-mode=execute"}, 40*time.Second)
	if err != nil {
		t.Fatalf("selected worker startup: %v; logs=%s", err, logs.String())
	}
	t.Cleanup(func() { _ = closeWorker() })
	if watcher.Handler != owner || !watcher.SelectedCommands["fix"] || watcher.SelectedCommands["comment"] {
		t.Fatalf("selected fix child did not hold an authenticated pull: handler=%v commands=%v", watcher.Handler, watcher.SelectedCommands)
	}
	bundle, err := os.ReadFile(filepath.Join("..", "..", "internal", "action", "testdata", "source.bundle"))
	if err != nil {
		t.Fatal(err)
	}
	job := action.Job{Version: action.ProtocolVersion,
		Identity: action.Identity{JobID: "job-1", AttemptID: "attempt-1", ExecutorID: action.LocalExecutorID,
			Repository: "owner/repo", RepositoryID: 1, OwnerID: 2, Requester: "writer", RequesterID: 3,
			Number: 7, SubjectType: "Issue"},
		Command: "fix", Action: command.Fix, Title: "Fix source",
		Source: workspace.Snapshot{Head: "38a7fa625016861399920bfd0fbea8b0ef053773", Repository: bundle,
			Files: []workspace.File{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")}}},
		Main:              action.RoleSettings{Provider: "codex", Model: "main-1", Effort: "high", Skills: []string{skills.BuiltinAntiPromptInjection}},
		RuntimeBudget:     time.Minute,
		InactivityTimeout: config.DefaultInactivityTimeout,
	}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	result, err := owner.Executor.Execute(t.Context(), job, windowsAcceptCheckpoints{})
	if err != nil {
		t.Fatalf("selected HTTPS fix: %v; logs=%s", err, logs.String())
	}
	if err := result.Validate(job); err != nil {
		t.Fatalf("invalid selected fix result: %v", err)
	}
	if result.Answer != "Fixed source." || len(result.Calls) != 1 || len(result.Changes) != 1 ||
		result.Changes[0].Path != "source.txt" || result.Changes[0].Mode != workspace.Regular ||
		result.Changes[0].Delete || string(result.Changes[0].Data) != "fixed\n" {
		t.Fatalf("selected HTTPS worker returned unexpected fix: %+v", result)
	}
	if data, err := os.ReadFile(modelCalls); err != nil || string(data) != "model\n" {
		t.Fatalf("model calls=%q error=%v", data, err)
	}
	if err := closeWorker(); err != nil {
		t.Fatal(err)
	}
	<-done
	entries, err := os.ReadDir(cfg.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "minion-") || strings.HasPrefix(entry.Name(), ".minion-") {
			t.Fatalf("selected worker left action scratch %q", entry.Name())
		}
	}
}
