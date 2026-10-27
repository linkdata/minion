package action

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/monitor"
)

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
