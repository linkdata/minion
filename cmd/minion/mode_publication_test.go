package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func TestStartupReconcilesIssuedWriteAfterGitPreflightFailure(t *testing.T) {
	for _, mode := range []string{"in-process", "orchestrate"} {
		t.Run(mode, func(t *testing.T) {
			isolateHostPaths(t, t.TempDir())
			t.Setenv("GH_TOKEN", "fixture-token")
			at := time.Now().UTC().Add(-time.Minute)
			statePath := filepath.Join(t.TempDir(), "state.json")
			cfg := config.Config{BotLogin: "bot", StatePath: statePath, WorkRoot: t.TempDir(),
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "o/r", ID: 1, OwnerID: 2}},
			}
			store, err := state.Open(statePath, at)
			if err != nil {
				t.Fatal(err)
			}
			marker := "<!-- linkdataminion:issued-job -->"
			store.Data.PublicationWrites = map[string]state.PublicationWrite{"issued-job": {
				AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", Repository: "o/r", Number: 7,
				Marker: marker, Since: at, StartedAt: at, Deadline: at.Add(2 * time.Minute), Status: state.PublicationWritePending,
			}}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			var stages []string
			body := "published\n\n" + marker
			previousTransport := http.DefaultTransport
			http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
				key := request.Method + " " + request.URL.Path
				stages = append(stages, key)
				recorder := httptest.NewRecorder()
				recorder.Header().Set("Content-Type", "application/json")
				switch key {
				case "GET /user":
					_, _ = recorder.WriteString(`{"id":8,"login":"bot"}`)
				case "GET /repos/o/r/issues/7/comments":
					_, _ = fmt.Fprintf(recorder, `[{"id":1,"node_id":"reply-node","body":%q,"user":{"login":"bot"},"updated_at":%q}]`, body, time.Now().UTC().Format(time.RFC3339))
				case "POST /graphql":
					_, _ = fmt.Fprintf(recorder, `{"data":{"node":{"body":%q,"author":{"login":"bot"}}}}`, body)
				default:
					t.Errorf("unexpected GitHub request %s", key)
					return nil, errors.New("unexpected GitHub request")
				}
				return recorder.Result(), nil
			})
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			code, output := runStartupFixtureWithRun(t, cfg, func() int {
				return runWithGitCheckAt(func(context.Context, string) error {
					stages = append(stages, "Git preflight")
					return errors.New("fixture Git failure")
				}, time.Now)
			}, "-mode="+mode)
			if code != 1 || !strings.Contains(output, "Git preflight: fixture Git failure") ||
				strings.Join(stages, ",") != "Git preflight,GET /user,GET /repos/o/r/issues/7/comments,POST /graphql" {
				t.Fatalf("reconciliation did not complete before preflight exit: exit=%d stages=%v log=%q", code, stages, output)
			}
			store, err = state.Open(statePath, at)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if write := store.Data.PublicationWrites["issued-job"]; write.Status != state.PublicationWriteObserved {
				t.Fatalf("issued write was not observed before preflight exit: %+v", write)
			}
		})
	}
}

func TestOrchestrateReconcilesIssuedWriteBeforeWorkerStartup(t *testing.T) {
	for _, withAccepted := range []bool{false, true} {
		t.Run(fmt.Sprintf("unassigned_accepted=%t", withAccepted), func(t *testing.T) {
			testOrchestrateReconcilesIssuedWriteBeforeWorkerStartup(t, withAccepted)
		})
	}
}

func testOrchestrateReconcilesIssuedWriteBeforeWorkerStartup(t *testing.T, withAccepted bool) {
	now := time.Now().UTC()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobID := "issued-job"
	marker := "<!-- linkdataminion:issued-job -->"
	store.Data.PublicationWrites[jobID] = state.PublicationWrite{
		AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", Repository: "o/r", Number: 7,
		Marker: marker, Since: now, StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
	}
	if withAccepted {
		store.Data.Accepted = &state.AcceptedAction{
			JobID: "other-job", AttemptID: "other-attempt", ExecutorID: "local",
			Repository: "o/r", RepositoryID: 1, OwnerID: 2, SubjectType: "Issue", SubjectNumber: 8,
			SourceKey: "comment:2", SourceNodeID: "node:2", Requester: "writer", RequesterID: 7,
			CommandName: "ask", Action: "comment", AcceptedAt: now, Deadline: now.Add(time.Hour),
		}
		if err := store.Data.Accepted.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	var lookups atomic.Int32
	body := "published\n\n" + marker
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			lookups.Add(1)
			_, _ = fmt.Fprintf(w, `[{"id":1,"node_id":"reply-node","body":%q,"user":{"login":"bot"},"updated_at":%q}]`, body, now.Format(time.RFC3339))
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = fmt.Fprintf(w, `{"data":{"node":{"body":%q,"author":{"login":"bot"}}}}`, body)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New("fixture-token")
	client.HTTPClient, client.BaseURL = server.Client(), server.URL
	watcher := &monitor.Monitor{Config: config.Config{BotLogin: "bot"}, GitHub: client}
	if code := runOrchestrateMode(t.Context(), nil, watcher, store, true, log.New(io.Discard, "", 0), "", 0); code != 1 {
		t.Fatalf("missing worker should fail after reconciliation: code=%d", code)
	}
	if lookups.Load() != 1 || store.Data.PublicationWrites[jobID].Status != state.PublicationWriteObserved {
		t.Fatalf("worker startup preceded reconciliation: lookups=%d write=%+v", lookups.Load(), store.Data.PublicationWrites[jobID])
	}
}
