package action

import (
	"errors"
	"net/http"
	"testing"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestSelectedFailedOutcomeChecksAcknowledgedCallBeforeACK(t *testing.T) {
	for _, test := range []struct {
		name          string
		omitCall      bool
		wantACKStatus int
		wantRefused   bool
	}{
		{name: "omitted acknowledged call", omitCall: true, wantACKStatus: http.StatusInternalServerError, wantRefused: true},
		{name: "matching incomplete usage", wantACKStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, _, _ := selectedHTTPResultFixture(t, nil)
			f.ack(t, "/before", f.call(1), http.StatusOK, 1)
			paid := f.call(2)
			paid.Checkpoint.Call.Tokens = usage.Tokens{Input: 7}
			f.ack(t, "/after", paid, http.StatusOK, 2)

			outcome := f.outcome(3, nil)
			outcome.Failure = newHTTPFailure(errors.Join(monitor.ErrTerminalJob, errExecutorInactivity))
			if !test.omitCall {
				outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
			}
			ack := f.ack(t, "/outcome", outcome, test.wantACKStatus, 3)
			if (ack.Failure != nil) != test.wantRefused {
				t.Fatalf("failed Outcome ACK refusal=%t, want %t", ack.Failure != nil, test.wantRefused)
			}
			got := f.result(t)
			if !got.result.matchesJob(f.job) || len(got.result.Calls) != len(outcome.Result.Calls) {
				t.Fatalf("failed Outcome changed diagnostic facts: result=%+v error=%v", got.result, got.err)
			}
			f.executor.mu.Lock()
			unavailable := f.executor.unavailable
			f.executor.mu.Unlock()
			if unavailable != test.wantRefused {
				t.Fatalf("worker unavailable=%t after failed Outcome, want %t", unavailable, test.wantRefused)
			}
			if test.wantRefused {
				if _, err := f.executor.Execute(t.Context(), f.job, f.tx.sink); err == nil {
					t.Fatal("protocol-invalid failed Outcome left the worker reusable")
				}
				if err := f.tx.sink.(*executionCheckpoints).finish(got.result, got.err); !errors.Is(err, monitor.ErrTerminalJob) {
					t.Fatalf("invalid failed Outcome escaped parent validation: %v", err)
				}
			} else {
				err := f.tx.sink.(*executionCheckpoints).finish(got.result, got.err)
				var inactivity interface{ ExecutorInactivity() bool }
				if !errors.As(err, &inactivity) || !inactivity.ExecutorInactivity() || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
					t.Fatalf("matching incomplete usage lost known inactivity: %v", err)
				}
			}
		})
	}
}

func TestSelectedFailedOutcomeRejectsConflictingWorkerLimitsBeforeACK(t *testing.T) {
	f, _, _ := selectedHTTPResultFixture(t, nil)
	outcome := f.outcome(1, nil)
	outcome.Failure = newHTTPFailure(errors.Join(monitor.ErrTerminalJob, errExecutorInactivity))
	outcome.Failure.RuntimeCap = true
	outcome.Failure.Deadline = true
	ack := f.ack(t, "/outcome", outcome, http.StatusBadRequest, 1)
	if ack.Failure == nil {
		t.Fatal("conflicting worker limit claims received a success ACK")
	}

	outcome.Failure.RuntimeCap = false
	outcome.Failure.Deadline = false
	f.ack(t, "/outcome", outcome, http.StatusOK, 1)
	got := f.result(t)
	final := f.tx.sink.(*executionCheckpoints).finish(got.result, got.err)
	var inactivity interface{ ExecutorInactivity() bool }
	if !errors.As(final, &inactivity) || !inactivity.ExecutorInactivity() ||
		errors.Is(final, monitor.ErrCheckpointedWorkerRuntimeCap) {
		t.Fatalf("valid inactivity retry lost its limit label: %v", final)
	}
}

func TestSelectedFailedOutcomeAfterRejectedBeforeQuarantinesWorker(t *testing.T) {
	f, _, _ := selectedHTTPResultFixture(t, nil)
	bad := f.call(1)
	bad.Checkpoint.Ordinal = 2 // Shaped, but inconsistent with the parent's first-call slot.
	ack := f.ack(t, "/before", bad, http.StatusInternalServerError, 1)
	if ack.Failure == nil {
		t.Fatal("invalid BeforeCall received a success ACK")
	}
	outcome := f.outcome(2, errors.New("worker stopped after rejected BeforeCall"))
	reply := f.request("/outcome", outcome)
	if reply.err != nil || reply.ack.Kind != "outcome" || reply.ack.Sequence != 2 ||
		(reply.status != http.StatusOK && reply.status != http.StatusInternalServerError) ||
		(reply.status == http.StatusInternalServerError && reply.ack.Failure == nil) {
		t.Fatalf("failed Outcome was not received: %+v", reply)
	}
	got := f.result(t)
	if !errors.Is(got.err, monitor.ErrTerminalJob) || !got.result.matchesJob(f.job) || len(got.result.Calls) != 0 {
		t.Fatalf("rejected BeforeCall changed parent failure or raw facts: result=%+v error=%v", got.result, got.err)
	}
	f.executor.mu.Lock()
	unavailable := f.executor.unavailable
	f.executor.mu.Unlock()
	if !unavailable {
		t.Fatal("rejected BeforeCall left the worker endpoint reusable")
	}
	if _, err := f.executor.Execute(t.Context(), f.job, f.tx.sink); err == nil {
		t.Fatal("rejected BeforeCall allowed another assignment")
	}
}

func TestSelectedFailedOutcomeRejectsJobInvalidResultBeforeACK(t *testing.T) {
	var path string
	checked := make(chan error, 1)
	f, _, _ := selectedHTTPResultFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" {
				w = failedResultACKWriter{ResponseWriter: w, path: &path, wantStatus: http.StatusInternalServerError, checked: checked}
			}
			next.ServeHTTP(w, r)
		})
	})
	path = failedResultDiagnosticPath(f)
	outcome := f.outcome(1, errors.New("worker failed"))
	outcome.Result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("changed\n")}}
	outcome.Result.Diagnostics = []Diagnostic{{Code: "result_detail", Message: "private result detail"}}
	if err := httpResultShape(outcome.Result); err != nil {
		t.Fatalf("test Result is not wire-shaped: %v", err)
	}
	if err := outcome.Result.validate(f.job, false); err == nil {
		t.Fatal("read-only Comment Result unexpectedly permits source changes")
	}
	ack := f.ack(t, "/outcome", outcome, http.StatusInternalServerError, 1)
	if ack.Failure == nil {
		t.Fatal("job-invalid failed Outcome received success ACK")
	}
	if err := <-checked; err != nil {
		t.Fatal(err)
	}
	got := f.result(t)
	if got.err == nil {
		t.Fatal("job-invalid failed Outcome was not rejected by the parent")
	}
	f.executor.mu.Lock()
	unavailable := f.executor.unavailable
	f.executor.mu.Unlock()
	if !unavailable {
		t.Fatal("job-invalid failed Outcome left the worker available")
	}
	if _, err := f.executor.Execute(t.Context(), f.job, f.tx.sink); err == nil {
		t.Fatal("job-invalid failed Outcome left the worker reusable")
	}
}
