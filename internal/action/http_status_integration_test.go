package action

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/linkdata/minion/internal/command"
)

func TestWorkerStatusPersistsLogBeforeResult(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	f := newHTTPServerTestWithRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, root)
	f.pull(t)
	base, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &httpExecutionClient{client: f.server.Client(), base: base}
	status := newWorkerStatus(job)
	const secret = "example-provider-secret"
	event := attemptRecord{Kind: attemptRecordDiscussion, Text: "saw " + secret,
		Call: "call-1", Agent: "helper-1", Role: "critic", EventSource: "claude"}
	if err := status.emit(t.Context(), transport, event, secret); err != nil {
		t.Fatal(err)
	}
	if observed, _, ok := f.executor.Status(); !ok || observed.LogSequence != 1 || observed.ProgressSequence != 1 {
		t.Fatalf("log delivery changed progress sequence: status=%+v present=%t", observed, ok)
	}
	before := httpTestRecords(t, root, job)
	if bytes.Contains(before, []byte(secret)) || !bytes.Contains(before, []byte("[redacted]")) {
		t.Fatalf("progressive record did not safely persist before Result: %q", before)
	}
	if err := status.drain(t.Context(), transport); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(httpTestRecords(t, root, job))), "\n")
	if len(lines) != 2 {
		t.Fatalf("record and coverage count = %d, want 2", len(lines))
	}
	var saved attemptRecord
	if err := json.Unmarshal([]byte(lines[0]), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Call != event.Call || saved.Agent != event.Agent || saved.Role != event.Role || saved.EventSource != event.EventSource {
		t.Fatalf("event metadata changed: %+v", saved)
	}
	saved = attemptRecord{}
	if err := json.Unmarshal([]byte(lines[1]), &saved); err != nil || saved.Kind != attemptRecordCoverage || saved.Text != "" {
		t.Fatalf("terminal coverage was not persisted: %+v error=%v", saved, err)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

type heldStatusACK struct {
	http.ResponseWriter
	entered chan struct{}
	release <-chan struct{}
}

func (w heldStatusACK) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w heldStatusACK) Write(data []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return w.ResponseWriter.Write(data)
}

func TestEmptyStatusACKDoesNotHoldOutcome(t *testing.T) {
	job := testJob(command.Comment)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f := newHTTPServerTestWithStopServerRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/status" {
				w = heldStatusACK{ResponseWriter: w, entered: entered, release: release}
			}
			next.ServeHTTP(w, r)
		})
	}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, 0, t.TempDir())
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, WorkerState: "busy", Phase: "prepare"}
	statusDone := make(chan httpServerTestExchange, 1)
	go func() { statusDone <- f.request("/status", httpObservation{Status: status}) }()
	<-entered
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
	once.Do(func() { close(release) })
	if got := <-statusDone; got.err != nil {
		t.Fatalf("held status ACK failed after outcome: %+v", got)
	}
}
