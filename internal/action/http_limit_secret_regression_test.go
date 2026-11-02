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
)

func TestSelectedLimitCannotReassembleRedactedSecret(t *testing.T) {
	const secret = "abcxyz"
	const independent = "independent model failure"
	for _, degraded := range []bool{false, true} {
		name := "normal"
		if degraded {
			name = "degraded"
		}
		t.Run(name, func(t *testing.T) {
			job := testJob(command.Comment)
			binding := httpJobBinding(job)
			result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
			if degraded {
				result.Answer = strings.Repeat("x", maxOutputBytes+1)
			}
			// The configured key is absent before normalization. Removing the
			// losing limit phrase would join these safe fragments into the key.
			runErr := errors.Join(monitor.ErrTerminalJob, errExecutorRuntimeCap,
				errors.New("abc"+errExecutorRuntimeCap.Error()+"xyz"), errors.New(independent))
			runErr = redactActionError(runErr, secret)
			if strings.Contains(runErr.Error(), secret) {
				t.Fatal("fixture already contains the configured key")
			}
			action, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cancel(errExecutorRuntimeCap) // Inactivity is the earlier absolute limit.
			now := time.Now()
			var received httpOutcome
			var wire []byte
			client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/outcome" {
					t.Fatalf("unexpected request: %s", r.URL.Path)
				}
				var err error
				wire, err = io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if err := httpDecode(bytes.NewReader(wire), httpOutcomeBytes, &received); err != nil {
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
				inactivity:   &inactivityTimer{last: now.Add(-3 * time.Second), timeout: time.Second},
				hardDeadline: now.Add(-time.Second), ownRuntimeCap: true, binding: binding,
			}
			err := checkpoints.outcome(result, runErr)
			if degraded != errors.Is(err, monitor.ErrExecutionOutcomeUnknown) ||
				!checkpoints.transport.outcomeAcknowledged || checkpoints.transport.outcomeDegraded != degraded {
				t.Fatalf("outcome error=%v acknowledged=%t degraded=%t", err,
					checkpoints.transport.outcomeAcknowledged, checkpoints.transport.outcomeDegraded)
			}
			failure := received.Failure
			if failure == nil || !failure.Inactivity || failure.RuntimeCap || !failure.Terminal ||
				!strings.Contains(failure.Message, errExecutorInactivity.Error()) ||
				!strings.Contains(failure.ObservedMessage, independent) ||
				!strings.Contains(failure.ObservedMessage, "abc"+errExecutorRuntimeCap.Error()+"xyz") ||
				strings.Contains(failure.Message, secret) || strings.Contains(failure.ObservedMessage, secret) ||
				bytes.Contains(wire, []byte(secret)) {
				t.Fatalf("selected limit exposed or lost diagnostic: failure=%+v wire_contains_key=%t",
					failure, bytes.Contains(wire, []byte(secret)))
			}
			reported, ok := failure.workerErr().(*workerFailureError)
			if !ok || reported.failure.ObservedMessage != failure.ObservedMessage {
				t.Fatalf("parent lost the bounded observed diagnostic: %+v", reported)
			}
		})
	}
}
