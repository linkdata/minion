package action

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/monitor"
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
