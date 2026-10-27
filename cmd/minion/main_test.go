package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
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

func TestCronPassSkipsPreflightWhenStateLocked(t *testing.T) {
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
		{name: "cron"}, {name: "check", args: []string{"-check"}, code: 1}, {name: "loop", args: []string{"-loop"}, code: 1},
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

func TestTerminalRecoveryBypassesNewWorkPreflight(t *testing.T) {
	for _, mode := range []struct {
		name          string
		args          []string
		wantCode      int
		wantRecovered bool
	}{
		{name: "cron", args: []string{"-mode=orchestrate"}, wantRecovered: true},
		{name: "loop", args: []string{"-mode=orchestrate", "-loop"}, wantCode: 1, wantRecovered: true},
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
			accepted := state.AcceptedAction{
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
			store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
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
			cfg := config.Config{BotLogin: "bot", StatePath: statePath, WorkRoot: badWorkRoot,
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
			code, output := runStartupFixtureWithRun(t, cfg, func() int {
				return runWithGitCheck(func(context.Context, string) error {
					t.Error("terminal recovery reached Git preflight")
					return errors.New("unexpected Git preflight")
				})
			}, mode.args...)
			if code != mode.wantCode || strings.Contains(output, "work root:") != (mode.name != "cron") || strings.Contains(output, "AI preflight:") || strings.Contains(output, "Git preflight:") {
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
		{name: "cron", args: []string{"-mode=orchestrate"}},
		{name: "loop", args: []string{"-mode=orchestrate", "-loop"}},
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
			cfg := config.Config{BotLogin: "bot", StatePath: statePath, WorkRoot: t.TempDir(),
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
			accepted := state.AcceptedAction{
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
			store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
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
		{name: "cron"}, {name: "check", args: []string{"-check"}}, {name: "loop", args: []string{"-loop"}},
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

func TestInstallSkillsBypassesGitAndProviderPreflight(t *testing.T) {
	for _, extra := range []string{"", "-check", "-loop"} {
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
				wantCode, wantLog = 2, "-install-skills cannot be combined with -loop or -check"
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
