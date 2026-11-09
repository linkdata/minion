package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
)

func occupiedAdminAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close occupied admin address: %v", err)
		}
	})
	return listener.Addr().String()
}

func TestSetupPreservesPendingDurableWork(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	address := occupiedAdminAddress(t)
	for _, kind := range []string{"empty", "accepted", "publication"} {
		t.Run(kind, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "state", "state.json")
			at := time.Now().UTC().Add(-time.Minute)
			if kind != "empty" {
				store, err := state.Open(statePath, at)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				switch kind {
				case "accepted":
					accepted := dashboardTestAccepted(at)
					accepted.Assigned = true
					store.Data.Accepted = accepted
					store.Data.JobFailures[accepted.JobID] = frozenFailureForTest(*accepted)
				case "publication":
					store.Data.PublicationWrites["issued-job"] = state.PublicationWrite{
						AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", BotID: 8,
						Repository: "owner/repo", Number: 7, Marker: "<!-- linkdataminion:issued-job -->",
						Since: at, StartedAt: at, Deadline: at.Add(time.Minute), Status: state.PublicationWritePending,
					}
				}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.Config{StatePath: statePath, WorkRoot: t.TempDir(),
				Admin:    &config.AdminConfig{Listen: address, Administrators: []config.AdminAccount{testAdminAccount()}},
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{}}
			code, output := runStartupFixtureWithRun(t, cfg, func() int {
				return runWithGitCheck(func(context.Context, string) error {
					t.Fatal("setup reached Git preflight")
					return nil
				})
			})
			if code != 1 || !strings.Contains(output, "setup UI:") ||
				strings.Contains(output, "AI preflight:") || strings.Contains(output, "GitHub authentication:") ||
				strings.Contains(output, "GitHub identity check:") {
				t.Fatalf("setup with %s state: code=%d log=%q", kind, code, output)
			}
			if kind == "empty" {
				if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("first-run setup created state before start_at can be saved: %v", err)
				}
			}
			reopened, err := state.Open(statePath, at)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if kind == "accepted" && reopened.Data.Accepted == nil ||
				kind == "publication" && reopened.Data.PublicationWrites["issued-job"].Status != state.PublicationWritePending {
				t.Fatalf("pending %s state was lost: %+v", kind, reopened.Data)
			}
		})
	}
}

func TestOneShotRejectsEmptyRepositoriesBeforePreflight(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config.Config{BotLogin: "bot", GitHub: config.GitHubConfig{Token: "configured-token"},
		StatePath: statePath, WorkRoot: t.TempDir(),
		Admin:    &config.AdminConfig{Listen: "127.0.0.1:8377", Administrators: []config.AdminAccount{testAdminAccount()}},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{}}
	gitChecks := 0
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error {
			gitChecks++
			return nil
		})
	}, "-oneshot")
	if code != 1 || gitChecks != 0 || !strings.Contains(output, "no repositories configured") {
		t.Fatalf("one-shot started without repositories: code=%d git_checks=%d log=%q", code, gitChecks, output)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("one-shot created state without repositories: %v", err)
	}
}

func TestServiceOffersRecoveryUIAfterAIPreflightFailure(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	address := occupiedAdminAddress(t)
	for _, tc := range []struct {
		name            string
		args            []string
		wantRecovery    bool
		pendingAccepted bool
	}{{name: "service", wantRecovery: true}, {name: "pending accepted", wantRecovery: true, pendingAccepted: true},
		{name: "check", args: []string{"-check"}}, {name: "oneshot", args: []string{"-oneshot"}}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Main: "claude", Adversarial: "claude", BotLogin: "bot",
				GitHub: config.GitHubConfig{Token: "configured-token"}, Claude: config.ClaudeConfig{Effort: "medium"},
				StatePath: filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
				Admin:    &config.AdminConfig{Listen: address, Administrators: []config.AdminAccount{testAdminAccount()}},
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
			if tc.pendingAccepted {
				store, err := state.Open(cfg.StatePath, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				accepted := dashboardTestAccepted(time.Now().UTC())
				store.Data.Accepted = accepted
				store.Data.JobFailures[accepted.JobID] = frozenFailureForTest(*accepted)
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			code, output := runStartupFixtureWithRun(t, cfg, func() int {
				return runWithGitCheck(func(context.Context, string) error {
					t.Fatal("AI preflight failure reached Git")
					return nil
				})
			}, tc.args...)
			if code != 1 || !strings.Contains(output, "AI preflight:") ||
				strings.Contains(output, "starting admin recovery UI") != tc.wantRecovery ||
				strings.Contains(output, "setup UI:") != tc.wantRecovery {
				t.Fatalf("AI recovery mode: code=%d output=%q", code, output)
			}
			if tc.pendingAccepted {
				store, err := state.Open(cfg.StatePath, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				if store.Data.Accepted == nil {
					t.Fatal("AI recovery UI lost pending accepted work")
				}
			} else {
				if _, err := os.Stat(cfg.StatePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("AI recovery created state before preflight passed: %v", err)
				}
			}
		})
	}
}

func TestServiceOffersRecoveryUIAfterLocalPreflightFailure(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	address := occupiedAdminAddress(t)
	account := testAdminAccount()
	for _, stateKind := range []string{"fresh", "existing"} {
		for _, failure := range []string{"work root", "Git"} {
			for _, mode := range []struct {
				name         string
				args         []string
				wantRecovery bool
			}{{name: "service", wantRecovery: true}, {name: "check", args: []string{"-check"}}, {name: "oneshot", args: []string{"-oneshot"}}} {
				t.Run(stateKind+"/"+failure+"/"+mode.name, func(t *testing.T) {
					statePath := filepath.Join(t.TempDir(), "state.json")
					if stateKind == "existing" {
						func() {
							initial := time.Now().UTC()
							store, err := state.Open(statePath, initial)
							if err != nil {
								t.Fatal(err)
							}
							defer func() {
								if err := store.Close(); err != nil {
									t.Error(err)
								}
							}()
							store.Data.Processed["prior-job"] = initial
							if err := store.Save(); err != nil {
								t.Fatal(err)
							}
						}()
					}
					workRoot := t.TempDir()
					if failure == "work root" {
						workRoot = filepath.Join(workRoot, "not-a-directory")
						if err := os.WriteFile(workRoot, nil, 0600); err != nil {
							t.Fatal(err)
						}
					}
					cfg := config.Config{Main: "codex", BotLogin: "bot", GitHub: config.GitHubConfig{Token: "configured-token"},
						StatePath: statePath, WorkRoot: workRoot,
						Admin:    &config.AdminConfig{Listen: address, Administrators: []config.AdminAccount{account}},
						Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
						Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
					gitChecks := 0
					code, output := runStartupFixtureWithRun(t, cfg, func() int {
						return runWithGitCheck(func(context.Context, string) error {
							gitChecks++
							return errors.New("fixture Git failure")
						})
					}, mode.args...)
					wantGitChecks, wantLog := 1, "Git preflight: fixture Git failure"
					if failure == "work root" {
						wantGitChecks, wantLog = 0, "work root:"
					}
					if code != 1 || gitChecks != wantGitChecks || !strings.Contains(output, wantLog) ||
						strings.Contains(output, "starting admin recovery UI") != mode.wantRecovery ||
						strings.Contains(output, "setup UI:") != mode.wantRecovery {
						t.Fatalf("local preflight recovery: code=%d git_checks=%d log=%q", code, gitChecks, output)
					}
					if stateKind == "fresh" {
						if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("preflight failure created state: %v", err)
						}
					} else {
						store, err := state.Open(statePath, time.Time{})
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := store.Close(); err != nil {
								t.Error(err)
							}
						})
						if store.Data.Processed["prior-job"].IsZero() {
							t.Fatal("preflight failure lost existing state")
						}
					}
				})
			}
		}
	}
}

func TestServiceOffersRecoveryUIAfterGitHubAuthenticationFailure(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	address := occupiedAdminAddress(t)
	certFile, keyFile, _ := remoteTestTLSFiles(t)
	for _, tc := range []struct {
		name           string
		args           []string
		pendingWrite   bool
		wantRecovery   bool
		status         int
		transportError bool
	}{
		{name: "service", wantRecovery: true, status: http.StatusUnauthorized},
		{name: "check", args: []string{"-check"}, status: http.StatusUnauthorized},
		{name: "oneshot", args: []string{"-oneshot"}, status: http.StatusUnauthorized},
		{name: "pending publication", pendingWrite: true, wantRecovery: true, status: http.StatusUnauthorized},
		{name: "GitHub unavailable", wantRecovery: true, status: http.StatusServiceUnavailable},
		{name: "transport error", wantRecovery: true, transportError: true},
		{name: "pending publication while unavailable", pendingWrite: true, wantRecovery: true, status: http.StatusServiceUnavailable},
		{name: "check while unavailable", args: []string{"-check"}, status: http.StatusServiceUnavailable},
		{name: "oneshot while unavailable", args: []string{"-oneshot"}, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousTransport := http.DefaultTransport
			http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/user" {
					t.Errorf("unexpected GitHub request: %s", request.URL)
				}
				if tc.transportError {
					return nil, errors.New("fixture connection failure")
				}
				recorder := httptest.NewRecorder()
				recorder.WriteHeader(tc.status)
				return recorder.Result(), nil
			})
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			cfg := config.Config{Main: "codex", BotLogin: "bot", GitHub: config.GitHubConfig{Token: "bad-token"},
				Codex:     config.CodexConfig{Model: "fixture-model", ReasoningEffort: "medium"},
				StatePath: filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
				Admin: &config.AdminConfig{Listen: address, Administrators: []config.AdminAccount{testAdminAccount()}},
				RemoteExecutor: &config.RemoteExecutorConfig{ExecutorID: "remote", Listen: "127.0.0.1:8443",
					TLSCertFile: certFile, TLSKeyFile: keyFile, Token: strings.Repeat("A", 43)},
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
			if tc.pendingWrite {
				store, err := state.Open(cfg.StatePath, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				at := time.Now().UTC()
				store.Data.PublicationWrites["issued-job"] = state.PublicationWrite{AttemptID: "attempt-1", Kind: "comment",
					BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 7,
					Marker: "<!-- linkdataminion:issued-job -->", Since: at, StartedAt: at, Deadline: at.Add(time.Minute),
					Status: state.PublicationWritePending}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			code, output := runStartupFixtureWithRun(t, cfg, func() int {
				return runWithGitCheck(func(context.Context, string) error { return nil })
			}, tc.args...)
			if code != 1 || !strings.Contains(output, "GitHub identity check:") ||
				strings.Contains(output, "starting admin recovery UI") != tc.wantRecovery ||
				strings.Contains(output, "setup UI:") != tc.wantRecovery {
				t.Fatalf("GitHub recovery mode: code=%d output=%q", code, output)
			}
			if !tc.pendingWrite {
				if _, err := os.Stat(cfg.StatePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("GitHub recovery created state before preflight passed: %v", err)
				}
			} else if tc.pendingWrite {
				store, err := state.Open(cfg.StatePath, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				if store.Data.PublicationWrites["issued-job"].Status != state.PublicationWritePending {
					t.Fatal("GitHub recovery UI lost pending publication")
				}
			}
		})
	}
}

func TestFreshCheckDoesNotSaveStateAfterRepositoryFailure(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	certFile, keyFile, _ := remoteTestTLSFiles(t)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		if request.URL.Path == "/user" {
			_, _ = recorder.WriteString(`{"id":8,"login":"bot"}`)
		} else {
			recorder.WriteHeader(http.StatusForbidden)
		}
		return recorder.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config.Config{Main: "codex", BotLogin: "bot", GitHub: config.GitHubConfig{Token: "configured-token"},
		Codex:     config.CodexConfig{Model: "fixture-model", ReasoningEffort: "medium"},
		StatePath: statePath, WorkRoot: t.TempDir(),
		Admin: &config.AdminConfig{Listen: "127.0.0.1:8377", Administrators: []config.AdminAccount{testAdminAccount()}},
		RemoteExecutor: &config.RemoteExecutorConfig{ExecutorID: "remote", Listen: "127.0.0.1:8443",
			TLSCertFile: certFile, TLSKeyFile: keyFile, Token: strings.Repeat("A", 43)},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error { return nil })
	}, "-check")
	if code != 1 || !strings.Contains(output, "preflight failed:") {
		t.Fatalf("repository preflight: code=%d log=%q", code, output)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed -check saved notification cursor: %v", err)
	}
}
