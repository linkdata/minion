package action

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

type resultContextAfterCheck struct {
	context.Context
	checks   int
	cause    error
	onSecond func()
}

func (c *resultContextAfterCheck) Err() error {
	c.checks++
	if c.checks > 1 {
		if c.checks == 2 && c.onSecond != nil {
			c.onSecond()
		}
		return c.cause
	}
	return nil
}

func TestElapsedSelectedDeadlineIsTerminalRatherThanAcceptanceUncertain(t *testing.T) {
	f, store, _ := selectedHTTPResultFixture(t, nil)
	outcome := completeHTTPResult(t, f)
	accepted := store.Data.Accepted
	accepted.AcceptedAt = time.Now().Add(-2 * time.Minute)
	accepted.Deadline = accepted.AcceptedAt.Add(time.Minute)
	sink := f.tx.sink.(*executionCheckpoints)
	sink.assignmentRequest.AcceptedAt = accepted.AcceptedAt
	sink.assignmentRequest.Deadline = accepted.Deadline
	err := sink.ReceiveCompletedResult(outcome.Result)
	if !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, monitor.ErrAcceptanceState) {
		t.Fatalf("elapsed accepted deadline got wrong failure classification: %v", err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || present {
		t.Fatalf("elapsed accepted deadline saved a result: present=%t err=%v", present, err)
	}
}

func TestSelectedResultContextEndsBetweenAcceptanceChecks(t *testing.T) {
	for _, test := range []struct {
		name     string
		cause    error
		terminal bool
	}{
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, store, _ := selectedHTTPResultFixture(t, nil)
			outcome := completeHTTPResult(t, f)
			sink := f.tx.sink.(*executionCheckpoints)
			// The context changes only at the final Store-lock check.
			sink.assignmentContext = &resultContextAfterCheck{Context: t.Context(), cause: test.cause}
			err := sink.ReceiveCompletedResult(outcome.Result)
			if !errors.Is(err, test.cause) || errors.Is(err, monitor.ErrTerminalJob) != test.terminal ||
				errors.Is(err, monitor.ErrAcceptanceState) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
				t.Fatalf("second acceptance check misclassified %s: %v", test.name, err)
			}
			if test.terminal && errors.Is(sink.finish(outcome.Result, err), monitor.ErrExecutionOutcomeUnknown) {
				t.Fatal("known expired outcome became unknown")
			}
			if present, err := store.HasAcceptedResult(); err != nil || present {
				t.Fatalf("ended result was saved: present=%t error=%v", present, err)
			}
		})
	}
}

func TestSelectedResultRechecksIdentityAtFinalStoreLock(t *testing.T) {
	f, store, _ := selectedHTTPResultFixture(t, nil)
	outcome := completeHTTPResult(t, f)
	sink := f.tx.sink.(*executionCheckpoints)
	sink.assignmentContext = &resultContextAfterCheck{Context: t.Context(), onSecond: func() {
		store.Data.Accepted.AttemptID = strings.Repeat("b", 32)
	}}
	err := sink.ReceiveCompletedResult(outcome.Result)
	if !errors.Is(err, monitor.ErrAcceptanceState) || errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("changed acceptance identity passed final check: %v", err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || present {
		t.Fatalf("changed identity saved result: present=%t error=%v", present, err)
	}
}

func TestSelectedResultSaveCrossingDeadlineCannotPublishAfterRestart(t *testing.T) {
	f, store, path := selectedHTTPResultFixture(t, nil)
	result := completeHTTPResult(t, f).Result
	sink := f.tx.sink.(*executionCheckpoints)
	synctest.Test(t, func(t *testing.T) {
		accepted := store.Data.Accepted
		accepted.AcceptedAt = time.Now()
		accepted.Deadline = accepted.AcceptedAt.Add(time.Second)
		sink.assignmentRequest.AcceptedAt = accepted.AcceptedAt
		sink.assignmentRequest.Deadline = accepted.Deadline
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		sink.writeAcceptedResult = func(data []byte) error {
			if err := store.WriteAcceptedResult(data); err != nil {
				return err
			}
			time.Sleep(2 * time.Second)
			return nil
		}
		err := sink.ReceiveCompletedResult(result)
		if !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("late save was acknowledged: %v", err)
		}
	})
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("late result was not retained as evidence: present=%t error=%v", present, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	accepted := reopened.Data.Accepted
	if accepted == nil || !accepted.Terminal || reopened.Data.TerminalDecisions[accepted.JobID].Outcome != "expired" {
		t.Fatalf("expired decision was not durable: accepted=%+v decisions=%+v", accepted, reopened.Data.TerminalDecisions)
	}
}

func TestSelectedResultCommitCrossingDeadlineRefusesOutcome(t *testing.T) {
	f, store, path := selectedHTTPResultFixture(t, nil)
	result := completeHTTPResult(t, f).Result
	sink := f.tx.sink.(*executionCheckpoints)
	synctest.Test(t, func(t *testing.T) {
		accepted := store.Data.Accepted
		accepted.AcceptedAt = time.Now().UTC()
		accepted.Deadline = accepted.AcceptedAt.Add(time.Second)
		sink.assignmentRequest.AcceptedAt = accepted.AcceptedAt
		sink.assignmentRequest.Deadline = accepted.Deadline
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		sink.saveAcceptedResult = func() error {
			time.Sleep(2 * time.Second)
			return store.Save()
		}
		err := sink.ReceiveCompletedResult(result)
		if !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("result committed after deadline was acknowledged: %v", err)
		}
		if !accepted.ResultReady || !accepted.Terminal ||
			store.Data.TerminalDecisions[accepted.JobID].Outcome != "expired" {
			t.Fatalf("late result was not terminal: accepted=%+v decisions=%+v", accepted, store.Data.TerminalDecisions)
		}
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	accepted := reopened.Data.Accepted
	if accepted == nil || !accepted.Terminal || reopened.Data.TerminalDecisions[accepted.JobID].Outcome != "expired" {
		t.Fatalf("late result lost terminal decision on restart: accepted=%+v decisions=%+v", accepted, reopened.Data.TerminalDecisions)
	}
}

func TestSelectedResultCancellationDuringCommitRefusesOutcome(t *testing.T) {
	f, store, _ := selectedHTTPResultFixture(t, nil)
	result := completeHTTPResult(t, f).Result
	sink := f.tx.sink.(*executionCheckpoints)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sink.assignmentContext = ctx
	sink.saveAcceptedResult = func() error {
		cancel()
		return store.Save()
	}
	err := sink.ReceiveCompletedResult(result)
	accepted := store.Data.Accepted
	if !errors.Is(err, context.Canceled) || accepted == nil || !accepted.Terminal ||
		store.Data.TerminalDecisions[accepted.JobID].Outcome != "canceled" {
		t.Fatalf("cancellation during result commit was acknowledged: err=%v accepted=%+v decisions=%+v", err, accepted, store.Data.TerminalDecisions)
	}
}

func TestSelectedResultCancellationBeforeDeadlineWinsDuringLateSave(t *testing.T) {
	f, store, _ := selectedHTTPResultFixture(t, nil)
	result := completeHTTPResult(t, f).Result
	sink := f.tx.sink.(*executionCheckpoints)
	synctest.Test(t, func(t *testing.T) {
		accepted := store.Data.Accepted
		accepted.AcceptedAt = time.Now().UTC()
		accepted.Deadline = accepted.AcceptedAt.Add(time.Second)
		sink.assignmentRequest.AcceptedAt = accepted.AcceptedAt
		sink.assignmentRequest.Deadline = accepted.Deadline
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithDeadline(t.Context(), accepted.Deadline)
		defer cancel()
		sink.assignmentContext = ctx
		sink.writeAcceptedResult = func(data []byte) error {
			if err := store.WriteAcceptedResult(data); err != nil {
				return err
			}
			cancel()
			time.Sleep(2 * time.Second)
			return nil
		}
		err := sink.ReceiveCompletedResult(result)
		decision := store.Data.TerminalDecisions[accepted.JobID]
		if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			decision.Outcome != "canceled" || !accepted.Terminal {
			t.Fatalf("cancellation before deadline became expiry after save: err=%v decision=%+v accepted=%+v", err, decision, accepted)
		}
	})
}

func TestSelectedResultInnerDeadlineAfterSaveDoesNotExpireAction(t *testing.T) {
	f, store, _ := selectedHTTPResultFixture(t, nil)
	result := completeHTTPResult(t, f).Result
	sink := f.tx.sink.(*executionCheckpoints)
	sink.writeAcceptedResult = func(data []byte) error {
		if err := store.WriteAcceptedResult(data); err != nil {
			return err
		}
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		t.Cleanup(cancel)
		sink.assignmentContext = ctx
		return nil
	}
	err := sink.ReceiveCompletedResult(result)
	decision := store.Data.TerminalDecisions[store.Data.Accepted.JobID]
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) ||
		!time.Now().Before(store.Data.Accepted.Deadline) || decision.Outcome != "failed" {
		t.Fatalf("inner deadline was treated as original expiry: err=%v decision=%+v accepted=%+v", err, decision, store.Data.Accepted)
	}
}
