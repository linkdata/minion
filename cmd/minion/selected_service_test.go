package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
)

func selectedServiceTestConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		WorkRoot: t.TempDir(), Main: "codex", BotLogin: "bot",
		Commands: map[string]config.CommandMapping{
			"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}},
			"review":  {Action: "review", Skills: []string{skills.BuiltinAntiPromptInjection}},
		},
		Repositories: []config.Repository{{Name: "owner/repo", Actions: []string{"comment"}}},
		Codex:        config.CodexConfig{APIKey: "selected-config-key", Model: "main-1", ReasoningEffort: "high"},
		Claude:       config.ClaudeConfig{APIKey: "unselected-config-key"},
	}
}

func TestAttachSelectedLocalWorkerFreezesStartupAndOwnsClose(t *testing.T) {
	for _, mode := range []string{"ready", "reject-before-pull"} {
		t.Run(mode, func(t *testing.T) {
			cfg := selectedServiceTestConfig(t)
			report := filepath.Join(t.TempDir(), "startup.json")
			t.Setenv("MINION_SELECTED_SERVICE_HELPER", mode)
			t.Setenv("MINION_SELECTED_SERVICE_REPORT", report)
			t.Setenv("GH_TOKEN", "parent-github-key")
			t.Setenv("GITHUB_TOKEN", "parent-github-key")
			t.Setenv("GH_ENTERPRISE_TOKEN", "parent-enterprise-key")
			t.Setenv("GITHUB_ENTERPRISE_TOKEN", "parent-enterprise-key")
			t.Setenv("CODEX_API_KEY", "selected-environment-key")
			t.Setenv("CODEX_ACCESS_TOKEN", "selected-environment-token")
			t.Setenv("OPENAI_API_KEY", "unrelated-openai-key")
			t.Setenv("ANTHROPIC_API_KEY", "unselected-environment-key")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "unrelated-anthropic-token")
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "unselected-claude-token")
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)
			owner := newOrchestrator(cfg, nil, nil, logger, nil)
			watcher := &monitor.Monitor{Config: cfg, Logger: logger}
			done, closeWorker, err := attachSelectedLocalWorker(t.Context(), owner, watcher, os.Args[0], []string{"-test.run=^TestSelectedLocalServiceChild$"}, 5*time.Second)
			if mode == "reject-before-pull" {
				if err == nil || done != nil || closeWorker != nil || owner.Executor != nil || watcher.Handler != nil || watcher.SelectedCommands != nil {
					t.Fatalf("failed preflight exposed a worker or discovery: done=%v close=%v owner=%v handler=%v commands=%v error=%v", done != nil, closeWorker != nil, owner.Executor, watcher.Handler, watcher.SelectedCommands, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if done == nil {
				t.Fatal("attached child has no exit signal")
			}
			t.Cleanup(func() { _ = closeWorker() })
			if watcher.Handler != owner || len(watcher.SelectedCommands) != 1 || !watcher.SelectedCommands["comment"] || watcher.SelectedCommands["review"] {
				t.Fatalf("monitor did not receive exact frozen selection: handler=%v commands=%v", watcher.Handler, watcher.SelectedCommands)
			}
			data, err := os.ReadFile(report)
			if err != nil {
				t.Fatal(err)
			}
			var startup action.WorkerStartup
			if err := json.Unmarshal(data, &startup); err != nil {
				t.Fatal(err)
			}
			if startup.WorkRoot != cfg.WorkRoot || !strings.HasPrefix(startup.Endpoint.BaseURL, "https://127.0.0.1:") || len(startup.Endpoint.Certificate) == 0 || startup.Endpoint.Token == "" || len(startup.Selection.Commands) != 1 || startup.Selection.Commands["comment"].Action != "comment" || startup.CodexAPIKey != cfg.Codex.APIKey || startup.CodexNativeAPIKey != "" || startup.CodexAccessToken != "" || startup.ClaudeAPIKey != "" {
				t.Fatalf("child startup differed from parent selection: work_root=%q endpoint=%q command_count=%d comment_action=%q", startup.WorkRoot, startup.Endpoint.BaseURL, len(startup.Selection.Commands), startup.Selection.Commands["comment"].Action)
			}
			now := time.Now().UTC()
			store := &state.Store{Data: state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)}}
			if code := runSelectedLocalService(t.Context(), watcher, store, false, done, logger, closeWorker); code != 0 {
				t.Fatalf("clean selected child shutdown returned %d: %s", code, logs.String())
			}
		})
	}
}

func TestAttachSelectedLocalWorkerSelectedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		adjust func(*config.Config)
		want   action.WorkerCredentials
	}{
		{"configured Codex", func(*config.Config) {}, action.WorkerCredentials{CodexAPIKey: "selected-config-key"}},
		{"native Codex", func(cfg *config.Config) {
			cfg.Codex.APIKey = ""
			cfg.Codex.NativeAPIKey = "selected-environment-key"
			cfg.Codex.NativeAccessToken = "selected-environment-token"
		}, action.WorkerCredentials{CodexNativeAPIKey: "selected-environment-key", CodexAccessToken: "selected-environment-token"}},
		{"Claude only", func(cfg *config.Config) {
			cfg.Main = "claude"
			cfg.Claude.Model, cfg.Claude.Effort = "claude-main", "medium"
		}, action.WorkerCredentials{ClaudeAPIKey: "unselected-config-key"}},
		{"Codex critic", func(cfg *config.Config) {
			cfg.Main, cfg.Adversarial = "claude", "codex"
			cfg.Claude.Model, cfg.Claude.Effort = "claude-main", "medium"
			cfg.Codex.APIKey = ""
			cfg.Codex.NativeAPIKey = "selected-environment-key"
			cfg.Codex.NativeAccessToken = "selected-environment-token"
		}, action.WorkerCredentials{CodexNativeAPIKey: "selected-environment-key", CodexAccessToken: "selected-environment-token", ClaudeAPIKey: "unselected-config-key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := selectedServiceTestConfig(t)
			tc.adjust(&cfg)
			t.Setenv("CODEX_API_KEY", "selected-environment-key")
			t.Setenv("CODEX_ACCESS_TOKEN", "selected-environment-token")
			t.Setenv("MINION_SELECTED_SERVICE_HELPER", "ready")
			report := filepath.Join(t.TempDir(), "startup.json")
			t.Setenv("MINION_SELECTED_SERVICE_REPORT", report)
			owner := newOrchestrator(cfg, nil, nil, log.New(io.Discard, "", 0), nil)
			watcher := &monitor.Monitor{Config: cfg}
			_, closeWorker, err := attachSelectedLocalWorker(t.Context(), owner, watcher, os.Args[0], []string{"-test.run=^TestSelectedLocalServiceChild$"}, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = closeWorker() })
			data, err := os.ReadFile(report)
			if err != nil {
				t.Fatal(err)
			}
			var startup action.WorkerStartup
			if err := json.Unmarshal(data, &startup); err != nil {
				t.Fatal(err)
			}
			if startup.CodexAPIKey != tc.want.CodexAPIKey || startup.CodexNativeAPIKey != tc.want.CodexNativeAPIKey || startup.CodexAccessToken != tc.want.CodexAccessToken || startup.ClaudeAPIKey != tc.want.ClaudeAPIKey {
				t.Fatal("child startup did not contain exactly the selected credentials")
			}
		})
	}
}

type unavailableSelectedHandler struct{ waits int }

func (h *unavailableSelectedHandler) WaitCapacity(context.Context) error {
	h.waits++
	return action.ErrLocalWorkerUnavailable
}

func (*unavailableSelectedHandler) Execute(context.Context, monitor.Job) error {
	return errors.New("unavailable worker received a Job")
}

func TestSelectedLocalServiceStopsAfterWorkerLoss(t *testing.T) {
	handler := &unavailableSelectedHandler{}
	watcher := &monitor.Monitor{Handler: handler}
	store := &state.Store{}
	var logs bytes.Buffer
	closed := 0
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	code := runSelectedLocalService(ctx, watcher, store, true, done, log.New(&logs, "", 0), func() error { closed++; return nil })
	if code != 1 || handler.waits != 1 || closed != 1 || ctx.Err() != nil {
		t.Fatalf("retired worker was retried or not closed: code=%d waits=%d closed=%d context=%v logs=%q", code, handler.waits, closed, ctx.Err(), logs.String())
	}
}

func TestSelectedLocalServiceReportsCloseFailure(t *testing.T) {
	now := time.Now().UTC()
	store := &state.Store{Data: state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)}}
	watcher := &monitor.Monitor{Handler: &action.Orchestrator{}}
	var logs bytes.Buffer
	closed := 0
	code := runSelectedLocalService(t.Context(), watcher, store, false, make(chan struct{}), log.New(&logs, "", 0), func() error {
		closed++
		return errors.New("child exited 7")
	})
	if code != 1 || closed != 1 || !strings.Contains(logs.String(), "close selected local worker: child exited 7") {
		t.Fatalf("failed Close was lost: code=%d closed=%d logs=%q", code, closed, logs.String())
	}
}

func TestSelectedLocalServiceStopsDuringScheduledSleep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store := &state.Store{Data: state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)}}
		watcher := &monitor.Monitor{}
		done := make(chan struct{})
		result := make(chan int, 1)
		closed := 0
		var logs bytes.Buffer
		go func() {
			result <- runSelectedLocalService(t.Context(), watcher, store, true, done, log.New(&logs, "", 0), func() error {
				closed++
				return nil
			})
		}()
		// The monitor is now blocked on its one-hour poll timer.
		synctest.Wait()
		close(done)
		synctest.Wait()
		select {
		case code := <-result:
			if code != 1 || closed != 1 || !time.Now().Before(store.Data.NextPollAt) || !strings.Contains(logs.String(), "selected local worker exited") {
				t.Fatalf("child exit did not interrupt the poll sleep: code=%d closed=%d now=%s next=%s logs=%q", code, closed, time.Now(), store.Data.NextPollAt, logs.String())
			}
		default:
			t.Fatal("child exit did not stop the sleeping monitor")
		}
	})
}

func TestSelectedLocalServiceChild(t *testing.T) {
	mode := os.Getenv("MINION_SELECTED_SERVICE_HELPER")
	if mode == "" {
		return
	}
	var startup action.WorkerStartup
	if err := json.NewDecoder(os.Stdin).Decode(&startup); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != "" || os.Getenv("GH_ENTERPRISE_TOKEN") != "" || os.Getenv("GITHUB_ENTERPRISE_TOKEN") != "" || os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" || os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" || os.Getenv("CODEX_API_KEY") != "" || os.Getenv("CODEX_ACCESS_TOKEN") != "" {
		t.Fatal("selected child inherited parent or unselected provider credentials")
	}
	data, err := json.Marshal(startup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("MINION_SELECTED_SERVICE_REPORT"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if mode == "reject-before-pull" {
		fmt.Fprintln(os.Stderr, "minion preflight: fixture rejected before pull")
		os.Exit(7)
	}
	client, err := action.NewLocalExecutionClient(startup.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	pull := fmt.Sprintf(`{"version":%d,"executor_id":"local"}`, action.ProtocolVersion)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, startup.Endpoint.BaseURL+"/next", strings.NewReader(pull))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("idle close status = %d", response.StatusCode)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
}
