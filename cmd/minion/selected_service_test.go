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
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
)

type terminalRecoveryGitHub struct {
	monitor.GitHub
	item  github.Item
	reply string
}

type pendingPushLoopGitHub struct {
	monitor.GitHub
	cancel context.CancelFunc
	checks int
}

func (g *pendingPushLoopGitHub) Branch(context.Context, string, string) (github.Branch, error) {
	g.checks++
	if g.checks == 1 {
		return github.Branch{}, &github.StatusError{StatusCode: http.StatusNotFound}
	}
	g.cancel()
	return github.Branch{}, context.Canceled
}

func serviceStore(t *testing.T, data state.Data) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.Data = data
	return store
}

func (g *terminalRecoveryGitHub) HasVerifiedBotReply(context.Context, string, int, time.Time, string, string, bool) (bool, error) {
	return false, nil
}

func (g *terminalRecoveryGitHub) CurrentItem(context.Context, github.Item) (github.Item, error) {
	return g.item, nil
}

func (g *terminalRecoveryGitHub) Repository(context.Context, string) (github.Repository, error) {
	return github.Repository{ID: 1, Owner: github.User{ID: 2}, DefaultBranch: "main"}, nil
}

func (g *terminalRecoveryGitHub) Permission(context.Context, string, string) (github.Permission, error) {
	return github.Permission{Permission: "write", User: g.item.Author}, nil
}

func (g *terminalRecoveryGitHub) CreateIssueComment(_ context.Context, _ string, _ int, body string) (github.Comment, error) {
	g.reply = body
	return github.Comment{ID: 42}, nil
}

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

func TestPublicationValidationUsesBoundRepositorySnapshot(t *testing.T) {
	cfg := selectedServiceTestConfig(t)
	repo := &cfg.Repositories[0]
	repo.ID, repo.OwnerID = 1, 2
	repo.AllowedActors = []string{"writer"}
	repo.AllowedActorIDs = map[string]int64{"writer": 7}
	at := time.Now().UTC().Add(-time.Minute)
	source := github.Item{Key: "comment:1", NodeID: "source-node", HTMLURL: "https://github.com/owner/repo/pull/7#issuecomment-1",
		UpdatedAt: at, Body: "@bot comment", Author: github.User{Login: "writer", ID: 7}}
	accepted := state.AcceptedAction{
		JobID: "job-1", AttemptID: "attempt-1", ExecutorID: action.LocalExecutorID,
		Repository: repo.Name, RepositoryID: repo.ID, OwnerID: repo.OwnerID,
		SubjectType: "PullRequest", SubjectNumber: 7,
		SourceKey: source.Key, SourceNodeID: source.NodeID, SourceURL: source.HTMLURL, SourceUpdatedAt: at,
		Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
		AcceptedAt: at, Deadline: at.Add(time.Hour), Assigned: true,
	}
	store := serviceStore(t, state.Data{Accepted: &accepted, JobFailures: map[string]state.JobFailure{
		accepted.JobID: {Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline},
	}})
	watcher := &monitor.Monitor{Config: cfg, GitHub: &terminalRecoveryGitHub{item: source}}
	owner := newOrchestrator(cfg, nil, store, nil, nil)
	bindPublicationValidation(owner, watcher)
	watcher.Config.Repositories = nil // Invitation registration may replace the slice during recovery.
	job := monitor.Job{Repository: *repo, SubjectType: accepted.SubjectType, Number: accepted.SubjectNumber,
		Source: source, Command: command.Command{Name: "comment", Action: command.Comment},
		ID: accepted.JobID, AttemptID: accepted.AttemptID, AcceptedAt: at, Deadline: accepted.Deadline}
	if err := owner.ValidatePublication(t.Context(), job, true); err != nil {
		t.Fatalf("publication lost the repository from its bound snapshot: %v", err)
	}
	if err := owner.ValidateFixedRefusal(t.Context(), job); err != nil {
		t.Fatalf("fixed refusal lost the repository from its bound snapshot: %v", err)
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
				if err == nil || done != nil || closeWorker != nil || owner.Executor != nil || watcher.Handler != nil || watcher.SelectedCommands != nil || watcher.SelectedExecutorID != "" {
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
			if watcher.Handler != owner || owner.ValidatePublication == nil || watcher.SelectedExecutorID != action.LocalExecutorID || len(watcher.SelectedCommands) != 1 || !watcher.SelectedCommands["comment"] || watcher.SelectedCommands["review"] {
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
			store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
			if code := runSelectedLocalService(t.Context(), watcher, store, false, done, logger, closeWorker); code != 0 {
				t.Fatalf("clean selected child shutdown returned %d: %s", code, logs.String())
			}
		})
	}
}

func TestOrchestrateModeRecoversAssignedActionWithMissingSoleSkill(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	cfg := selectedServiceTestConfig(t)
	cfg.Commands = map[string]config.CommandMapping{
		"comment": {Action: "comment", Skills: []string{"https://github.com/example/skills/tree/main/skills/not-installed"}},
	}
	cfg.Repositories[0].ID, cfg.Repositories[0].OwnerID = 1, 2
	cfg.Repositories[0].AllowedActors = []string{"writer"}
	cfg.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
	if installed, err := cfg.CommandInstalled("comment"); err != nil || installed {
		t.Fatalf("fixture command skill must be absent: installed=%t error=%v", installed, err)
	}
	at := time.Now().UTC().Add(-time.Minute)
	accepted := state.AcceptedAction{
		JobID: "job-1", AttemptID: "attempt-1", ExecutorID: action.LocalExecutorID,
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
		SubjectType: "PullRequest", SubjectNumber: 7,
		SourceKey: "comment:1", SourceNodeID: "source-node", SourceURL: "https://github.com/owner/repo/pull/7#issuecomment-1",
		Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
		AcceptedAt: at, Deadline: at.Add(time.Hour), Assigned: true,
	}
	gh := &terminalRecoveryGitHub{item: github.Item{
		Key: accepted.SourceKey, NodeID: accepted.SourceNodeID, HTMLURL: accepted.SourceURL,
		Body: "@bot comment", Author: github.User{Login: accepted.Requester, ID: accepted.RequesterID},
	}}
	statePath := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(statePath, at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	owner := newOrchestrator(cfg, nil, store, logger, nil)
	watcher := &monitor.Monitor{Config: cfg, GitHub: gh, Logger: logger}
	if code := runOrchestrateMode(t.Context(), owner, watcher, store, false, logger, "", time.Second); code != 0 {
		t.Fatalf("assigned action required a removed skill or worker: code=%d logs=%q", code, logs.String())
	}
	if gh.reply == "" || !strings.Contains(gh.reply, "Minion could not complete this request") || !strings.HasSuffix(gh.reply, "<!-- linkdataminion:job-1 -->") {
		t.Fatalf("fixed failure reply was not posted: %q", gh.reply)
	}
	if store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || len(store.Data.JobFailures) != 0 || owner.Executor != nil || watcher.SelectedCommands != nil {
		t.Fatalf("terminal completion started a worker or was not saved: %+v executor=%v selected=%v", store.Data, owner.Executor, watcher.SelectedCommands)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(statePath, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Data.Accepted != nil || reopened.Data.Processed[accepted.JobID].IsZero() {
		t.Fatalf("terminal completion was not durable: %+v", reopened.Data)
	}
}

func TestOrchestrateModeAttachesAfterExpiredAcceptanceRecovery(t *testing.T) {
	for _, loop := range []bool{false, true} {
		name := "one shot"
		if loop {
			name = "loop"
		}
		t.Run(name, func(t *testing.T) {
			cfg := selectedServiceTestConfig(t)
			cfg.Repositories[0].ID, cfg.Repositories[0].OwnerID = 1, 2
			cfg.Repositories[0].AllowedActors = []string{"writer"}
			cfg.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
			at := time.Now().UTC().Add(-time.Minute)
			accepted := state.AcceptedAction{
				JobID: "job-1", AttemptID: "attempt-1", ExecutorID: action.LocalExecutorID,
				Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
				SubjectType: "PullRequest", SubjectNumber: 7,
				SourceKey: "comment:1", SourceNodeID: "source-node", SourceURL: "https://github.com/owner/repo/pull/7#issuecomment-1",
				Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
				AcceptedAt: at, Deadline: at.Add(2 * time.Minute),
			}
			store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), at.Add(-time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			store.Data.Accepted = &accepted
			store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline}
			if terminalRecoveryPending(store) {
				t.Fatal("acceptance was already terminal before startup preflight")
			}
			// Model the deadline crossing after main's initial recovery check.
			accepted.Deadline = time.Now().UTC().Add(-time.Second)
			failure := store.Data.JobFailures[accepted.JobID]
			failure.Deadline = accepted.Deadline
			store.Data.JobFailures[accepted.JobID] = failure
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			if !terminalRecoveryPending(store) {
				t.Fatal("acceptance did not become terminal after startup preflight")
			}
			gh := &terminalRecoveryGitHub{item: github.Item{
				Key: accepted.SourceKey, NodeID: accepted.SourceNodeID, HTMLURL: accepted.SourceURL,
				Body: "@bot comment", Author: github.User{Login: accepted.Requester, ID: accepted.RequesterID},
			}}
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)
			owner := newOrchestrator(cfg, nil, store, logger, nil)
			watcher := &monitor.Monitor{Config: cfg, GitHub: gh, Logger: logger}
			code := runOrchestrateMode(t.Context(), owner, watcher, store, loop, logger,
				filepath.Join(t.TempDir(), "missing-worker"), time.Second)
			attempted := strings.Contains(logs.String(), "start selected worker: start local worker:")
			if loop && (code != 1 || !attempted) || !loop && (code != 0 || attempted) {
				t.Fatalf("worker attach after recovery: loop=%t code=%d attempted=%t logs=%q", loop, code, attempted, logs.String())
			}
			if gh.reply == "" || store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || len(store.Data.JobFailures) != 0 {
				t.Fatalf("deadline recovery did not finish before worker attach: reply=%q state=%+v", gh.reply, store.Data)
			}
		})
	}
}

func TestMonitorLoopRechecksPendingPushAtSubsecondWake(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining time.Duration
	}{
		{"half second", 500 * time.Millisecond},
		{"just over one second", 1100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				deadline := now.Add(tc.remaining)
				jobID := strings.Repeat("a", 64)
				accepted := state.AcceptedAction{
					JobID: jobID, AttemptID: strings.Repeat("b", 32), ExecutorID: action.LocalExecutorID,
					Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
					SubjectType: "Issue", SubjectNumber: 7,
					SourceKey: "comment:1", SourceNodeID: "node:1", SourceURL: "https://github.com/owner/repo/issues/7#issuecomment-1",
					Requester: "writer", RequesterID: 7, CommandName: "fix", Action: "fix",
					AcceptedAt: now, Deadline: deadline, RetryAt: deadline, Assigned: true,
				}
				sha := strings.Repeat("c", 40)
				store := serviceStore(t, state.Data{
					Accepted: &accepted, JobFailures: map[string]state.JobFailure{
						jobID: {Attempts: 1, AcceptedAt: now, Deadline: deadline},
					},
					PendingFixes: map[string]state.PendingFix{jobID: {SHA: sha}},
					PublicationWrites: map[string]state.PublicationWrite{jobID + "-push": {
						AttemptID: accepted.AttemptID, Kind: "push", BotLogin: "bot", Repository: "bot/repo", Number: 7,
						Marker: "<!-- linkdataminion:" + jobID + " -->", Branch: "minion/fix-7-" + jobID[:12], SHA: sha,
						Since: now, StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
					}},
					NextPollAt: now.Add(time.Hour), NextInviteCheckAt: now.Add(time.Hour),
				})
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				gh := &pendingPushLoopGitHub{cancel: cancel}
				watcher := &monitor.Monitor{GitHub: gh, Now: time.Now}
				var logs bytes.Buffer
				code := runMonitorLoop(ctx, watcher, store, true, nil, log.New(&logs, "", 0), false)
				if elapsed := time.Since(now); code != 0 || gh.checks != 2 || elapsed != tc.remaining/2 {
					t.Fatalf("loop missed pending push wake: code=%d checks=%d elapsed=%s want=%s logs=%q",
						code, gh.checks, elapsed, tc.remaining/2, logs.String())
				}
			})
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
	watcher := &monitor.Monitor{Handler: handler, SelectedExecutorID: action.LocalExecutorID}
	store := serviceStore(t, state.Data{NextInviteCheckAt: time.Now().Add(time.Hour)})
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

func TestRunMonitorLoopStopsOnUncertainAcceptanceState(t *testing.T) {
	store := serviceStore(t, state.Data{Accepted: &state.AcceptedAction{}})
	var logs bytes.Buffer
	if code := runMonitorLoop(t.Context(), &monitor.Monitor{}, store, true, nil, log.New(&logs, "", 0), false); code != 1 {
		t.Fatalf("uncertain acceptance kept polling: code=%d logs=%q", code, logs.String())
	}
}

func TestSelectedLocalServiceReportsCloseFailure(t *testing.T) {
	now := time.Now().UTC()
	store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
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
		store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
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
