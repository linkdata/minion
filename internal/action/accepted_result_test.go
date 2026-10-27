package action

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/usage"
)

func selectedHTTPResultFixture(t *testing.T, wrap func(http.Handler) http.Handler) (*httpServerTest, *state.Store, string) {
	t.Helper()
	f := newExecutionAdapterFixture(t, testJob(command.Comment))
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
	httpFixture := newHTTPServerTest(t, t.Context(), f.job, f.sink, wrap)
	httpFixture.pull(t)
	if !requestMatchesAccepted(f.sink.assignmentRequest, *store.Data.Accepted) {
		t.Fatalf("selected HTTP fixture request differs from accepted action: request=%+v accepted=%+v", f.sink.assignmentRequest, store.Data.Accepted)
	}
	return httpFixture, store, path
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
	if err := <-checked; err != nil {
		t.Fatalf("successful ACK preceded durable result: %v", err)
	}
	if got := f.result(t); got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("completed outcome changed: result=%+v error=%v", got.result, got.err)
	}
	record, err := readSavedResult(store)
	if err != nil || !record.MatchesAccepted(*store.Data.Accepted) || !reflect.DeepEqual(record.Job, f.job) || !reflect.DeepEqual(record.Result, outcome.Result) {
		t.Fatalf("durable original job or complete result differs: record=%+v error=%v", record, err)
	}
}

func TestSelectedHTTPOutcomeRejectsChangedAttemptAndSaveFailure(t *testing.T) {
	for _, change := range []string{"foreign attempt", "changed deadline", "expired deadline", "save failure"} {
		t.Run(change, func(t *testing.T) {
			f, store, path := selectedHTTPResultFixture(t, nil)
			outcome := completeHTTPResult(t, f)
			switch change {
			case "foreign attempt":
				store.Data.Accepted.AttemptID = "other-attempt"
			case "changed deadline":
				store.Data.Accepted.Deadline = store.Data.Accepted.Deadline.Add(time.Minute)
			case "expired deadline":
				store.Data.Accepted.Deadline = time.Now().Add(-time.Second)
			case "save failure":
				if err := os.Mkdir(path+".result", 0700); err != nil {
					t.Fatal(err)
				}
			}
			ack := f.request("/outcome", outcome)
			if ack.err != nil || ack.status != http.StatusInternalServerError || ack.ack.Failure == nil {
				t.Fatalf("failed selected result received success ACK: %+v", ack)
			}
			f.tx.mu.Lock()
			completed := f.tx.outcome
			f.tx.mu.Unlock()
			if completed {
				t.Fatal("failed selected result marked transaction complete")
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
	if record, err := readSavedResult(store); err != nil || !reflect.DeepEqual(record.Result, outcome.Result) {
		t.Fatalf("lost ACK erased durable result: record=%+v error=%v", record, err)
	}
	if duplicate := f.request("/outcome", outcome); duplicate.err != nil || duplicate.status != http.StatusConflict {
		t.Fatalf("duplicate outcome changed completed attempt: %+v", duplicate)
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
	if completed {
		t.Fatal("selected outcome without durable receiver marked transaction complete")
	}
}
