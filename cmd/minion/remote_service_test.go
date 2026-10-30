package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
)

func TestSelectedRemoteServiceUsesAuthenticatedPullAndStopsWithEndpoint(t *testing.T) {
	certFile, keyFile, leaf := remoteTestTLSFiles(t)
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	commands := map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
	cfg := config.Config{Main: "codex", Commands: commands,
		Repositories:   []config.Repository{{Name: "owner/repo", Actions: []string{"comment"}}},
		RemoteExecutor: &config.RemoteExecutorConfig{ExecutorID: "remote", Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile, Token: token}}
	owner := &action.Orchestrator{Main: "codex", Commands: commands}
	watcher := &monitor.Monitor{Config: cfg}
	endpoint, err := attachSelectedRemoteWorker(owner, watcher)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	if watcher.Handler != owner || watcher.SelectedExecutorID != "remote" || !watcher.SelectedCommands["comment"] {
		t.Fatalf("remote service selection missing: executor=%q commands=%v", watcher.SelectedExecutorID, watcher.SelectedCommands)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+endpoint.Addr().String()+"/next",
		strings.NewReader(fmt.Sprintf(`{"version":%d,"executor_id":"remote"}`, action.ProtocolVersion)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	type pullResult struct {
		status int
		body   []byte
		err    error
	}
	pullDone := make(chan pullResult, 1)
	go func() {
		response, err := client.Do(request)
		if err != nil {
			pullDone <- pullResult{err: err}
			return
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr == nil {
			readErr = closeErr
		}
		pullDone <- pullResult{status: response.StatusCode, body: body, err: readErr}
	}()
	if err := owner.WaitCapacity(ctx); err != nil {
		t.Fatalf("authenticated pull did not provide capacity: %v", err)
	}
	now := time.Now().UTC()
	store := serviceStore(t, state.Data{NextInviteCheckAt: now.Add(time.Hour), NextPollAt: now.Add(time.Hour)})
	var logs bytes.Buffer
	if code := runSelectedRemoteService(ctx, owner, watcher, store, false, log.New(&logs, "", 0), endpoint, time.Second); code != 0 {
		t.Fatalf("remote one-pass service failed: code=%d logs=%q", code, logs.String())
	}
	<-endpoint.Done()
	got := <-pullDone
	if got.status != http.StatusConflict || got.err != nil || !bytes.Contains(got.body, []byte(`"kind":"next"`)) || !bytes.Contains(got.body, []byte(`"failure"`)) {
		t.Fatalf("held pull did not receive idle shutdown: status=%d body=%q error=%v", got.status, got.body, got.err)
	}
}

func remoteTestTLSFiles(t *testing.T) (certFile, keyFile string, leaf *x509.Certificate) {
	t.Helper()
	certificateServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := certificateServer.TLS.Certificates[0]
	leaf = certificateServer.Certificate()
	certificateServer.Close()
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, leaf
}

func TestRemoteCheckWithValidTLSDoesNotRequireLocalAI(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	certFile, keyFile, _ := remoteTestTLSFiles(t)
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	cfg := config.Config{
		BotLogin: "fixture", Main: "codex", Codex: config.CodexConfig{Model: "fixture-model", ReasoningEffort: "medium"},
		StatePath: filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
		RemoteExecutor: &config.RemoteExecutorConfig{ExecutorID: "remote", Listen: "127.0.0.1:8443",
			TLSCertFile: certFile, TLSKeyFile: keyFile, Token: token},
	}
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error { return nil })
	}, "-check")
	if code != 1 || !strings.Contains(output, "GitHub authentication:") || strings.Contains(output, "AI preflight:") || strings.Contains(output, "remote TLS preflight:") {
		t.Fatalf("valid remote -check required local AI before GitHub: code=%d output=%q", code, output)
	}
}
