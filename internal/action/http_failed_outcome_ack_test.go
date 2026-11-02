package action

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/linkdata/minion/internal/monitor"
)

type failedResultACKWriter struct {
	http.ResponseWriter
	path       *string
	wantStatus int
	checked    chan error
}

func (w failedResultACKWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w failedResultACKWriter) WriteHeader(status int) {
	if status == w.wantStatus {
		data, err := os.ReadFile(*w.path)
		if err == nil {
			var saved retainedWorkerDiagnostics
			err = json.Unmarshal(data, &saved)
			if err == nil && (saved.WorkerReportedFailure == nil || len(saved.WorkerReportedDiagnostics) != 1 ||
				saved.WorkerReportedDiagnostics[0].Message != "private result detail") {
				err = fmt.Errorf("failed Result diagnostics absent at ACK: %+v", saved)
			}
		}
		w.checked <- err
	}
	w.ResponseWriter.WriteHeader(status)
}

func failedResultDiagnosticPath(f *httpServerTest) string {
	root := f.tx.sink.(*executionCheckpoints).owner.WorkRoot
	return filepath.Join(root, "worker-diagnostics", f.job.Identity.AttemptID+".json")
}

func failedResultWithDiagnostic(f *httpServerTest) httpOutcome {
	outcome := f.outcome(1, errors.Join(monitor.ErrTerminalJob, errors.New("worker failed")))
	outcome.Result.Diagnostics = []Diagnostic{{Code: "result_detail", Message: "private result detail"}}
	return outcome
}

func TestSelectedFailedResultDiagnosticsAreDurableBeforeACK(t *testing.T) {
	var path string
	checked := make(chan error, 1)
	f, store, _ := selectedHTTPResultFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" {
				w = failedResultACKWriter{ResponseWriter: w, path: &path, wantStatus: http.StatusOK, checked: checked}
			}
			next.ServeHTTP(w, r)
		})
	})
	path = failedResultDiagnosticPath(f)
	status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head,
		Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{httpTestRecord(f.job, 1, "earlier discussion")}}, http.StatusOK, 1)
	if saved := httpTestRecords(t, f.tx.recordRoot, f.job); !bytes.Contains(saved, []byte("earlier discussion")) {
		t.Fatalf("earlier Status was not saved: %q", saved)
	}
	f.ack(t, "/outcome", failedResultWithDiagnostic(f), http.StatusOK, 1)
	if err := <-checked; err != nil {
		t.Fatal(err)
	}
	if got := f.result(t); got.err == nil {
		t.Fatalf("failed Result became successful: %v", got.err)
	}
	if saved, err := store.HasAcceptedResult(); err != nil || saved {
		t.Fatalf("failed Result became publishable: saved=%t error=%v", saved, err)
	}
}

func TestSelectedFailedResultLostACKDoesNotDuplicateDiagnostics(t *testing.T) {
	var failFirst atomic.Bool
	failFirst.Store(true)
	f, _, _ := selectedHTTPResultFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" && failFirst.Swap(false) {
				w = httpServerFailWrite{w}
			}
			next.ServeHTTP(w, r)
		})
	})
	outcome := failedResultWithDiagnostic(f)
	if reply := f.request("/outcome", outcome); reply.err == nil {
		t.Fatalf("outcome ACK was not lost: %+v", reply)
	}
	path := failedResultDiagnosticPath(f)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("lost ACK also lost diagnostics: %v", err)
	}
	if reply := f.request("/outcome", outcome); reply.err != nil || reply.status != http.StatusConflict {
		t.Fatalf("sealed outcome accepted a retry: %+v", reply)
	}
	got := f.result(t)
	if got.err == nil {
		t.Fatalf("failed Result became successful after lost ACK: %v", got.err)
	}
	if err := f.tx.sink.(*executionCheckpoints).owner.retainWorkerDiagnostics(f.job, got.result, got.err); err != nil {
		t.Fatalf("later diagnostic retention conflicted: %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("retry duplicated or changed diagnostics: error=%v before=%q after=%q", err, before, after)
	}
}

func TestSelectedFailedResultDiagnosticSaveFailureRefusesACK(t *testing.T) {
	f, store, _ := selectedHTTPResultFixture(t, nil)
	blocked := filepath.Dir(failedResultDiagnosticPath(f))
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	ack := f.ack(t, "/outcome", failedResultWithDiagnostic(f), http.StatusInternalServerError, 1)
	if ack.Failure == nil || !ack.Failure.Terminal {
		t.Fatalf("unsaved diagnostics received a success ACK: %+v", ack)
	}
	if got := f.result(t); !errors.Is(got.err, monitor.ErrTerminalJob) {
		t.Fatalf("unsaved diagnostics escaped terminal failure: %v", got.err)
	}
	if saved, err := store.HasAcceptedResult(); err != nil || saved {
		t.Fatalf("unsaved failed Result became publishable: saved=%t error=%v", saved, err)
	}
	if data, err := os.ReadFile(blocked); err != nil || string(data) != "not a directory" {
		t.Fatalf("diagnostic save changed blocking path: %q, %v", data, err)
	}
}
