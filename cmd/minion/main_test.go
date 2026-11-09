package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/testsupport"
)

type startupTransport func(*http.Request) (*http.Response, error)

func (f startupTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func startupCommandMarkers(t *testing.T) map[string]string {
	t.Helper()
	markers := make(map[string]string)
	for _, name := range []string{"git", "codex", "claude", "gh"} {
		path := filepath.Join(os.Getenv("PATH"), name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'called' > \"$0.called\"\nprintf 'fixture preflight failure' >&2\nexit 37\n"), 0700); err != nil {
			t.Fatal(err)
		}
		markers[name] = path + ".called"
	}
	return markers
}

func assertNoStartupCommands(t *testing.T, markers map[string]string) {
	t.Helper()
	for name, path := range markers {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("early shortcut invoked %s or could not inspect marker: %v", name, err)
		}
	}
}

func runStartupFixture(t *testing.T, cfg config.Config, args ...string) (int, string) {
	t.Helper()
	return runStartupFixtureWithRun(t, cfg, run, args...)
}

func runStartupFixtureWithRun(t *testing.T, cfg config.Config, start func() int, args ...string) (int, string) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "stderr.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	oldFlags, oldArgs, oldStderr := flag.CommandLine, os.Args, os.Stderr
	defer func() { flag.CommandLine, os.Args, os.Stderr = oldFlags, oldArgs, oldStderr }()
	flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
	os.Args = append([]string{"minion", "-config", configPath}, args...)
	os.Stderr = logFile
	code := start()
	os.Stderr = oldStderr
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(data)
}

func TestWebservUserSwitchPrecedesInitialConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	oldFlags, oldArgs := flag.CommandLine, os.Args
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
	os.Args = []string{"minion", "-config", path, "-user", "minion-no-such-user-!"}
	if code := runWithGitCheck(func(context.Context, string) error {
		t.Fatal("checked Git before switching user")
		return nil
	}); code != 2 {
		t.Fatalf("user switch exit = %d, want 2", code)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created config before switching user: %v", err)
	}
}

func TestGitHubStartupUsesConfiguredToken(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	t.Setenv("GH_TOKEN", "wrong-ambient-token")
	t.Setenv("GITHUB_TOKEN", "wrong-ambient-token")
	certFile, keyFile, _ := remoteTestTLSFiles(t)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "Bearer configured-bot-token" {
			t.Errorf("GitHub request used %q", got)
		}
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/user":
			_, _ = recorder.WriteString(`{"id":8,"login":"bot"}`)
		case "/repos/owner/repo/notifications":
			_, _ = recorder.WriteString(`[]`)
		case "/repos/owner/repo":
			_, _ = recorder.WriteString(`{"id":1,"full_name":"owner/repo","default_branch":"main","owner":{"id":2}}`)
		case "/repos/owner/repo/collaborators/bot/permission":
			_, _ = recorder.WriteString(`{"permission":"write","user":{"id":8,"login":"bot"}}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", request.Method, request.URL)
			recorder.WriteHeader(http.StatusNotFound)
		}
		return recorder.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	cfg := config.Config{
		BotLogin: "bot", GitHub: config.GitHubConfig{Token: "configured-bot-token"},
		Codex:     config.CodexConfig{Model: "fixture-model", ReasoningEffort: "medium"},
		StatePath: filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
		RemoteExecutor: &config.RemoteExecutorConfig{
			ExecutorID: "remote", Listen: "127.0.0.1:8443", TLSCertFile: certFile, TLSKeyFile: keyFile,
			Token: strings.Repeat("A", 43),
		},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
	}
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error { return nil })
	}, "-check")
	if code != 0 || !strings.Contains(output, "preflight passed as @bot") {
		t.Fatalf("configured GitHub account failed preflight: code=%d output=%q", code, output)
	}
	cfg.Admin = &config.AdminConfig{Listen: "127.0.0.1:8080", Administrators: []config.AdminAccount{testAdminAccount()}}
	cfg.Repositories = nil
	code, output = runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error { return nil })
	}, "-check")
	if code != 1 || !strings.Contains(output, "no repositories configured") {
		t.Fatalf("empty setup passed repository preflight: code=%d output=%q", code, output)
	}
}

func TestCheckValidAdminTLSWithoutBinding(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	address := occupiedAdminAddress(t)
	certFile, keyFile, _ := remoteTestTLSFiles(t)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/user":
			_, _ = recorder.WriteString(`{"id":8,"login":"bot"}`)
		case "/repos/owner/repo/notifications":
			_, _ = recorder.WriteString(`[]`)
		case "/repos/owner/repo":
			_, _ = recorder.WriteString(`{"id":1,"full_name":"owner/repo","default_branch":"main","owner":{"id":2}}`)
		case "/repos/owner/repo/collaborators/bot/permission":
			_, _ = recorder.WriteString(`{"permission":"write","user":{"id":8,"login":"bot"}}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", request.Method, request.URL)
			recorder.WriteHeader(http.StatusNotFound)
		}
		return recorder.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	cfg := config.Config{
		BotLogin: "bot", GitHub: config.GitHubConfig{Token: "test-token"}, Main: "codex",
		Codex: config.CodexConfig{Model: "fixture-model", ReasoningEffort: "medium"},
		Admin: &config.AdminConfig{Listen: address, TLSCertFile: certFile, TLSKeyFile: keyFile,
			Administrators: []config.AdminAccount{testAdminAccount()}},
		RemoteExecutor: &config.RemoteExecutorConfig{ExecutorID: "remote", Listen: "127.0.0.1:8443",
			TLSCertFile: certFile, TLSKeyFile: keyFile, Token: strings.Repeat("A", 43)},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
		StatePath:    filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
	}
	start := func() int { return runWithGitCheck(func(context.Context, string) error { return nil }) }
	code, output := runStartupFixtureWithRun(t, cfg, start, "-check")
	if code != 0 || !strings.Contains(output, "preflight passed as @bot") {
		t.Fatalf("-check rejected valid TLS on occupied port: code=%d output=%q", code, output)
	}
	code, output = runStartupFixtureWithRun(t, cfg, start)
	if code != 1 || !strings.Contains(output, "admin dashboard: prepare listener:") || !strings.Contains(output, address) {
		t.Fatalf("service did not report occupied admin port: code=%d output=%q", code, output)
	}
}

func TestLocalClaudeSocketOptInLogsWarning(t *testing.T) {
	for _, tc := range []struct {
		name, main, adversarial string
		warn                    bool
	}{
		{name: "main Claude", main: "claude", warn: true},
		{name: "critic Claude", main: "codex", adversarial: "claude", warn: true},
		{name: "Codex only", main: "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				BotLogin: "fixture", Main: tc.main, Adversarial: tc.adversarial,
				Claude:    config.ClaudeConfig{Effort: "high", AllowAllUnixSockets: true},
				StatePath: filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
			}
			store, err := state.Open(cfg.StatePath, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			code, output := runStartupFixture(t, cfg)
			if code != 1 || !strings.Contains(output, state.ErrLocked.Error()) || strings.Contains(output, claudeUnixSocketWarning) {
				t.Fatalf("locked startup warned before admission: exit=%d log=%q", code, output)
			}
			cfg.StatePath = filepath.Join(t.TempDir(), "active-state.json")
			activeState, err := state.Open(cfg.StatePath, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if err := activeState.Close(); err != nil {
				t.Fatal(err)
			}
			code, output = runStartupFixtureWithRun(t, cfg, func() int {
				return runWithGitCheck(func(context.Context, string) error { return errors.New("fixture Git stop") })
			})
			wantWarnings := 0
			if runtime.GOOS == "linux" && tc.warn {
				wantWarnings = 1
			}
			if code != 1 || !strings.Contains(output, "Git preflight: fixture Git stop") || strings.Count(output, claudeUnixSocketWarning) != wantWarnings {
				t.Fatalf("local socket warning: exit=%d want=%d log=%q", code, wantWarnings, output)
			}
		})
	}
}

func TestLocalClaudeSocketOptInReachesModelCheck(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Bubblewrap opt-in applies only on Linux")
	}
	testsupport.FakeBubblewrap(t, "--unshare-user")
	cfg := config.Config{Main: "claude", Claude: config.ClaudeConfig{Model: "claude-pinned", Effort: "low", AllowAllUnixSockets: true}}
	var output bytes.Buffer
	if err := checkModels(t.Context(), &cfg, log.New(&output, "", 0)); err == nil || !strings.Contains(err.Error(), "Claude:") || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("local model check lost Bubblewrap opt-in: %v", err)
	}
}

func TestOneShotPassSkipsPreflightWhenStateLocked(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	markers := startupCommandMarkers(t)
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "")
	}

	statePath := filepath.Join(t.TempDir(), "state.json")
	workRoot := t.TempDir()

	cfg := config.Config{
		BotLogin: "fixture", StatePath: statePath, WorkRoot: workRoot,
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
	}
	store, err := state.Open(statePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	for _, mode := range []struct {
		name string
		args []string
		code int
	}{
		{name: "oneshot", args: []string{"-oneshot"}}, {name: "check", args: []string{"-check"}, code: 1}, {name: "service", code: 1},
	} {
		t.Run(mode.name, func(t *testing.T) {
			code, output := runStartupFixture(t, cfg, mode.args...)
			if code != mode.code || mode.code == 1 && !strings.Contains(output, state.ErrLocked.Error()) || mode.code == 0 && !strings.Contains(output, "skipping this cron pass") {
				t.Fatalf("locked startup mode changed: exit=%d want=%d log=%q", code, mode.code, output)
			}
			assertNoStartupCommands(t, markers)
		})
	}
}

func TestRunRejectsUnsafeWorkRootBeforeExecutionPreflight(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	root := t.TempDir()
	workRoot := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(workRoot, nil, 0600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config.Config{
		Main: "codex", BotLogin: "fixture", StatePath: statePath, WorkRoot: workRoot,
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
	}
	code, output := runStartupFixture(t, cfg)
	if code != 1 || !strings.Contains(output, "work root:") {
		t.Fatalf("unsafe work root: exit=%d log=%q", code, output)
	}
	store, err := state.Open(statePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.Data.Accepted != nil || len(store.Data.JobFailures) != 0 {
		t.Fatalf("unsafe work root admitted an action: %+v", store.Data)
	}
}

func TestPreflightFailurePrunesExpiredPrivateRecords(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")
	workRoot := t.TempDir()
	diagnostic := filepath.Join(workRoot, "worker-diagnostics", "old-attempt.json")
	if err := os.MkdirAll(filepath.Dir(diagnostic), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diagnostic, []byte("expired private diagnostic"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(statePath, now)
	if err != nil {
		t.Fatal(err)
	}
	store.Data.TerminalDecisions["old-job"] = state.TerminalDecision{
		AttemptID: "old-attempt", Outcome: "failed", DecidedAt: now.Add(-31 * 24 * time.Hour),
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		BotLogin: "fixture", StatePath: statePath, WorkRoot: workRoot,
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
	}
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheckAt(func(context.Context, string) error {
			return errors.New("fixture Git failure")
		}, func() time.Time { return now })
	})
	if code != 1 || !strings.Contains(output, "Git preflight: fixture Git failure") {
		t.Fatalf("failed preflight: code=%d log=%q", code, output)
	}
	if _, err := os.Stat(diagnostic); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired private diagnostic survived failed preflight: %v", err)
	}
}

func TestTerminalRecoveryBypassesNewWorkPreflight(t *testing.T) {
	for _, mode := range []struct {
		name          string
		args          []string
		wantCode      int
		wantRecovered bool
	}{
		{name: "oneshot", args: []string{"-mode=orchestrate", "-oneshot"}, wantRecovered: true},
		{name: "service", args: []string{"-mode=orchestrate"}, wantCode: 1, wantRecovered: true},
		{name: "check", args: []string{"-mode=orchestrate", "-check"}, wantCode: 1},
	} {
		t.Run(mode.name, func(t *testing.T) {
			isolateHostPaths(t, t.TempDir())
			markers := startupCommandMarkers(t)
			t.Setenv("GH_TOKEN", "offline-token")
			root := t.TempDir()
			badWorkRoot := filepath.Join(root, "not-a-directory")
			if err := os.WriteFile(badWorkRoot, nil, 0600); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(root, "state.json")
			at := time.Now().UTC().Add(-time.Minute)
			accepted := state.AcceptedAction{MainProvider: "codex", MainSettings: frozenTestCodex,
				JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "local",
				Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
				SubjectType: "PullRequest", SubjectNumber: 7,
				SourceKey: "comment:1", SourceNodeID: "source-node", SourceURL: "https://github.com/owner/repo/pull/7#issuecomment-1",
				Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
				AcceptedAt: at, Deadline: at.Add(time.Hour), Assigned: true,
			}
			store, err := state.Open(statePath, at.Add(-time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			store.Data.Accepted = &accepted
			store.Data.JobFailures[accepted.JobID] = frozenFailureForTest(accepted)
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			previousTransport := http.DefaultTransport
			http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || request.URL.Path != "/user" {
					t.Errorf("terminal discard made unexpected GitHub request: %s %s", request.Method, request.URL)
				}
				recorder := httptest.NewRecorder()
				recorder.Header().Set("Content-Type", "application/json")
				_, _ = recorder.WriteString(`{"id":8,"login":"bot"}`)
				return recorder.Result(), nil
			})
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			cfg := config.Config{BotLogin: "bot", GitHub: config.GitHubConfig{Token: "offline-token"}, StatePath: statePath, WorkRoot: badWorkRoot,
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
			code, output := runStartupFixtureWithRun(t, cfg, func() int {
				return runWithGitCheck(func(context.Context, string) error {
					t.Error("terminal recovery reached Git preflight")
					return errors.New("unexpected Git preflight")
				})
			}, mode.args...)
			if code != mode.wantCode || strings.Contains(output, "work root:") != (mode.name != "oneshot") || strings.Contains(output, "AI preflight:") || strings.Contains(output, "Git preflight:") {
				t.Fatalf("terminal discard was blocked by new-work preflight: exit=%d log=%q", code, output)
			}
			assertNoStartupCommands(t, markers)
			store, err = state.Open(statePath, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if recovered := store.Data.Accepted == nil && !store.Data.Processed[accepted.JobID].IsZero() && len(store.Data.JobFailures) == 0; recovered != mode.wantRecovered {
				t.Fatalf("terminal acceptance recovery = %t, want %t: %+v", recovered, mode.wantRecovered, store.Data)
			}
			if mode.name == "check" && (store.Data.Accepted == nil || store.Data.Accepted.AttemptID != accepted.AttemptID || store.Data.JobFailures[accepted.JobID].Attempts != 1) {
				t.Fatalf("-check changed accepted action: %+v", store.Data)
			}
		})
	}
}

func TestPreflightFailureRecoversAcceptanceWhoseDeadlinePassed(t *testing.T) {
	for _, mode := range []struct {
		name string
		args []string
	}{
		{name: "oneshot", args: []string{"-mode=orchestrate", "-oneshot"}},
		{name: "service", args: []string{"-mode=orchestrate"}},
		{name: "check", args: []string{"-mode=orchestrate", "-check"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			isolateHostPaths(t, t.TempDir())
			markers := startupCommandMarkers(t)
			t.Setenv("GH_TOKEN", "offline-token")
			for _, key := range []string{"GITHUB_TOKEN", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
				t.Setenv(key, "")
			}
			statePath := filepath.Join(t.TempDir(), "state.json")
			cfg := config.Config{BotLogin: "bot", GitHub: config.GitHubConfig{Token: "offline-token"}, StatePath: statePath, WorkRoot: t.TempDir(),
				Commands:       map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
				AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"comment"},
					AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7}}},
			}
			var requests []string
			var reply string
			previousTransport := http.DefaultTransport
			http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
				key := request.Method + " " + request.URL.Path
				requests = append(requests, key)
				recorder := httptest.NewRecorder()
				recorder.Header().Set("Content-Type", "application/json")
				switch key {
				case "GET /user":
					_, _ = recorder.WriteString(`{"id":8,"login":"bot"}`)
				case "GET /repos/owner/repo":
					_, _ = recorder.WriteString(`{"id":1,"full_name":"owner/repo","default_branch":"main","owner":{"id":2}}`)
				case "GET /repos/owner/repo/issues/7/comments":
					_, _ = recorder.WriteString(`[]`)
				case "POST /graphql":
					_, _ = recorder.WriteString(`{"data":{"node":{"body":"@bot comment","author":{"id":7,"login":"writer"}}}}`)
				case "GET /repos/owner/repo/collaborators/writer/permission":
					_, _ = recorder.WriteString(`{"permission":"write","user":{"id":7,"login":"writer"}}`)
				case "POST /repos/owner/repo/issues/7/comments":
					var input struct {
						Body string `json:"body"`
					}
					if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					reply = input.Body
					recorder.WriteHeader(http.StatusCreated)
					_, _ = recorder.WriteString(`{"id":42}`)
				default:
					t.Errorf("unexpected GitHub request: %s", key)
					return nil, errors.New("unexpected GitHub request")
				}
				return recorder.Result(), nil
			})
			t.Cleanup(func() { http.DefaultTransport = previousTransport })

			now := time.Now().UTC()
			accepted := state.AcceptedAction{MainProvider: "codex", MainSettings: frozenTestCodex,
				JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "local",
				Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
				SubjectType: "PullRequest", SubjectNumber: 7,
				SourceKey: "comment:1", SourceNodeID: "source-node", SourceURL: "https://github.com/owner/repo/pull/7#issuecomment-1",
				Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
				AcceptedAt: now.Add(-2 * time.Minute), Deadline: now.Add(-time.Minute),
			}
			store, err := state.Open(statePath, now.Add(-2*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			store.Data.Accepted = &accepted
			store.Data.JobFailures[accepted.JobID] = frozenFailureForTest(accepted)
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			fakeNow := accepted.Deadline.Add(-time.Second)
			if terminalRecoveryPendingAt(store, fakeNow) {
				t.Fatal("acceptance was terminal before preflight")
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			gitChecks := 0
			code, output := runStartupFixtureWithRun(t, cfg, func() int {
				return runWithGitCheckAt(func(context.Context, string) error {
					gitChecks++
					if !fakeNow.Before(accepted.Deadline) {
						t.Fatal("deadline passed before Git preflight")
					}
					fakeNow = accepted.Deadline.Add(time.Second)
					return errors.New("fixture Git failure")
				}, func() time.Time { return fakeNow })
			}, mode.args...)
			if code != 1 || gitChecks != 1 || !strings.Contains(output, "Git preflight: fixture Git failure") || strings.Contains(output, "AI preflight:") || strings.Contains(output, "start selected worker:") {
				t.Fatalf("startup lost preflight failure: exit=%d git_checks=%d log=%q", code, gitChecks, output)
			}
			assertNoStartupCommands(t, markers)
			store, err = state.Open(statePath, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if mode.name == "check" {
				if len(requests) != 0 || reply != "" || store.Data.Accepted == nil || store.Data.Accepted.AttemptID != accepted.AttemptID || !store.Data.Accepted.Deadline.Equal(accepted.Deadline) || store.Data.JobFailures[accepted.JobID].Attempts != 1 || len(store.Data.Processed) != 0 {
					t.Fatalf("-check recovered accepted action: requests=%v reply=%q state=%+v", requests, reply, store.Data)
				}
				return
			}
			if len(requests) == 0 || !strings.Contains(reply, "Minion could not complete this request") || !strings.HasSuffix(reply, "<!-- linkdataminion:job-1 -->") || store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || len(store.Data.JobFailures) != 0 {
				t.Fatalf("expired acceptance was not recovered: requests=%v reply=%q state=%+v", requests, reply, store.Data)
			}
		})
	}
}

func TestRunChecksGitBeforeProvidersAndGitHub(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell Git wrapper fixture is unavailable on native Windows")
	}
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []struct {
		name string
		args []string
	}{
		{name: "oneshot", args: []string{"-oneshot"}}, {name: "check", args: []string{"-check"}}, {name: "service"},
	} {
		for _, gitMode := range []string{"old", "failed", "real"} {
			t.Run(mode.name+"/"+gitMode, func(t *testing.T) {
				isolateHostPaths(t, t.TempDir())
				for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
					t.Setenv(key, "")
				}
				markers := startupCommandMarkers(t)
				bin := os.Getenv("PATH")
				script := "#!/bin/sh\nprintf 'called\\n' >> \"$0.called\"\n"
				switch gitMode {
				case "old":
					script += "case \" $* \" in *\" --version \"*) printf 'git version 2.45.0\\n'; exit 0;; esac\nprintf 'unexpected old-Git capability invocation' >&2\nexit 91\n"
				case "failed":
					script += "printf 'fixture Git failure' >&2\nexit 37\n"
				case "real":
					if err := os.Symlink(gitBinary, filepath.Join(bin, "git.real")); err != nil {
						t.Fatal(err)
					}
					script += "exec \"$0.real\" \"$@\"\n"
				}
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				statePath := filepath.Join(t.TempDir(), "state.json")
				cfg := config.Config{Main: "codex", BotLogin: "fixture", StatePath: statePath,
					WorkRoot: t.TempDir(),
					Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
					Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
				}
				code, output := runStartupFixture(t, cfg, mode.args...)
				if code != 1 {
					t.Fatalf("startup continued after %s preflight failure: exit=%d log=%q", gitMode, code, output)
				}
				wantStage, wantCause := "Git preflight:", "fixture Git failure"
				if gitMode == "old" {
					wantCause = "2.45.0"
				}
				if gitMode == "real" {
					wantStage, wantCause = "AI preflight:", "fixture preflight failure"
				}
				if !strings.Contains(output, wantStage) || !strings.Contains(output, wantCause) || (gitMode != "real" && strings.Contains(output, "AI preflight:")) || strings.Contains(output, "GitHub authentication:") {
					t.Fatalf("startup lost preflight order or concrete cause: %s", output)
				}
				for name, marker := range markers {
					_, err := os.Stat(marker)
					wantCalled := name == "git" || name == "codex" && gitMode == "real"
					if (err == nil) != wantCalled || err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("%s invocation=%t want=%t error=%v log=%q", name, err == nil, wantCalled, err, output)
					}
				}
				store, err := state.Open(statePath, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if len(store.Data.JobFailures) != 0 {
					t.Fatalf("preflight created job failures: %+v", store.Data)
				}
			})
		}
	}
}

func TestCheckRejectsUnverifiedSameProviderPairBeforeWorkerStartup(t *testing.T) {
	cfg := config.Config{BotLogin: "fixture", Main: "claude", Adversarial: "claude",
		Claude:    config.ClaudeConfig{Effort: "medium"},
		StatePath: filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error {
			t.Fatal("unverified role pair reached Git preflight")
			return nil
		})
	}, "-check")
	if code != 1 || !strings.Contains(output, "same-provider native role pairing is not verified") {
		t.Fatalf("unverified pair passed -check: code=%d output=%q", code, output)
	}
}

func TestInstallSkillsBypassesGitAndProviderPreflight(t *testing.T) {
	for _, extra := range []string{"", "-check", "-oneshot"} {
		t.Run("extra="+extra, func(t *testing.T) {
			isolateHostPaths(t, t.TempDir())
			markers := startupCommandMarkers(t)
			root := t.TempDir()
			cfg := config.Config{BotLogin: "fixture", StatePath: filepath.Join(root, "state.json"), WorkRoot: filepath.Join(root, "not-mounted"),
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
			}
			args := []string{"-install-skills"}
			wantCode, wantLog := 0, "configured skills installed for Codex and Claude"
			if extra != "" {
				args = append(args, extra)
				wantCode, wantLog = 2, "-install-skills cannot be combined with -oneshot or -check"
			}
			code, output := runStartupFixture(t, cfg, args...)
			if code != wantCode || !strings.Contains(output, wantLog) {
				t.Fatalf("install-only shortcut changed: exit=%d log=%q", code, output)
			}
			assertNoStartupCommands(t, markers)
			for _, path := range []string{cfg.StatePath, cfg.StatePath + ".lock"} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("install-only path opened state: path=%s error=%v", path, err)
				}
			}
		})
	}
}

func TestInstallSkillsBeforeFirstStartLeavesConfigUncreated(t *testing.T) {
	home := t.TempDir()
	isolateHostPaths(t, home)
	configPath := filepath.Join(t.TempDir(), "config.local.json")
	oldFlags, oldArgs := flag.CommandLine, os.Args
	t.Cleanup(func() { flag.CommandLine, os.Args = oldFlags, oldArgs })
	flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
	os.Args = []string{"minion", "-config", configPath, "-install-skills"}
	if code := runWithGitCheck(func(context.Context, string) error {
		t.Fatal("install-only reached Git preflight")
		return nil
	}); code != 0 {
		t.Fatalf("install default skills: exit=%d", code)
	}
	if _, err := os.Lstat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("install-only consumed first-start config creation: %v", err)
	}
	for _, provider := range []string{".agents", ".claude"} {
		path := filepath.Join(home, provider, "skills", "security-audit", "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("default skill was not installed at %s: %v", path, err)
		}
	}
}

func TestCheckMissingConfigDoesNotBootstrap(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.local.json")
	logFile, err := os.CreateTemp(t.TempDir(), "stderr-")
	if err != nil {
		t.Fatal(err)
	}
	oldFlags, oldArgs, oldStderr := flag.CommandLine, os.Args, os.Stderr
	t.Cleanup(func() {
		flag.CommandLine, os.Args, os.Stderr = oldFlags, oldArgs, oldStderr
		_ = logFile.Close()
	})
	flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
	os.Args = []string{"minion", "-config", configPath, "-check"}
	os.Stderr = logFile
	if code := runWithGitCheck(func(context.Context, string) error {
		t.Fatal("check with missing config reached Git preflight")
		return nil
	}); code != 2 {
		t.Fatalf("check with missing config: exit=%d", code)
	}
	if _, err := os.Lstat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check created config: %v", err)
	}
	output, err := os.ReadFile(logFile.Name())
	if err != nil || !strings.Contains(string(output), "config:") || !strings.Contains(string(output), "config.local.json") {
		t.Fatalf("check did not report missing config: log=%q error=%v", output, err)
	}
}
