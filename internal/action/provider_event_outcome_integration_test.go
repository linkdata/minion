package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunHTTPQueuedProviderEventDoesNotHoldOutcomeOrClaimCoverage(t *testing.T) {
	job := testJob(command.Comment)
	root := t.TempDir()
	statusEntered := make(chan struct{}, 1)
	releaseStatus := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseStatus) })
	defer release()
	f := newHTTPServerTestWithStopServerRoot(t, t.Context(), job, runnerExecutionCheckpoints{},
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/status" {
					data, err := io.ReadAll(io.LimitReader(r.Body, httpControlBytes+1))
					if err != nil {
						t.Errorf("read Status request: %v", err)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(data))
					var observation httpObservation
					if json.Unmarshal(data, &observation) == nil && len(observation.Logs) == 1 &&
						observation.Logs[0].Kind == attemptRecordDiscussion && observation.Logs[0].Sequence == 1 {
						w = heldStatusACK{ResponseWriter: w, entered: statusEntered, release: releaseStatus}
					}
				}
				next.ServeHTTP(w, r)
			})
		}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, 0, root)
	client := *f.server.Client()
	client.Timeout = 0
	runner := &Runner{WorkRoot: t.TempDir(), runModel: func(ctx context.Context, request modelRun) (aicli.Result, error) {
		if request.onEvent == nil {
			return aicli.Result{}, errors.New("model call has no provider event sink")
		}
		request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant", Text: "first"})
		select {
		case <-statusEntered:
		case <-ctx.Done():
			return aicli.Result{}, ctx.Err()
		}
		request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant", Text: "second"})
		return aicli.Result{Answer: "Complete", Tokens: usage.Tokens{Input: 1}, UsageComplete: true}, nil
	}}
	workerDone := make(chan error, 1)
	go func() { workerDone <- RunHTTP(t.Context(), &client, f.server.URL, job.Identity.ExecutorID, runner) }()
	select {
	case result := <-f.done:
		if result.err != nil || result.result.Answer != "Complete" {
			t.Fatalf("required Result was lost: %+v", result)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("held Status ACK delayed required Result")
	}
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("worker waited for held Status ACK after Outcome ACK")
	}
	release()
	data := httpTestRecords(t, root, job)
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	if len(lines) != 1 {
		t.Fatalf("queued event or false coverage was saved after outcome: %q", data)
	}
	var record attemptRecord
	if err := json.Unmarshal(lines[0], &record); err != nil || record.Kind != attemptRecordDiscussion || record.Text != "first" {
		t.Fatalf("first event was not durably saved before held ACK: %+v error=%v", record, err)
	}
}
