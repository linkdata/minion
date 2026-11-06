package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/config"
)

func TestLoadRemoteWorkerConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remote.json")
	cfg := remoteWorkerConfig{BaseURL: "https://worker.example:8443", ExecutorID: "remote-1", Token: "token", WorkRoot: "work"}
	cfg.Claude.AllowAllUnixSockets = true
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadRemoteWorkerConfig(path)
	if err != nil || loaded.WorkRoot != filepath.Join(dir, "work") || loaded.BaseURL != cfg.BaseURL || loaded.ExecutorID != cfg.ExecutorID || !loaded.Claude.AllowAllUnixSockets {
		t.Fatalf("load remote worker config: %+v, %v", loaded, err)
	}
	for name, body := range map[string]string{
		"unknown field": `{"executor_id":"remote-1","work_root":"work","extra":true}`,
		"trailing data": `{"executor_id":"remote-1","work_root":"work"}{}`,
		"invalid ID":    `{"executor_id":"bad/id","work_root":"work"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadRemoteWorkerConfig(path); err == nil {
				t.Fatal("invalid remote worker config accepted")
			}
		})
	}
}

func TestRemoteExecuteModeRejectsServiceFlags(t *testing.T) {
	for _, flag := range []string{"-loop", "-check", "-install-skills", "-loop=false", "-check=false", "-install-skills=false"} {
		t.Run(flag, func(t *testing.T) {
			code, output := runStartupFixture(t, config.Config{}, "-mode=remote-execute", flag)
			if code != 2 || !strings.Contains(output, "cannot be combined") {
				t.Fatalf("remote worker accepted %s: code=%d output=%q", flag, code, output)
			}
		})
	}
}

func TestRemoteExecuteChecksWorkerGitBeforePull(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	configPath := filepath.Join(t.TempDir(), "remote.json")
	cfg := remoteWorkerConfig{
		BaseURL: "https://127.0.0.1:1", ExecutorID: "remote", Token: token, WorkRoot: t.TempDir(),
	}
	cfg.Claude.AllowAllUnixSockets = true
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	var logs bytes.Buffer
	if code := runRemoteExecuteMode(configPath, log.New(&logs, "", 0)); code != 1 || !strings.Contains(logs.String(), "remote worker Git preflight: git readiness requires git") {
		t.Fatalf("missing worker Git reached the pull: exit=%d logs=%q", code, logs.String())
	}
	wantWarnings := 0
	if runtime.GOOS == "linux" {
		wantWarnings = 1
	}
	if got := strings.Count(logs.String(), "Claude Unix-socket opt-in enabled"); got != wantWarnings {
		t.Fatalf("socket opt-in warnings = %d, want %d: %q", got, wantWarnings, logs.String())
	}
}

type remoteWorkerFailureTransport struct{ err error }

func (t remoteWorkerFailureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func TestRemoteWorkerPreflightLogsOnlySafeFailure(t *testing.T) {
	const privateText = "private transport failure text"
	client := &http.Client{Transport: remoteWorkerFailureTransport{err: errors.New(privateText)}}
	var logs bytes.Buffer
	code := runRemoteWorkerPreflightAndLoop(t.Context(), remoteWorkerConfig{
		BaseURL: "https://worker.invalid", ExecutorID: "remote", WorkRoot: t.TempDir(),
	}, client, log.New(&logs, "", 0))
	if code != 1 || logs.String() != "remote worker: selection preflight failed: remote selection preflight unavailable\n" ||
		strings.Contains(logs.String(), privateText) {
		t.Fatalf("remote preflight exposed transport text: exit=%d logs=%q", code, logs.String())
	}
}

func TestRemoteWorkerLoopDoesNotLogPrivateFailureText(t *testing.T) {
	const privateText = "private model discussion returned in a worker error"
	client := &http.Client{Transport: remoteWorkerFailureTransport{err: errors.New(privateText)}}
	var logs bytes.Buffer
	cfg := remoteWorkerConfig{
		BaseURL: "https://worker.invalid", ExecutorID: "remote", WorkRoot: t.TempDir(),
	}
	code := runRemoteWorkerLoop(t.Context(), cfg, client, log.New(&logs, "", 0))
	if code != 1 || logs.String() != "remote worker: execution or protocol failure\n" || strings.Contains(logs.String(), privateText) {
		t.Fatalf("remote worker exposed untrusted error text: exit=%d logs=%q", code, logs.String())
	}
}

type stalledPreflightBody struct{ ctx context.Context }

func (b stalledPreflightBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (stalledPreflightBody) Close() error { return nil }

func TestRemoteWorkerStalledPreflightNeverPulls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var paths []string
		client := &http.Client{Transport: startupTransport(func(request *http.Request) (*http.Response, error) {
			paths = append(paths, request.URL.Path)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: stalledPreflightBody{request.Context()}, Request: request}, nil
		})}
		var logs bytes.Buffer
		start := time.Now()
		code := runRemoteWorkerPreflightAndLoop(t.Context(), remoteWorkerConfig{
			BaseURL: "https://worker.invalid", ExecutorID: "remote", WorkRoot: t.TempDir(),
		}, client, log.New(&logs, "", 0))
		if code != 1 || !strings.Contains(logs.String(), "selection preflight failed: remote selection preflight timed out") ||
			len(paths) != 1 || paths[0] != "/preflight" || time.Since(start) > time.Minute {
			t.Fatalf("stalled preflight reached worker pull or exceeded startup bound: code=%d paths=%v elapsed=%s logs=%q", code, paths, time.Since(start), logs.String())
		}
	})
}

func TestRemoteCheckRejectsMissingTLSFiles(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	cfg := config.Config{
		BotLogin: "fixture", Main: "codex", Codex: config.CodexConfig{Model: "fixture-model", ReasoningEffort: "medium"},
		StatePath: filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
		RemoteExecutor: &config.RemoteExecutorConfig{ExecutorID: "remote", Listen: "127.0.0.1:8443",
			TLSCertFile: filepath.Join(t.TempDir(), "missing-cert.pem"), TLSKeyFile: filepath.Join(t.TempDir(), "missing-key.pem"), Token: token},
	}
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error { return nil })
	}, "-check")
	if code != 1 || !strings.Contains(output, "remote TLS preflight:") {
		t.Fatalf("remote -check missed absent TLS files: code=%d output=%q", code, output)
	}
}
