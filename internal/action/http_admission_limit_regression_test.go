package action

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestSelectedWatchAdmissionControlTimeoutSurvivesLaterInactivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.RuntimeBudget = 2 * httpControlTimeout
		job.InactivityTimeout = httpControlTimeout + time.Second
		binding := httpJobBinding(job)
		var received httpOutcome
		requests := 0
		response := func(value any, limit int) (*http.Response, error) {
			data, err := httpEncode(value, limit)
			if err != nil {
				t.Fatal(err)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader(data))}, nil
		}
		client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
			requests++
			switch r.URL.Path {
			case "/next":
				return response(httpAssignment{Job: job, StopGrace: httpControlTimeout}, httpJobBytes)
			case "/cancel":
				time.Sleep(httpControlTimeout + 2*time.Second) // Virtual delay: control < idle < processing < hard.
				return nil, context.DeadlineExceeded
			case "/outcome":
				if err := httpDecode(r.Body, httpOutcomeBytes, &received); err != nil {
					t.Fatal(err)
				}
				return response(httpAcknowledgement{httpBinding: binding, Kind: "outcome", Sequence: received.Sequence}, httpControlBytes)
			default:
				t.Fatalf("worker advanced before watch admission: %s", r.URL.Path)
				return nil, errors.New("unexpected request")
			}
		})}
		err := RunHTTP(t.Context(), client, "http://worker.test", job.Identity.ExecutorID, &Runner{runModel: forbidModel(t)})
		if !errors.Is(err, monitor.ErrRequestTransport) || errors.Is(err, errExecutorInactivity) || requests != 3 ||
			received.Failure == nil || !received.Failure.RequestTransport || !received.Failure.Deadline ||
			received.Failure.Inactivity || received.Failure.RuntimeCap || len(received.Result.Calls) != 0 {
			t.Fatalf("selected watch control timeout changed after idle callback: error=%v requests=%d failure=%+v", err, requests, received.Failure)
		}
	})
}

func TestSelectedWorkerLimitDiagnosticKeepsIndependentError(t *testing.T) {
	for _, test := range []struct {
		name     string
		detail   string
		degraded bool
	}{
		{name: "model failure", detail: "independent model failure"},
		{name: "cleanup failure in degraded Outcome", detail: "independent cleanup failure", degraded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			runErr := errors.Join(monitor.ErrTerminalJob, errExecutorRuntimeCap, errors.New(test.detail))
			got, degraded, err := captureWorkerLimitOutcome(t, errExecutorRuntimeCap, runErr,
				now.Add(-time.Second), now.Add(-2*time.Second), test.degraded)
			if degraded != test.degraded || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) != test.degraded {
				t.Fatalf("degraded=%t error=%v, want degraded=%t", degraded, err, test.degraded)
			}
			if got.Failure == nil || !got.Failure.Inactivity || !got.Failure.Terminal ||
				!strings.Contains(got.Failure.Message, test.detail) ||
				strings.Contains(got.Failure.Message, "runtime cap") || len(got.Failure.Message) > httpFailureMessageLimit {
				t.Fatalf("selected limit lost independent diagnostic: %+v", got.Failure)
			}
		})
	}
}
