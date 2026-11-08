package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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

func TestServiceOffersRecoveryUIAfterAIPreflightFailure(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	address := occupiedAdminAddress(t)
	for _, tc := range []struct {
		name         string
		args         []string
		wantRecovery bool
	}{{"service", nil, true}, {"check", []string{"-check"}, false}, {"oneshot", []string{"-oneshot"}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Main: "claude", Adversarial: "claude", BotLogin: "bot",
				GitHub: config.GitHubConfig{Token: "configured-token"}, Claude: config.ClaudeConfig{Effort: "medium"},
				StatePath: filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
				Admin:    &config.AdminConfig{Listen: address, Administrators: []config.AdminAccount{testAdminAccount()}},
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
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
		})
	}
}

func TestServiceOffersRecoveryUIAfterGitHubAuthenticationFailure(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	address := occupiedAdminAddress(t)
	certFile, keyFile, _ := remoteTestTLSFiles(t)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/user" {
			t.Errorf("unexpected GitHub request: %s", request.URL)
		}
		recorder := httptest.NewRecorder()
		recorder.WriteHeader(http.StatusUnauthorized)
		return recorder.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	for _, tc := range []struct {
		name         string
		args         []string
		pendingWrite bool
		wantRecovery bool
	}{{"service", nil, false, true}, {"check", []string{"-check"}, false, false},
		{"oneshot", []string{"-oneshot"}, false, false}, {"pending publication", nil, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
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
		})
	}
}
