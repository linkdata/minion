package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func TestRemoteAcceptedCleanupFailureClosesHeldPull(t *testing.T) {
	certFile, keyFile, leaf := remoteTestTLSFiles(t)
	cfg := selectedServiceTestConfig(t)
	cfg.Repositories[0].ID, cfg.Repositories[0].OwnerID = 1, 2
	cfg.Repositories[0].AllowedActors = []string{"writer"}
	cfg.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
	cfg.RemoteExecutor = &config.RemoteExecutorConfig{
		ExecutorID: "remote", Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile,
		Token: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
	}
	now := time.Now().UTC()
	accepted := state.AcceptedAction{MainProvider: "codex", MainSettings: frozenTestCodex,
		JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "remote",
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
		SubjectType: "Issue", SubjectNumber: 7,
		SourceKey: "comment:1", SourceNodeID: "source-node",
		SourceURL: "https://github.com/owner/repo/issues/7#issuecomment-1", SourceUpdatedAt: now,
		Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
		AcceptedAt: now.Add(-time.Minute), Deadline: now.Add(time.Hour), Assigned: true,
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(statePath, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = frozenFailureForTest(accepted)
	store.Data.NextInviteCheckAt, store.Data.NextPollAt = now.Add(time.Hour), now.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath+".result", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath+".result", "held"), []byte("operator evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	releaseReply := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseReply) })
	defer release()
	gh := &remoteRecoveryLoopGitHub{terminalRecoveryGitHub: &terminalRecoveryGitHub{item: github.Item{
		Key: accepted.SourceKey, NodeID: accepted.SourceNodeID, HTMLURL: accepted.SourceURL,
		UpdatedAt: now, Body: "@bot comment", Author: github.User{Login: "writer", ID: 7},
	}}, resumed: make(chan struct{}, 1), replyEntered: make(chan struct{}, 1), releaseReply: releaseReply}
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	owner := newOrchestrator(cfg, nil, store, logger, nil)
	watcher := &monitor.Monitor{Config: cfg, GitHub: gh, BotID: 8, Logger: logger}
	endpoint, err := attachSelectedRemoteWorker(owner, watcher)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}}
	t.Cleanup(client.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	fetchRemotePreflight(t, ctx, client, endpoint, cfg.RemoteExecutor.Token)
	serviceDone := make(chan int, 1)
	go func() {
		serviceDone <- runSelectedRemoteService(ctx, owner, watcher, store, true, logger, endpoint, 5*time.Second, nil)
	}()
	select {
	case <-gh.replyEntered:
	case code := <-serviceDone:
		t.Fatalf("remote service exited before terminal reply: code=%d logs=%q", code, logs.String())
	case <-ctx.Done():
		t.Fatalf("terminal reply did not begin: %v logs=%q", ctx.Err(), logs.String())
	}
	type pullResult struct {
		status int
		err    error
	}
	pullDone := make(chan pullResult, 1)
	go func() {
		body := fmt.Sprintf(`{"version":%d,"executor_id":"remote"}`, action.ProtocolVersion)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+endpoint.Addr().String()+"/next", strings.NewReader(body))
		if err != nil {
			pullDone <- pullResult{err: err}
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+cfg.RemoteExecutor.Token)
		response, err := client.Do(request)
		if err != nil {
			pullDone <- pullResult{err: err}
			return
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		pullDone <- pullResult{status: response.StatusCode, err: errors.Join(readErr, closeErr)}
	}()
	readyCtx, stopReady := context.WithTimeout(ctx, 5*time.Second)
	defer stopReady()
	if err := owner.WaitCapacity(readyCtx); err != nil {
		t.Fatalf("authenticated pull was not held before cleanup: %v", err)
	}
	release()
	select {
	case code := <-serviceDone:
		if code != 1 {
			t.Fatalf("accepted cleanup failure returned code %d; logs=%q", code, logs.String())
		}
	case <-ctx.Done():
		t.Fatalf("accepted cleanup failure left service running: %v logs=%q", ctx.Err(), logs.String())
	}
	if !strings.Contains(logs.String(), "clear terminal result") || store.Data.Accepted != nil ||
		store.Data.Processed[accepted.JobID].IsZero() || store.Data.TerminalDecisions[accepted.JobID].Outcome != "lost" || gh.reply == "" {
		t.Fatalf("accepted cleanup failure did not settle durably: state=%+v reply=%q logs=%q", store.Data, gh.reply, logs.String())
	}
	select {
	case <-endpoint.Done():
	default:
		t.Fatal("cleanup failure left remote endpoint serving")
	}
	select {
	case pulled := <-pullDone:
		if pulled.status != 0 && pulled.status != http.StatusConflict {
			t.Fatalf("failed cleanup retired or delivered the held pull: status=%d error=%v", pulled.status, pulled.err)
		}
		if pulled.status == 0 && pulled.err == nil {
			t.Fatal("held pull ended without a shutdown response or transport error")
		}
	case <-ctx.Done():
		t.Fatalf("held pull stayed open after endpoint shutdown: %v", ctx.Err())
	}
	if err := owner.WaitCapacity(ctx); !errors.Is(err, action.ErrRemoteWorkerUnavailable) {
		t.Fatalf("closed endpoint advertised capacity: %v", err)
	}
	if _, err := os.Stat(filepath.Join(statePath+".result", "held")); err != nil {
		t.Fatalf("cleanup failure lost sidecar evidence: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(statePath, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Data.Accepted != nil || reopened.Data.Processed[accepted.JobID].IsZero() ||
		reopened.Data.TerminalDecisions[accepted.JobID].Outcome != "lost" {
		t.Fatalf("cleanup failure reopened the repository reservation: %+v", reopened.Data)
	}
}
