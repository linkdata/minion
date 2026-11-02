package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
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

func TestRemoteRestartDeliversOriginalAcceptanceOnFreshPull(t *testing.T) {
	offlineModeGitRemote(t)
	base := t.TempDir()
	for _, dir := range []string{"home", "codex-home"} {
		if err := os.Mkdir(filepath.Join(base, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("CODEX_HOME", filepath.Join(base, "codex-home"))
	modelCalls := filepath.Join(base, "model-calls")
	offlineModeCodex(t, modelCalls)
	certFile, keyFile, leaf := remoteTestTLSFiles(t)
	cfg := selectedServiceTestConfig(t)
	cfg.WorkRoot, cfg.StatePath = filepath.Join(base, "work"), filepath.Join(base, "state.json")
	cfg.BotLogin = "linkdataminion"
	cfg.Repositories[0].Name, cfg.Repositories[0].ID, cfg.Repositories[0].OwnerID = "o/r", 1, 2
	cfg.Repositories[0].AllowedActors = []string{"writer"}
	cfg.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
	cfg.RemoteExecutor = &config.RemoteExecutorConfig{
		ExecutorID: "remote", Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile,
		Token: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
	}
	if err := os.Mkdir(cfg.WorkRoot, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	accepted := state.AcceptedAction{
		JobID: "job-1", AttemptID: "0123456789abcdef0123456789abcdef", ExecutorID: "remote",
		Repository: "o/r", RepositoryID: 1, OwnerID: 2, SubjectType: "Issue", SubjectNumber: 7,
		SourceKey: "comment:42", SourceNodeID: "request-node",
		SourceURL: "https://github.com/o/r/issues/7#issuecomment-42", SourceUpdatedAt: now,
		Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
		AcceptedAt: now, Deadline: now.Add(10 * time.Minute),
	}
	store, err := state.Open(cfg.StatePath, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
	store.Data.NextInviteCheckAt, store.Data.NextPollAt = now.Add(time.Hour), now.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(cfg.StatePath, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.Data.Accepted == nil || store.Data.Accepted.Assigned || store.Data.Accepted.AttemptID != accepted.AttemptID ||
		!store.Data.Accepted.Deadline.Equal(accepted.Deadline) {
		t.Fatalf("restart changed the undelivered acceptance: %+v", store.Data.Accepted)
	}

	var mu sync.Mutex
	var published int
	var atPublication state.Data
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer offline-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = fmt.Fprint(w, `{"number":7,"title":"Issue","body":"Describe the source"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprintf(w, `[{"id":42,"node_id":"request-node","body":"@linkdataminion comment","user":{"id":7,"login":"writer"},"created_at":%q,"updated_at":%q,"html_url":%q}]`,
				now.Format(time.RFC3339), now.Format(time.RFC3339), accepted.SourceURL)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/collaborators/writer/permission":
			_, _ = fmt.Fprint(w, `{"permission":"write","user":{"login":"writer","id":7}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var query struct {
				Query string `json:"query"`
			}
			if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
				t.Errorf("decode GraphQL: %v", err)
				http.Error(w, "invalid query", http.StatusBadRequest)
				return
			}
			if strings.Contains(query.Query, "addReaction") {
				_, _ = fmt.Fprint(w, `{"data":{"addReaction":{"subject":{"reactions":{"viewerHasReacted":true}}}}}`)
			} else {
				_, _ = fmt.Fprint(w, `{"data":{"node":{"body":"@linkdataminion comment","author":{"login":"writer","id":7}}}}`)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			var input struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !strings.Contains(input.Body, "No findings.") {
				t.Errorf("invalid publication: body=%q error=%v", input.Body, err)
				http.Error(w, "invalid publication", http.StatusBadRequest)
				return
			}
			data, err := os.ReadFile(cfg.StatePath)
			if err != nil {
				t.Errorf("read assignment before publication: %v", err)
			}
			var snapshot state.Data
			if err == nil {
				if err := json.Unmarshal(data, &snapshot); err != nil {
					t.Errorf("decode assignment before publication: %v", err)
				}
			}
			mu.Lock()
			published++
			atPublication = snapshot
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":99}`)
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := github.New("offline-token")
	client.HTTPClient, client.BaseURL = server.Client(), server.URL
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	owner := newOrchestrator(cfg, client, store, logger, nil)
	owner.BotID = 8
	watcher := &monitor.Monitor{Config: cfg, GitHub: client, BotID: 8, Logger: logger}
	endpoint, err := attachSelectedRemoteWorker(owner, watcher)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	serviceDone := make(chan int, 1)
	go func() {
		serviceDone <- runSelectedRemoteService(ctx, owner, watcher, store, false, logger, endpoint, 10*time.Second)
	}()
	noPull, stopNoPull := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stopNoPull()
	if err := owner.WaitCapacity(noPull); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("restart advertised capacity without an authenticated pull: %v", err)
	}
	select {
	case code := <-serviceDone:
		t.Fatalf("remote service exited before worker connected: code=%d logs=%q", code, logs.String())
	default:
	}
	origin := "https://" + endpoint.Addr().String()
	workerClient, err := action.NewLocalExecutionClient(action.LocalEndpointAccess{
		BaseURL: origin, Certificate: leaf.Raw, Token: cfg.RemoteExecutor.Token,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer workerClient.CloseIdleConnections()
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- action.RunHTTP(ctx, workerClient, origin, "remote", &action.Runner{WorkRoot: cfg.WorkRoot})
	}()
	select {
	case code := <-serviceDone:
		if code != 0 {
			t.Fatalf("remote restart failed: code=%d logs=%q", code, logs.String())
		}
	case <-ctx.Done():
		t.Fatalf("remote restart stalled: %v logs=%q", ctx.Err(), logs.String())
	}
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatalf("remote worker failed: %v logs=%q", err, logs.String())
		}
	case <-ctx.Done():
		t.Fatalf("remote worker stalled: %v logs=%q", ctx.Err(), logs.String())
	}
	data, err := os.ReadFile(modelCalls)
	if err != nil || strings.Count(string(data), "model\n") != 1 {
		t.Fatalf("accepted action ran %d model calls: data=%q error=%v logs=%q", strings.Count(string(data), "model\n"), data, err, logs.String())
	}
	mu.Lock()
	count, checkpoint := published, atPublication
	mu.Unlock()
	if count != 1 || checkpoint.Accepted == nil || !checkpoint.Accepted.Assigned ||
		checkpoint.Accepted.AttemptID != accepted.AttemptID ||
		!checkpoint.Accepted.AcceptedAt.Equal(accepted.AcceptedAt) || !checkpoint.Accepted.Deadline.Equal(accepted.Deadline) ||
		checkpoint.JobFailures[accepted.JobID].Attempts != 1 || store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
		t.Fatalf("remote restart changed attempt or skipped its publication: count=%d checkpoint=%+v state=%+v logs=%q", count, checkpoint, store.Data, logs.String())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(cfg.StatePath, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Data.Accepted != nil || reopened.Data.Processed[accepted.JobID].IsZero() {
		t.Fatalf("remote completion was not durable: %+v", reopened.Data)
	}
}
