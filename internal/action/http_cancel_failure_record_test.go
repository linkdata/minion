package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
)

func TestRunHTTPCanceledNativeCallSendsFailureRecordDuringStopGrace(t *testing.T) {
	job := testJob(command.Comment)
	root := t.TempDir()
	markerSaved := make(chan struct{})
	var markerOnce sync.Once
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/after" {
				select {
				case <-markerSaved:
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Second):
					t.Error("canceled native call did not deliver its failure record during stop grace")
				}
			}
			if r.URL.Path != "/status" {
				next.ServeHTTP(w, r)
				return
			}
			data, err := io.ReadAll(io.LimitReader(r.Body, httpControlBytes+1))
			if err != nil {
				t.Errorf("read Status: %v", err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			var observation httpObservation
			if err := json.Unmarshal(data, &observation); err != nil {
				t.Errorf("decode Status: %v", err)
				return
			}
			next.ServeHTTP(w, r)
			if len(observation.Logs) == 1 && observation.Logs[0].EventSource == job.Main.Provider+".call.failed" {
				markerOnce.Do(func() { close(markerSaved) })
			}
		})
	}
	f := newHTTPServerTestWithStopServerRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, wrap,
		func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, 0, root)
	client := *f.server.Client()
	client.Timeout = 0
	workerCtx, cancelWorker := context.WithCancel(t.Context())
	defer cancelWorker()
	runner := &Runner{WorkRoot: t.TempDir(), runModel: func(ctx context.Context, _ modelRun) (aicli.Result, error) {
		cancelWorker()
		<-ctx.Done()
		return aicli.Result{}, ctx.Err()
	}}
	if err := RunHTTP(workerCtx, &client, f.server.URL, job.Identity.ExecutorID, runner); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled worker error = %v", err)
	}
	select {
	case <-markerSaved:
	default:
		t.Fatal("failed call record did not reach the parent before Outcome")
	}
	saved := httpTestRecords(t, root, job)
	if !bytes.Contains(saved, []byte(`"event_source":"`+job.Main.Provider+`.call.failed"`)) ||
		!bytes.Contains(saved, []byte(`context canceled`)) ||
		bytes.Contains(saved, []byte(`"kind":"coverage"`)) {
		t.Fatalf("canceled call record or incomplete coverage changed: %s", strings.TrimSpace(string(saved)))
	}
	select {
	case outcome := <-f.done:
		var failure *workerFailureError
		if !errors.As(outcome.err, &failure) || !failure.failure.Canceled || len(outcome.result.Calls) != 1 {
			t.Fatalf("required failed Outcome lost: %+v", outcome)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("required failed Outcome did not reach the parent")
	}
}
