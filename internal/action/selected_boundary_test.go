package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
)

func TestExecuteRequiresSavedSelectedAttemptBeforeGitHub(t *testing.T) {
	for _, scenario := range []string{"missing selected checkpoint", "missing attempt", "invalid attempt", "missing store", "different attempt"} {
		t.Run(scenario, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				http.Error(w, "unexpected GitHub access", http.StatusInternalServerError)
			}))
			t.Cleanup(server.Close)
			e := &Orchestrator{GitHub: testGitHub(server, "token"), Store: openState(t, time.Now()), assignmentRequired: true}
			job := usageJob()
			job.Command = command.Command{Name: "comment", Action: command.Comment}
			reserveActionRequest(t, e.Store, &job, false)
			switch scenario {
			case "missing selected checkpoint":
				e.assignmentRequired = false
			case "missing attempt":
				job.AttemptID = ""
			case "invalid attempt":
				job.AttemptID = "../attempt"
			case "missing store":
				e.Store = nil
			case "different attempt":
				job.AttemptID = strings.Repeat("b", 32)
			}
			if err := e.Execute(t.Context(), job); !errors.Is(err, monitor.ErrAcceptanceState) || requests != 0 {
				t.Fatalf("unselected execution reached GitHub: error=%v requests=%d", err, requests)
			}
		})
	}
}

func TestExecuteDoesNotRunEmbeddedRunnerWithoutSelectedExecutor(t *testing.T) {
	gitFixtureRemote(t)
	modelCalls, writes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		if !serveIssueFixture(t, w, r) {
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	e := &Orchestrator{Store: openState(t, time.Now()), GitHub: testGitHub(server, "token"), WorkRoot: t.TempDir(),
		Commands:           map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
		assignmentRequired: true,
		Runner: Runner{runModel: func(context.Context, modelRun) (aicli.Result, error) {
			modelCalls++
			return aicli.Result{Answer: "should not run"}, nil
		}},
	}
	job := usageJob()
	job.Command = command.Command{Name: "comment", Action: command.Comment}
	job.Repository.Actions = []string{"comment"}
	reserveActionRequest(t, e.Store, &job, false)
	if err := e.Execute(t.Context(), job); !errors.Is(err, monitor.ErrAcceptanceState) || !strings.Contains(err.Error(), "selected executor is unavailable") || modelCalls != 0 || writes != 0 {
		t.Fatalf("missing selected executor ran work: error=%v models=%d writes=%d", err, modelCalls, writes)
	}
}

func TestIssuedWriteNeverMatchesMissingAttempt(t *testing.T) {
	job := usageJob()
	job.Command = command.Command{Name: "comment", Action: command.Comment}
	store := openState(t, time.Now())
	reserveActionRequest(t, store, &job, true)
	started := time.Now().UTC()
	write := state.PublicationWrite{AttemptID: job.AttemptID, Kind: "comment", BotLogin: "bot", BotID: 8,
		Repository: job.Repository.Name, Number: job.Number, Marker: job.Marker(), Since: job.Source.UpdatedAt,
		StartedAt: started, Deadline: started.Add(time.Minute), Status: state.PublicationWritePending}
	missing := job
	missing.AttemptID = ""
	if taskPublicationMatches(write, missing, "comment") || existingTaskPublicationMatches(write, missing) {
		t.Fatal("empty attempt matched an issued write")
	}
	blankWrite := write
	blankWrite.AttemptID = ""
	if taskPublicationMatches(blankWrite, missing, "comment") || existingTaskPublicationMatches(blankWrite, missing) {
		t.Fatal("empty issued attempt matched an empty request attempt")
	}
	store.Data.PublicationWrites[job.ID] = write
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	e := &Orchestrator{Store: store, BotLogin: "bot", BotID: 8}
	if handled, err := e.resumeTaskPublication(t.Context(), missing); !handled || !errors.Is(err, monitor.ErrAcceptanceState) {
		t.Fatalf("empty attempt resumed issued write: handled=%t error=%v", handled, err)
	}
	if err := e.beginTaskPublication(t.Context(), missing, "comment"); !errors.Is(err, monitor.ErrAcceptanceState) {
		t.Fatalf("empty attempt issued write: %v", err)
	}
	if err := e.recordPublishedOutcome(missing); !errors.Is(err, monitor.ErrAcceptanceState) {
		t.Fatalf("empty attempt recorded success: %v", err)
	}
	if store.Data.PublicationWrites[job.ID] != write || len(store.Data.TerminalDecisions) != 0 {
		t.Fatalf("empty attempt changed publication state: %+v", store.Data)
	}
}

func TestSelectedFixedReplyNeedsNoWorker(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case serveIssueFixture(t, w, r):
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			writes++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	store := openState(t, time.Now())
	e := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", BotID: 8,
		Commands:            map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{skills.BuiltinAntiPromptInjection}}},
		assignmentRequired:  true,
		ValidatePublication: func(context.Context, monitor.Job, bool) error { return nil },
		Runner:              Runner{runModel: forbidModel(t)},
	}
	job := usageJob()
	job.Command = command.Command{Name: "review", Action: command.Review}
	job.Repository.Actions = []string{"review"}
	reserveActionRequest(t, store, &job, false)
	if err := e.Execute(t.Context(), job); err != nil || writes != 1 {
		t.Fatalf("selected fixed reply required a worker: error=%v writes=%d", err, writes)
	}
	if decision := store.Data.TerminalDecisions[job.ID]; decision.AttemptID != job.AttemptID || decision.Outcome != "succeeded" {
		t.Fatalf("selected fixed reply did not record success: %+v", decision)
	}
}

func TestSelectedExecutorCannotSkipAssignmentDelivery(t *testing.T) {
	for _, tryCheckpoint := range []bool{false, true} {
		name := "complete result"
		if tryCheckpoint {
			name = "model checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			gitFixtureRemote(t)
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				if !serveIssueFixture(t, w, r) {
					t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			store := openState(t, time.Now())
			job := usageJob()
			job.Command = command.Command{Name: "comment", Action: command.Comment}
			job.Repository.Actions = []string{"comment"}
			reserveActionRequest(t, store, &job, true) // A stale assigned marker cannot prove this delivery.
			e := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), WorkRoot: t.TempDir(),
				Commands:            map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
				assignmentRequired:  true,
				ValidatePublication: func(context.Context, monitor.Job, bool) error { return nil },
				Executor: executorFunc(func(_ context.Context, prepared Job, sink Checkpoints) (Result, error) {
					result := completedRecoveryResult(prepared)
					if tryCheckpoint {
						fact := CallCheckpoint{Version: prepared.Version, Identity: prepared.Identity, SourceRef: prepared.Source.Head,
							Call: callMetadata(result.Calls[0])}
						if err := sink.BeforeCall(fact); !errors.Is(err, monitor.ErrAcceptanceState) {
							t.Fatalf("model checkpoint bypassed delivery: %v", err)
						}
					}
					return result, nil
				}),
			}
			if err := e.Execute(t.Context(), job); !errors.Is(err, monitor.ErrAcceptanceState) || writes != 0 {
				t.Fatalf("undelivered assignment reached publication: error=%v writes=%d", err, writes)
			}
		})
	}
}
