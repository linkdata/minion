package action

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func TestCallerDeadlineWinsDelayedInactivityOutcome(t *testing.T) {
	job := testJob(command.Comment)
	binding := httpJobBinding(job)
	result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
	now := time.Now()
	hard, idle := now.Add(-2*time.Second), now.Add(-time.Second)
	action, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cancel(errors.Join(monitor.ErrTerminalJob, errExecutorInactivity)) // The later callback won the context race.
	var received httpOutcome
	client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/outcome" {
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
		if err := httpDecode(r.Body, httpOutcomeBytes, &received); err != nil {
			t.Fatal(err)
		}
		data, err := httpEncode(httpAcknowledgement{httpBinding: binding, Kind: "outcome", Sequence: received.Sequence}, httpControlBytes)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	checkpoints := &httpClientCheckpoints{
		transport: &httpExecutionClient{client: client, base: &url.URL{Scheme: "http", Host: "worker.test"}},
		control:   t.Context(), action: action,
		inactivity:   &inactivityTimer{last: idle.Add(-time.Second), timeout: time.Second},
		hardDeadline: hard, ownRuntimeCap: false, binding: binding,
	}
	if err := checkpoints.outcome(result, errors.Join(monitor.ErrTerminalJob, errExecutorInactivity)); err != nil {
		t.Fatalf("caller deadline outcome: %v", err)
	}
	if received.Failure == nil || !received.Failure.Terminal || !received.Failure.Deadline ||
		received.Failure.Inactivity || received.Failure.RuntimeCap ||
		strings.Contains(received.Failure.Message, "inactivity") || !checkpoints.transport.outcomeAcknowledged {
		t.Fatalf("later inactivity callback replaced caller deadline: %+v", received.Failure)
	}
}

func TestAdmissionFailureKeepsTransportBeforeLaterInactivity(t *testing.T) {
	for _, test := range []struct {
		name           string
		observedOffset time.Duration
		wantInactivity bool
	}{
		{name: "transport observed before idle", observedOffset: -500 * time.Millisecond},
		{name: "idle overdue when transport observed", observedOffset: 500 * time.Millisecond, wantInactivity: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			idle := time.Now().Add(-time.Second)
			timer := time.NewTimer(time.Hour)
			t.Cleanup(func() { timer.Stop() })
			var laterCause error
			inactivity := &inactivityTimer{
				last: idle.Add(-time.Second), timeout: time.Second, timer: timer,
				cancel: func(err error) { laterCause = err },
			}
			transport := errors.Join(monitor.ErrRequestTransport, errors.New("watch transport failed"))
			got := admissionFailure(transport, nil, inactivity, idle.Add(test.observedOffset), idle.Add(time.Minute), true)
			if !errors.Is(laterCause, errExecutorInactivity) || !errors.Is(got, monitor.ErrRequestTransport) ||
				errors.Is(got, errExecutorInactivity) != test.wantInactivity {
				t.Fatalf("admission failure=%v later callback=%v, want inactivity=%t", got, laterCause, test.wantInactivity)
			}
		})
	}
}

func TestSelectedCallerDeadlineWithAcknowledgedCallStaysKnown(t *testing.T) {
	f, _, _ := selectedHTTPResultFixture(t, nil)
	f.ack(t, "/before", f.call(1), http.StatusOK, 1)
	paid := f.call(2)
	paid.Checkpoint.Call.Tokens = usage.Tokens{Input: 7}
	paid.Checkpoint.Call.UsageCoverage.Scope = "known_subset"
	f.ack(t, "/after", paid, http.StatusOK, 2)
	parent, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	checkpoints := f.tx.sink.(*executionCheckpoints)
	checkpoints.assignmentContext = parent
	outcome := f.outcome(3, nil)
	outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
	outcome.Failure = &httpFailure{Message: "caller deadline", Terminal: true, Deadline: true}
	f.ack(t, "/outcome", outcome, http.StatusOK, 3)
	got := f.result(t)
	final := checkpoints.finish(got.result, got.err)
	if errors.Is(final, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(final, monitor.ErrCheckpointedParentDeadline) ||
		!got.result.matchesJob(f.job) ||
		len(got.result.Calls) != 1 || got.result.Calls[0] != paid.Checkpoint.Call {
		t.Fatalf("bound caller deadline became unknown: result=%+v error=%v", got.result, final)
	}
}
