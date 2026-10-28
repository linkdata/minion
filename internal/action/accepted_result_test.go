package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestChangedFixResultRequiresOriginalDestination(t *testing.T) {
	job := testJob(command.Fix)
	result := completedRecoveryResult(job)
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("changed\n")}}
	accepted := acceptedForJob(job)
	accepted.Assigned = true
	record := AcceptedResultRecord{Accepted: *accepted, Job: job, Result: result}
	if err := record.Validate(); err == nil {
		t.Fatal("changed fix result accepted without original destination")
	}
	record.FixDestination = &FixDestination{RepositoryID: accepted.RepositoryID, DefaultBranch: "main", Visibility: "public", ForkID: 4, ForkFullName: "bot/repo"}
	if err := record.Validate(); err != nil {
		t.Fatalf("valid original destination: %v", err)
	}
	changes := record.Result.Changes
	record.Result.Changes = nil
	if err := record.Validate(); err == nil {
		t.Fatal("unchanged fix result accepted an original destination")
	}
	record.Result.Changes = changes
	record.FixDestination.RepositoryID++
	if err := record.Validate(); err == nil {
		t.Fatal("original destination accepted with wrong repository ID")
	}
}

func TestAcceptedResultMatchesEveryImmutableSelectedField(t *testing.T) {
	accepted := *acceptedForJob(testJob(command.Comment))
	record := AcceptedResultRecord{Accepted: accepted}
	if !record.MatchesAccepted(accepted) {
		t.Fatal("identical selected action did not match")
	}
	for i := range reflect.TypeOf(accepted).NumField() {
		field := reflect.TypeOf(accepted).Field(i)
		changed := accepted
		value := reflect.ValueOf(&changed).Elem().Field(i)
		switch value.Kind() {
		case reflect.String:
			value.SetString(value.String() + "x")
		case reflect.Int, reflect.Int64:
			value.SetInt(value.Int() + 1)
		case reflect.Bool:
			value.SetBool(!value.Bool())
		case reflect.Struct:
			if value.Type() != reflect.TypeOf(time.Time{}) {
				t.Fatalf("unhandled accepted-action field %s: %s", field.Name, value.Type())
			}
			value.Set(reflect.ValueOf(value.Interface().(time.Time).Add(time.Nanosecond)))
		default:
			t.Fatalf("unhandled accepted-action field %s", field.Name)
		}
		want := field.Name == "RetryAt" || field.Name == "Assigned" || field.Name == "Terminal"
		if got := record.MatchesAccepted(changed); got != want {
			t.Errorf("changed %s matched=%t, want %t", field.Name, got, want)
		}
	}
}

func selectedHTTPResultFixture(t *testing.T, wrap func(http.Handler) http.Handler, server ...func(http.Handler) *httptest.Server) (*httpServerTest, *state.Store, string) {
	return selectedHTTPResultFixtureForJob(t, testJob(command.Comment), wrap, server...)
}

func selectedHTTPResultFixtureForJob(t *testing.T, job Job, wrap func(http.Handler) http.Handler, server ...func(http.Handler) *httptest.Server) (*httpServerTest, *state.Store, string) {
	t.Helper()
	f := newExecutionAdapterFixture(t, job)
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	accepted := acceptedForJob(f.job)
	store.Data.Accepted = accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	f.owner.Store, f.owner.assignmentRequired = store, true
	f.owner.ValidateAssignment = func(context.Context, monitor.Job) error { return nil }
	f.sink.assignmentContext = t.Context()
	f.sink.assignmentRequest = monitor.Job{
		Repository:  config.Repository{Name: accepted.Repository, ID: accepted.RepositoryID, OwnerID: accepted.OwnerID},
		SubjectType: accepted.SubjectType, Number: accepted.SubjectNumber,
		Source: github.Item{Key: accepted.SourceKey, NodeID: accepted.SourceNodeID, HTMLURL: accepted.SourceURL,
			UpdatedAt: accepted.SourceUpdatedAt, Author: github.User{Login: accepted.Requester, ID: accepted.RequesterID}},
		Command: command.Command{Name: accepted.CommandName, Action: f.job.Action},
		ID:      accepted.JobID, AttemptID: accepted.AttemptID, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
	}
	if err := f.sink.BeforeAssignment(f.job); err != nil {
		t.Fatalf("save selected assignment: %v", err)
	}
	newServer := httptest.NewServer
	if len(server) != 0 {
		newServer = server[0]
	}
	httpFixture := newHTTPServerTestWithServer(t, t.Context(), f.job, f.sink, wrap, newServer)
	httpFixture.pull(t)
	if !monitor.AssignmentMatchesAccepted(f.sink.assignmentRequest, *store.Data.Accepted) {
		t.Fatalf("selected HTTP fixture request differs from accepted action: request=%+v accepted=%+v", f.sink.assignmentRequest, store.Data.Accepted)
	}
	return httpFixture, store, path
}

func TestSelectedFixOutcomePersistsOriginalDestination(t *testing.T) {
	f, store, _ := selectedHTTPResultFixtureForJob(t, testJob(command.Fix), nil)
	original := &FixDestination{RepositoryID: f.job.Identity.RepositoryID, DefaultBranch: "main", Visibility: "public", ForkID: 4, ForkFullName: "bot/repo"}
	f.tx.sink.(*executionCheckpoints).fixDestination = original
	outcome := completeHTTPResult(t, f)
	outcome.Result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("changed\n")}}
	f.ack(t, "/outcome", outcome, http.StatusOK, 3)
	record, err := readSavedResult(store)
	if err != nil || !reflect.DeepEqual(record.FixDestination, original) {
		t.Fatalf("saved fix destination differs: got=%+v want=%+v error=%v", record.FixDestination, original, err)
	}
}

func TestPublishRecoveredRejectsResultFromPriorAcceptedAttempt(t *testing.T) {
	job := testJob(command.Comment)
	store, _, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	store.Data.Accepted.AttemptID = "fedcba9876543210fedcba9876543210"
	request.AttemptID = store.Data.Accepted.AttemptID
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if !monitor.AssignmentMatchesAccepted(request, *store.Data.Accepted) {
		t.Fatal("new request does not match its accepted attempt")
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Errorf("old result reached GitHub: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token")}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrAcceptanceState) || !strings.Contains(err.Error(), "accepted result differs from current action") || requests.Load() != 0 {
		t.Fatalf("old result was accepted for new attempt: requests=%d error=%v", requests.Load(), err)
	}
}

func completeHTTPResult(t *testing.T, f *httpServerTest) httpOutcome {
	t.Helper()
	f.ack(t, "/before", f.call(1), http.StatusOK, 1)
	paid := f.call(2)
	paid.Checkpoint.Call.Tokens, paid.Checkpoint.Call.Complete = usage.Tokens{Input: 7, Output: 2}, true
	f.ack(t, "/after", paid, http.StatusOK, 2)
	outcome := f.outcome(3, nil)
	outcome.Result.Answer = "Completed answer"
	outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
	return outcome
}

func readSavedResult(store *state.Store) (AcceptedResultRecord, error) {
	var data []byte
	err := store.WithLock(func() error {
		var err error
		data, err = store.ReadAcceptedResult(AcceptedResultMaxBytes)
		return err
	})
	if err != nil {
		return AcceptedResultRecord{}, err
	}
	return DecodeAcceptedResultRecord(data)
}

type acceptedResultACKWriter struct {
	http.ResponseWriter
	store *state.Store
	check chan error
}

func (w acceptedResultACKWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w acceptedResultACKWriter) WriteHeader(status int) {
	if status == http.StatusOK {
		record, err := readSavedResult(w.store)
		if err == nil && !record.Accepted.Assigned {
			err = errors.New("saved result lacks assigned marker")
		}
		w.check <- err
	}
	w.ResponseWriter.WriteHeader(status)
}

func TestSelectedTerminalCheckpointRefusalDoesNotBecomeAcceptanceFailure(t *testing.T) {
	f, store, _ := selectedHTTPResultFixture(t, nil)
	bad := f.call(1)
	bad.Checkpoint.Call.Round = 0
	ack := f.ack(t, "/before", bad, http.StatusInternalServerError, 1)
	if ack.Failure == nil || !ack.Failure.Terminal {
		t.Fatalf("invalid checkpoint was not terminal: %+v", ack)
	}
	ack = f.ack(t, "/outcome", f.outcome(2, nil), http.StatusInternalServerError, 2)
	if ack.Failure == nil || !ack.Failure.Terminal {
		t.Fatalf("later outcome changed terminal checkpoint error: %+v", ack)
	}
	f.tx.mu.Lock()
	stopCancel := f.tx.stopCancel
	f.tx.mu.Unlock()
	if stopCancel == nil {
		t.Fatal("refused checkpoint did not start control drain")
	}
	if got := f.result(t); !errors.Is(got.err, monitor.ErrTerminalJob) || errors.Is(got.err, monitor.ErrAcceptanceState) {
		t.Fatalf("terminal checkpoint became a service acceptance failure: %v", got.err)
	}
	if saved, err := store.HasAcceptedResult(); err != nil || saved {
		t.Fatalf("invalid checkpoint saved a result: saved=%t error=%v", saved, err)
	}
}

func TestSelectedCheckpointRefusalWithoutMarkerRemainsJobFailure(t *testing.T) {
	f, store, _ := selectedHTTPResultFixture(t, nil)
	outcome := completeHTTPResult(t, f)
	checkpointErr := errors.New("checkpoint save failed")
	f.tx.mu.Lock()
	f.tx.checkpointErr = checkpointErr // Model claims success after a parent-owned checkpoint refusal.
	f.tx.mu.Unlock()
	ack := f.ack(t, "/outcome", outcome, http.StatusInternalServerError, 3)
	if ack.Failure == nil || !ack.Failure.Terminal {
		t.Fatalf("checkpoint refusal was not terminal for this job: %+v", ack)
	}
	got := f.result(t)
	if !errors.Is(got.err, checkpointErr) || !errors.Is(got.err, monitor.ErrTerminalJob) ||
		errors.Is(got.err, monitor.ErrAcceptanceState) || !reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("checkpoint refusal became service failure or lost result: result=%+v error=%v", got.result, got.err)
	}
	if saved, err := store.HasAcceptedResult(); err != nil || saved {
		t.Fatalf("refused checkpoint saved a result: saved=%t error=%v", saved, err)
	}
}

func TestSelectedCanceledOutcomeKeepsDiagnosticsWithoutSavingResult(t *testing.T) {
	f, store, _ := selectedHTTPResultFixture(t, nil)
	outcome := completeHTTPResult(t, f)
	outcome.Result.Diagnostics = []Diagnostic{{Code: "canceled", Message: "worker finished after cancellation"}}
	f.tx.stopBudget()
	ack := f.ack(t, "/outcome", outcome, http.StatusInternalServerError, 3)
	if ack.Failure == nil {
		t.Fatal("canceled result received a success ACK")
	}
	got := f.result(t)
	if !reflect.DeepEqual(got.result, outcome.Result) || !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("canceled result lost diagnostics or became publishable: result=%+v error=%v", got.result, got.err)
	}
	if saved, err := store.HasAcceptedResult(); err != nil || saved {
		t.Fatalf("canceled result was saved: saved=%t error=%v", saved, err)
	}
}

func TestSelectedHTTPOutcomePersistsCompleteRecordBeforeACK(t *testing.T) {
	var store *state.Store
	checked := make(chan error, 1)
	f, savedStore, _ := selectedHTTPResultFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" {
				w = acceptedResultACKWriter{ResponseWriter: w, store: store, check: checked}
			}
			next.ServeHTTP(w, r)
		})
	})
	store = savedStore
	outcome := completeHTTPResult(t, f)
	f.ack(t, "/outcome", outcome, http.StatusOK, 3)
	select {
	case err := <-checked:
		if err != nil {
			t.Fatalf("successful ACK preceded durable result: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("successful ACK did not check durable result")
	}
	if got := f.result(t); got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("completed outcome changed: result=%+v error=%v", got.result, got.err)
	}
	record, err := readSavedResult(store)
	if err != nil || !record.MatchesAccepted(*store.Data.Accepted) || !reflect.DeepEqual(record.Job, f.job) || !reflect.DeepEqual(record.Result, outcome.Result) {
		t.Fatalf("durable original job or complete result differs: record=%+v error=%v", record, err)
	}
}

func TestSelectedAuditUnavailableSavesResultForRecoveryWithSuccessACK(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	nodePath := os.Getenv("PATH")
	job := testJob(command.SecurityAudit)
	job.Identity.Repository = "o/r"
	f, store, _ := selectedHTTPResultFixtureForJob(t, job, nil)
	outcome := completeHTTPResult(t, f)
	audit := completeAuditResult(f.job)
	outcome.Result.Answer, outcome.Result.Artifacts = audit.Answer, audit.Artifacts
	if err := outcome.Result.Validate(f.job); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // The publishing validator disappears after model work.
	ack := f.request("/outcome", outcome)
	if ack.err != nil || ack.status != http.StatusOK || ack.ack.Failure != nil {
		t.Fatalf("saved result did not receive success ACK: %+v", ack)
	}
	record, err := readSavedResult(store)
	if err != nil || !reflect.DeepEqual(record.Result, outcome.Result) {
		t.Fatalf("unavailable validator lost the complete result: record=%+v error=%v", record, err)
	}
	got := f.result(t)
	if !reflect.DeepEqual(got.result, outcome.Result) || !errors.Is(got.err, monitor.ErrPostModelPublication) ||
		errors.Is(got.err, monitor.ErrTerminalJob) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("saved outcome was not a known publication retry: result=%+v error=%v", got.result, got.err)
	}
	f.executor.mu.Lock()
	unavailable := f.executor.unavailable
	f.executor.mu.Unlock()
	if unavailable {
		t.Fatal("parent-side audit outage retired healthy worker capacity")
	}
	if err := f.tx.sink.(*executionCheckpoints).finish(got.result, got.err); !errors.Is(err, monitor.ErrPostModelPublication) ||
		errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("saved outcome became terminal or unknown after checkpoint finish: %v", err)
	}

	t.Setenv("PATH", nodePath)
	var posts atomic.Int32
	server := auditRecoveryServer(t, &posts)
	checkpoints := f.tx.sink.(*executionCheckpoints)
	recovered := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot",
		WorkRoot: checkpoints.owner.WorkRoot, Runner: Runner{runModel: forbidModel(t)}}
	if err := recovered.PublishRecovered(t.Context(), checkpoints.assignmentRequest); err != nil || posts.Load() != 1 {
		t.Fatalf("restored validator did not publish saved audit: posts=%d error=%v", posts.Load(), err)
	}
}

func TestSelectedAuditInvalidValidatorResultDoesNotSave(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\ncase \"$2\" in -e) exit 0;; esac\nexit 9\n")
	job := testJob(command.SecurityAudit)
	f, store, _ := selectedHTTPResultFixtureForJob(t, job, nil)
	outcome := completeHTTPResult(t, f)
	audit := completeAuditResult(f.job)
	outcome.Result.Answer, outcome.Result.Artifacts = audit.Answer, audit.Artifacts
	if err := outcome.Result.Validate(f.job); err != nil {
		t.Fatal(err)
	}
	ack := f.request("/outcome", outcome)
	if ack.err != nil || ack.status != http.StatusInternalServerError || ack.ack.Failure == nil || !ack.ack.Failure.Terminal {
		t.Fatalf("invalid audit validator result was not terminal: %+v", ack)
	}
	if got := f.result(t); !errors.Is(got.err, monitor.ErrTerminalJob) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) ||
		!reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("invalid audit result did not retain its known outcome: result=%+v error=%v", got.result, got.err)
	}
	if saved, err := store.HasAcceptedResult(); err != nil || saved {
		t.Fatalf("invalid audit validator result left a publishable sidecar: saved=%t error=%v", saved, err)
	}
}

func TestSelectedInvalidPublicationReturnsKnownResultWithoutSuccessACK(t *testing.T) {
	f, store, _ := selectedHTTPResultFixture(t, nil)
	f.tx.sink.(*executionCheckpoints).owner.Runner.Codex.APIKey = "configured-secret"
	outcome := completeHTTPResult(t, f)
	outcome.Result.Answer = "configured-secret"
	outcome.Result.Diagnostics = []Diagnostic{{Code: "private", Message: "worker cleanup detail"}}
	ack := f.request("/outcome", outcome)
	if ack.err != nil || ack.status != http.StatusInternalServerError || ack.ack.Failure == nil || !ack.ack.Failure.Terminal {
		t.Fatalf("invalid publication received a success ACK: %+v", ack)
	}
	got := f.result(t)
	if !errors.Is(got.err, monitor.ErrTerminalJob) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) ||
		errors.Is(got.err, monitor.ErrAcceptanceState) || !reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("known rejected result lost its diagnostics: result=%+v error=%v", got.result, got.err)
	}
	f.executor.mu.Lock()
	unavailable := f.executor.unavailable
	f.executor.mu.Unlock()
	if !unavailable {
		t.Fatal("rejected result left worker capacity reusable")
	}
	if present, err := store.HasAcceptedResult(); err != nil || present {
		t.Fatalf("invalid result left a publishable receipt: present=%t error=%v", present, err)
	}
}

func TestSelectedHTTPOutcomeRejectsChangedAttemptAndSaveFailure(t *testing.T) {
	for _, change := range []string{"foreign attempt", "changed deadline", "save failure"} {
		t.Run(change, func(t *testing.T) {
			f, store, path := selectedHTTPResultFixture(t, nil)
			outcome := completeHTTPResult(t, f)
			switch change {
			case "foreign attempt":
				outcome.Result.Diagnostics = []Diagnostic{{Code: "worker", Message: "complete but refused"}}
				store.Data.Accepted.AttemptID = "other-attempt"
			case "changed deadline":
				store.Data.Accepted.Deadline = store.Data.Accepted.Deadline.Add(time.Minute)
			case "save failure":
				if err := os.Mkdir(path+".result", 0700); err != nil {
					t.Fatal(err)
				}
			}
			ack := f.request("/outcome", outcome)
			if ack.err != nil || ack.status != http.StatusInternalServerError || ack.ack.Failure == nil {
				t.Fatalf("failed selected result received success ACK: %+v", ack)
			}
			if change == "save failure" && !errors.Is(f.tx.sink.(*executionCheckpoints).failure, monitor.ErrAcceptanceState) {
				t.Fatalf("failed result save had wrong parent error class: %v", f.tx.sink.(*executionCheckpoints).failure)
			}
			f.tx.mu.Lock()
			completed := f.tx.outcome
			f.tx.mu.Unlock()
			if !completed {
				t.Fatal("refused complete result did not seal its known outcome")
			}
			if got := f.result(t); !reflect.DeepEqual(got.result, outcome.Result) ||
				!errors.Is(got.err, monitor.ErrAcceptanceState) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
				t.Fatalf("refused complete result lost its known outcome: result=%+v error=%v", got.result, got.err)
			}
			if change != "save failure" {
				if _, err := store.ReadAcceptedResult(AcceptedResultMaxBytes); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rejected attempt saved a result: %v", err)
				}
			}
		})
	}
}

func TestSelectedHTTPOutcomeLostACKKeepsDurableRecord(t *testing.T) {
	var failFirst atomic.Bool
	failFirst.Store(true)
	f, store, _ := selectedHTTPResultFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" && failFirst.Swap(false) {
				w = httpServerFailWrite{w}
			}
			next.ServeHTTP(w, r)
		})
	})
	outcome := completeHTTPResult(t, f)
	if reply := f.request("/outcome", outcome); reply.err == nil {
		t.Fatalf("outcome ACK was not lost: %+v", reply)
	}
	f.tx.mu.Lock()
	completed, checkpointErr := f.tx.outcome, f.tx.checkpointErr
	f.tx.mu.Unlock()
	if !completed {
		t.Fatalf("lost ACK did not complete transaction: %v", checkpointErr)
	}
	if got := f.result(t); got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("lost ACK erased accepted result: result=%+v error=%v", got.result, got.err)
	}
	f.executor.mu.Lock()
	unavailable := f.executor.unavailable
	f.executor.mu.Unlock()
	if !unavailable {
		t.Fatal("lost ACK released worker capacity")
	}
	if record, err := readSavedResult(store); err != nil || !reflect.DeepEqual(record.Result, outcome.Result) {
		t.Fatalf("lost ACK erased durable result: record=%+v error=%v", record, err)
	}
}

type selectedWithoutResultReceiver struct{ Checkpoints }

func (selectedWithoutResultReceiver) AssignmentRequired() bool      { return true }
func (selectedWithoutResultReceiver) BeforeAssignment(Job) error    { return nil }
func (selectedWithoutResultReceiver) AssignmentNotDelivered() error { return nil }

func TestSelectedHTTPOutcomeRequiresCompletionReceiver(t *testing.T) {
	job := testJob(command.Comment)
	f := newHTTPServerTest(t, t.Context(), job, selectedWithoutResultReceiver{runnerExecutionCheckpoints{}}, nil)
	f.pull(t)
	outcome := f.outcome(1, nil)
	if ack := f.request("/outcome", outcome); ack.err != nil || ack.status != http.StatusInternalServerError || ack.ack.Failure == nil {
		t.Fatalf("selected outcome without durable receiver received success ACK: %+v", ack)
	}
	f.tx.mu.Lock()
	completed := f.tx.outcome
	f.tx.mu.Unlock()
	if !completed {
		t.Fatal("selected outcome without durable receiver was not sealed")
	}
	if got := f.result(t); !reflect.DeepEqual(got.result, outcome.Result) || !errors.Is(got.err, monitor.ErrAcceptanceState) || !strings.Contains(got.err.Error(), "selected HTTP outcome receiver is missing") {
		t.Fatalf("selected outcome without durable receiver lost its refusal: result=%+v error=%v", got.result, got.err)
	}
}
