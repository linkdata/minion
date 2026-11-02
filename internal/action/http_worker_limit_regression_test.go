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
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestWorkerLimitKeepsEarlierCallerDeadlineWhenCallbacksLag(t *testing.T) {
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		cause error
		hard  time.Duration
		idle  time.Duration
		want  workerLimit
	}{
		{name: "caller before delayed inactivity callback", cause: errExecutorInactivity, hard: 9 * time.Second, idle: 10 * time.Second, want: workerLimitCallerDeadline},
		{name: "caller and inactivity tie", cause: errExecutorInactivity, hard: 10 * time.Second, idle: 10 * time.Second, want: workerLimitCallerDeadline},
		{name: "inactivity before delayed caller callback", cause: context.DeadlineExceeded, hard: 10 * time.Second, idle: 9 * time.Second, want: workerLimitInactivity},
		{name: "inactivity before caller with no callback", hard: 10 * time.Second, idle: 9 * time.Second, want: workerLimitInactivity},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := selectWorkerLimit(test.cause, start.Add(12*time.Second),
				start.Add(test.hard), start.Add(test.idle), false)
			if got != test.want {
				t.Fatalf("worker limit = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWorkerLimitOutcomeKeepsConsistentDiagnosticAndFallback(t *testing.T) {
	for _, test := range []struct {
		name         string
		cause        error
		runErr       error
		hardFirst    bool
		oversized    bool
		wantCap      bool
		wantDetail   string
		wantObserved string
	}{
		{name: "inactivity beats hard callback", cause: errExecutorRuntimeCap,
			runErr: errors.Join(monitor.ErrTerminalJob, errExecutorRuntimeCap), wantDetail: "inactivity"},
		{name: "inactivity outranks model cancellation", cause: errExecutorInactivity,
			runErr: errors.Join(monitor.ErrTerminalJob, context.Canceled), wantDetail: "inactivity", wantObserved: context.Canceled.Error()},
		{name: "hard beats inactivity callback", cause: errExecutorInactivity,
			runErr: errors.Join(monitor.ErrTerminalJob, errExecutorInactivity), hardFirst: true, wantCap: true, wantDetail: "runtime cap"},
		{name: "runtime cap outranks model cancellation", cause: errExecutorRuntimeCap,
			runErr: errors.Join(monitor.ErrTerminalJob, context.Canceled), hardFirst: true, wantCap: true, wantDetail: "runtime cap", wantObserved: context.Canceled.Error()},
		{name: "selected hard limit is terminal", cause: errExecutorInactivity,
			runErr: errors.New("ordinary model failure"), hardFirst: true, wantCap: true, wantDetail: "runtime cap"},
		{name: "degraded outcome keeps corrected inactivity", cause: errExecutorRuntimeCap,
			runErr: errors.Join(monitor.ErrTerminalJob, errExecutorRuntimeCap), oversized: true, wantDetail: "inactivity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			hard, idle := now.Add(-time.Second), now.Add(-2*time.Second)
			if test.hardFirst {
				hard, idle = idle, hard
			}
			got, degraded, err := captureWorkerLimitOutcome(t, test.cause, test.runErr, hard, idle, test.oversized)
			if degraded != test.oversized || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) != test.oversized {
				t.Fatalf("degraded=%t error=%v, want degraded=%t", degraded, err, test.oversized)
			}
			if got.Failure == nil || !got.Failure.Terminal || got.Failure.Canceled || got.Failure.Inactivity == test.wantCap ||
				got.Failure.RuntimeCap != test.wantCap || got.Failure.Deadline != test.wantCap ||
				!strings.Contains(got.Failure.Message, test.wantDetail) {
				t.Fatalf("inconsistent selected limit: %+v", got.Failure)
			}
			if test.wantObserved != "" && !strings.Contains(got.Failure.ObservedMessage, test.wantObserved) {
				t.Fatalf("model cancellation missing from observed diagnostic: %+v", got.Failure)
			}
			losing := "runtime cap"
			if test.wantCap {
				losing = "inactivity"
			}
			if strings.Contains(got.Failure.Message, losing) {
				t.Fatalf("losing limit remained in diagnostic: %+v", got.Failure)
			}
			if test.oversized && got.Result.Answer != "" {
				t.Fatalf("degraded outcome kept oversized answer: %d bytes", len(got.Result.Answer))
			}
		})
	}
}

func captureWorkerLimitOutcome(t *testing.T, cause, runErr error, hard, idle time.Time, oversized bool) (httpOutcome, bool, error) {
	t.Helper()
	job := testJob(command.Comment)
	binding := httpJobBinding(job)
	result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
	if oversized {
		result.Answer = strings.Repeat("x", maxOutputBytes+1)
	}
	actionCtx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cancel(cause)
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
	inactivity := &inactivityTimer{last: idle.Add(-time.Second), timeout: time.Second}
	checkpoints := &httpClientCheckpoints{
		transport: &httpExecutionClient{client: client, base: &url.URL{Scheme: "http", Host: "worker.test"}},
		control:   t.Context(), action: actionCtx, inactivity: inactivity, hardDeadline: hard, ownRuntimeCap: true,
		binding: binding,
	}
	err := checkpoints.outcome(result, runErr)
	if !checkpoints.transport.outcomeAcknowledged {
		t.Fatal("Outcome acknowledgement was not accepted")
	}
	return received, checkpoints.transport.outcomeDegraded, err
}

func TestBeforeCallChecksAbsoluteLimitsAroundACK(t *testing.T) {
	for _, test := range []struct {
		name        string
		hard, idle  time.Duration
		ackDelay    time.Duration
		cancelAt    time.Duration
		callerLimit bool
		cause, want error
		requests    int
	}{
		{name: "runtime cap before ACK", hard: -time.Second, idle: time.Minute, want: errExecutorRuntimeCap},
		{name: "inactivity before ACK", hard: time.Minute, idle: -time.Second, want: errExecutorInactivity},
		{name: "caller deadline before ACK", hard: -time.Second, idle: time.Minute, callerLimit: true, want: context.DeadlineExceeded},
		{name: "earlier cancellation before ACK", hard: -time.Second, idle: time.Minute, cause: context.Canceled, want: context.Canceled},
		{name: "runtime cap during ACK", hard: 5 * time.Second, idle: time.Minute, ackDelay: 6 * time.Second, want: errExecutorRuntimeCap, requests: 1},
		{name: "inactivity during ACK", hard: time.Minute, idle: 5 * time.Second, ackDelay: 6 * time.Second, want: errExecutorInactivity, requests: 1},
		{name: "earlier cancellation during ACK", hard: 5 * time.Second, idle: time.Minute, ackDelay: 6 * time.Second, cancelAt: 4 * time.Second, want: context.Canceled, requests: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				job := testJob(command.Comment)
				binding := httpJobBinding(job)
				started := time.Now()
				action, cancelAction := context.WithCancelCause(t.Context())
				defer cancelAction(nil)
				if test.cause != nil {
					cancelAction(test.cause)
				}
				requests := 0
				client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
					requests++
					if r.URL.Path != "/before" {
						t.Fatalf("unexpected request: %s", r.URL.Path)
					}
					if test.cancelAt > 0 {
						time.Sleep(test.cancelAt)
						cancelAction(context.Canceled)
						time.Sleep(test.ackDelay - test.cancelAt)
					} else {
						time.Sleep(test.ackDelay)
					}
					data, err := httpEncode(httpAcknowledgement{httpBinding: binding, Kind: "before", Sequence: 1}, httpControlBytes)
					if err != nil {
						t.Fatal(err)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(bytes.NewReader(data))}, nil
				})}
				checkpoints := &httpClientCheckpoints{
					transport: &httpExecutionClient{client: client, base: &url.URL{Scheme: "http", Host: "worker.test"}},
					control:   t.Context(), action: action, inactivity: &inactivityTimer{last: started, timeout: test.idle},
					hardDeadline: started.Add(test.hard), ownRuntimeCap: !test.callerLimit, binding: binding,
				}
				call := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head,
					Ordinal: 1, Call: CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1}}
				got := checkpoints.BeforeCall(call)
				if !errors.Is(got, test.want) || requests != test.requests ||
					errors.Is(got, errExecutorInactivity) != (test.want == errExecutorInactivity) ||
					errors.Is(got, errExecutorRuntimeCap) != (test.want == errExecutorRuntimeCap) {
					t.Fatalf("before call error=%v requests=%d, want %v and %d requests", got, requests, test.want, test.requests)
				}
				if test.cause == nil && test.cancelAt == 0 && action.Err() != nil {
					t.Fatalf("test context canceled instead of exercising absolute deadline: %v", action.Err())
				}
				if test.requests == 1 {
					if retry := checkpoints.BeforeCall(call); retry != got || requests != 1 {
						t.Fatalf("expired acknowledged call was retried: first=%v retry=%v requests=%d", got, retry, requests)
					}
				}
			})
		})
	}
}
