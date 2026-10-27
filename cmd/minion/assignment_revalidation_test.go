package main

import (
	"bytes"
	"context"
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
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
)

// The accepted command is changed while the parent is preparing its checkout.
// The real execute-mode child must not reach its model unless the final check passes.
func TestSelectedWorkerRevalidatesAfterPreparation(t *testing.T) {
	for _, change := range []string{"valid", "edited", "revoked", "replaced repository", "transient lookup"} {
		t.Run(change, func(t *testing.T) {
			offlineModeGitRemote(t)
			base := t.TempDir()
			for _, dir := range []string{"home", "codex-home", "work"} {
				if err := os.Mkdir(filepath.Join(base, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", filepath.Join(base, "home"))
			t.Setenv("USERPROFILE", filepath.Join(base, "home"))
			t.Setenv("CODEX_HOME", filepath.Join(base, "codex-home"))
			modelCalls := filepath.Join(base, "model-calls")
			offlineModeCodex(t, modelCalls)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			worker := offlineModeWorkerPath(t, binary)

			at := time.Now().UTC().Add(-time.Minute)
			accepted := state.AcceptedAction{
				JobID: "job-7", AttemptID: "0123456789abcdef0123456789abcdef", ExecutorID: action.LocalExecutorID,
				Repository: "o/r", RepositoryID: 1, OwnerID: 2,
				SubjectType: "Issue", SubjectNumber: 7,
				SourceKey: "comment:42", SourceNodeID: "request-node", SourceURL: "https://github.com/o/r/issues/7#issuecomment-42",
				SourceUpdatedAt: at, Requester: "writer", RequesterID: 7,
				CommandName: "comment", Action: "comment", AcceptedAt: at, Deadline: at.Add(10 * time.Minute),
			}
			statePath := filepath.Join(base, "state.json")
			store, err := state.Open(statePath, at.Add(-time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			store.Data.Accepted = &accepted
			store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline}
			store.Data.NextInviteCheckAt = at.Add(time.Hour)
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}

			var mu sync.Mutex
			body, permission, repoID := "@linkdataminion comment", "write", int64(1)
			issueReads, publications := 0, 0
			preparing, resume := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer offline-token" {
					http.Error(w, "missing token", http.StatusUnauthorized)
					return
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
					mu.Lock()
					id := repoID
					mu.Unlock()
					_, _ = fmt.Fprintf(w, `{"id":%d,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":"main"}`, id)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
					mu.Lock()
					issueReads++
					pause := issueReads == 2
					mu.Unlock()
					if pause {
						close(preparing)
						select {
						case <-resume:
						case <-r.Context().Done():
							return
						}
					}
					_, _ = fmt.Fprint(w, `{"number":7,"title":"Issue","body":"Describe source"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
					_, _ = fmt.Fprint(w, `[]`)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/collaborators/writer/permission":
					mu.Lock()
					p := permission
					mu.Unlock()
					if p == "transient" {
						http.Error(w, "temporary failure", http.StatusServiceUnavailable)
						return
					}
					_, _ = fmt.Fprintf(w, `{"permission":%q,"user":{"login":"writer","id":7}}`, p)
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					var input struct {
						Query string `json:"query"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						http.Error(w, "bad query", http.StatusBadRequest)
						return
					}
					if strings.Contains(input.Query, "addReaction") {
						_, _ = fmt.Fprint(w, `{"data":{"addReaction":{"subject":{"reactions":{"viewerHasReacted":true}}}}}`)
						return
					}
					mu.Lock()
					current := body
					mu.Unlock()
					if current == "edited" {
						_, _ = fmt.Fprint(w, `{"data":{"node":{"body":"edited","author":{"login":"writer","id":7},"lastEditedAt":"2026-10-01T00:00:00Z"}}}`)
					} else {
						_, _ = fmt.Fprintf(w, `{"data":{"node":{"body":%q,"author":{"login":"writer","id":7}}}}`, current)
					}
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
					mu.Lock()
					publications++
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
			cfg := config.Config{WorkRoot: filepath.Join(base, "work"), StatePath: statePath,
				BotLogin: "linkdataminion", Main: "codex", Codex: config.CodexConfig{Model: "main-1", ReasoningEffort: "high"},
				Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
				Repositories: []config.Repository{{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"comment"},
					AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7}}},
			}
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)
			owner := newOrchestrator(cfg, client, store, logger, nil)
			watcher := &monitor.Monitor{Config: cfg, GitHub: client, Logger: logger}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			finished := make(chan int, 1)
			go func() {
				finished <- runOrchestrateMode(ctx, owner, watcher, store, false, logger, worker, 20*time.Second)
			}()
			select {
			case <-preparing:
			case <-ctx.Done():
				t.Fatalf("preparation never paused: %v; log=%s", ctx.Err(), logs.String())
			}
			mu.Lock()
			switch change {
			case "edited":
				body = "edited"
			case "revoked":
				permission = "read"
			case "replaced repository":
				repoID = 99
			case "transient lookup":
				permission = "transient"
			}
			mu.Unlock()
			close(resume)
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatalf("selected service did not finish: %v; log=%s", ctx.Err(), logs.String())
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = state.Open(statePath, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			modelData, err := os.ReadFile(modelCalls)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			mu.Lock()
			posted := publications
			mu.Unlock()
			if change == "valid" {
				if strings.Count(string(modelData), "model\n") != 1 || posted != 1 || store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
					t.Fatalf("valid assignment did not complete: model=%q posted=%d state=%+v log=%s", modelData, posted, store.Data, logs.String())
				}
				return
			}
			if len(modelData) != 0 || posted != 0 {
				t.Fatalf("invalid or unreadable assignment reached model/publication: model=%q posted=%d log=%s", modelData, posted, logs.String())
			}
			if change == "transient lookup" {
				if store.Data.Accepted == nil || store.Data.Accepted.Assigned || store.Data.Accepted.AttemptID != accepted.AttemptID ||
					!store.Data.Accepted.Deadline.Equal(accepted.Deadline) || store.Data.JobFailures[accepted.JobID].Attempts != 1 {
					t.Fatalf("transient lookup lost original attempt: %+v; log=%s", store.Data, logs.String())
				}
				mu.Lock()
				permission = "write"
				mu.Unlock()
				store.Data.Accepted.RetryAt = time.Now().Add(-time.Second)
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				restarted := newOrchestrator(cfg, client, store, logger, nil)
				freshMonitor := &monitor.Monitor{Config: cfg, GitHub: client, Logger: logger}
				if code := runOrchestrateMode(t.Context(), restarted, freshMonitor, store, false, logger, worker, 20*time.Second); code != 0 {
					t.Fatalf("restart did not reuse the accepted action: exit=%d log=%s", code, logs.String())
				}
				modelData, err = os.ReadFile(modelCalls)
				if err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				posted = publications
				mu.Unlock()
				if strings.Count(string(modelData), "model\n") != 1 || posted != 1 || store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
					t.Fatalf("restart changed attempt or skipped delivery: model=%q posted=%d state=%+v log=%s", modelData, posted, store.Data, logs.String())
				}
			} else if store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || len(store.Data.JobFailures) != 0 {
				t.Fatalf("invalid assignment was not durably discarded: %+v; log=%s", store.Data, logs.String())
			}
		})
	}
}
