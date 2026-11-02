package action

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

type capResultReceiver struct {
	runnerExecutionCheckpoints
	received bool
}

func (r *capResultReceiver) ReceiveCompletedResult(Result) error {
	r.received = true
	return nil
}

func TestHTTPStatusTotalCapSavesMarkerAndRejectsSuccess(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	receiver := &capResultReceiver{}
	f := newHTTPServerTestWithRoot(t, t.Context(), job, receiver, root)
	f.pull(t)
	store, err := openAttemptRecordStoreWithLimit(root, job.Identity, job.Source.Head, 8192)
	if err != nil {
		t.Fatal(err)
	}
	f.tx.records = store
	first := httpTestRecord(job, 1, "saved")
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	store.limit = store.stopSpace + store.coverSpace + int64(len(encoded)+1)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	if ack := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{first}}, http.StatusOK, 1); ack.LogSequence != 1 {
		t.Fatalf("first record highwater = %d", ack.LogSequence)
	}
	const rejectedText = "private-rejected-discussion-text"
	second := httpTestRecord(job, 2, rejectedText)
	status.Sequence, status.LogSequence = 2, 2
	capAck := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{second}}, http.StatusInternalServerError, 2)
	if capAck.Failure == nil || !capAck.Failure.CaptureLimit ||
		!errors.Is(capAck.Failure.err(), errRemoteCaptureLimit) || strings.Contains(capAck.Failure.Message, rejectedText) {
		t.Fatalf("total cap was not a fixed typed rejection: %+v", capAck.Failure)
	}
	saved := httpTestRecords(t, root, job)
	lines := bytes.Split(bytes.TrimSpace(saved), []byte{'\n'})
	if len(lines) != 2 || bytes.Contains(saved, []byte(rejectedText)) || !bytes.Contains(lines[0], []byte(`"text":"saved"`)) {
		t.Fatalf("cap changed acknowledged records or retained rejected text: %q", saved)
	}
	var marker attemptRecord
	if err := json.Unmarshal(lines[1], &marker); err != nil || marker.Kind != attemptRecordStop ||
		marker.Sequence != 2 || marker.Text != "capture_limit" {
		t.Fatalf("cap marker = %+v, error=%v", marker, err)
	}
	status.Sequence, status.LogSequence, status.WorkerState = 3, 2, "stopping"
	if ack := f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 3); ack.LogSequence != 2 {
		t.Fatalf("stopping Status lost cap marker highwater: %+v", ack)
	}
	if observed, _, ok := f.executor.Status(); !ok || observed.WorkerState != "stopping" {
		t.Fatalf("bounded Status stopped after cap: %+v, present=%t", observed, ok)
	}
	status.Sequence = 4
	retry := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{second}}, http.StatusInternalServerError, 4)
	if retry.Failure == nil || !retry.Failure.CaptureLimit || !bytes.Equal(httpTestRecords(t, root, job), saved) {
		t.Fatalf("lost cap reply retry changed marker or lost typed rejection: %+v", retry.Failure)
	}
	outcome := f.outcome(1, nil)
	outcome.Result.Answer = "would publish"
	f.ack(t, "/outcome", outcome, http.StatusInternalServerError, 1)
	got := f.result(t)
	var journal bytes.Buffer
	log.New(&journal, "", 0).Printf("attempt failed: %v", got.err)
	if receiver.received || !errors.Is(got.err, monitor.ErrTerminalJob) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) ||
		len(got.result.Diagnostics) != 1 || got.result.Diagnostics[0].Code != "capture_limit" ||
		got.result.Diagnostics[0].Message != captureLimitMessage || got.result.Answer != "" ||
		strings.Contains(journal.String(), rejectedText) {
		t.Fatalf("cap race published or lost fixed diagnostic: received=%t result=%+v err=%v", receiver.received, got.result, got.err)
	}
}

func TestHTTPStatusCapWorkerLossKeepsDiagnosticAndUnknown(t *testing.T) {
	const privateText = "private-rejected-discussion-text"
	root := t.TempDir()
	job := testJob(command.Comment)
	f := newHTTPServerTestWithRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, root)
	f.pull(t)
	store, err := openAttemptRecordStoreWithLimit(root, job.Identity, job.Source.Head, 8192)
	if err != nil {
		t.Fatal(err)
	}
	store.limit = store.stopSpace + store.coverSpace + 1
	f.tx.records = store
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	ack := f.ack(t, "/status", httpObservation{Status: status,
		Logs: []attemptRecord{httpTestRecord(job, 1, privateText)}}, http.StatusInternalServerError, 1)
	if ack.Failure == nil || !ack.Failure.CaptureLimit {
		t.Fatalf("worker loss fixture did not confirm the store cap: %+v", ack.Failure)
	}
	// The worker disconnects without an Outcome. Expire the existing stop grace directly.
	f.tx.stopExecution(io.ErrClosedPipe, true)
	f.tx.mu.Lock()
	stopCancel := f.tx.stopCancel
	f.tx.mu.Unlock()
	stopCancel()
	got := f.result(t)
	records := httpTestRecords(t, root, job)
	if !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || errors.Is(got.err, monitor.ErrTerminalJob) ||
		!got.result.matchesJob(job) || got.result.Answer != "" || len(got.result.Diagnostics) != 1 ||
		got.result.Diagnostics[0] != (Diagnostic{Code: "capture_limit", Message: captureLimitMessage}) ||
		bytes.Contains(records, []byte(privateText)) || !bytes.Contains(records, []byte(`"kind":"stop"`)) ||
		!bytes.Contains(records, []byte(`"text":"capture_limit"`)) || bytes.Contains(records, []byte(`"kind":"coverage"`)) ||
		strings.Contains(got.err.Error(), privateText) {
		t.Fatalf("worker loss erased the bounded cap evidence or changed unknown classification: result=%+v err=%v records=%q", got.result, got.err, records)
	}
}

func TestHTTPStatusCapMarkerWriteFailureRemainsAmbiguous(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	f := newHTTPServerTestWithRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, root)
	f.pull(t)
	store, err := openAttemptRecordStoreWithLimit(root, job.Identity, job.Source.Head, 8192)
	if err != nil {
		t.Fatal(err)
	}
	f.tx.records = store
	first := httpTestRecord(job, 1, "saved")
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	store.limit = store.stopSpace + store.coverSpace + int64(len(encoded)+1)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{first}}, http.StatusOK, 1)
	store.syncFile = func() error { return syscall.EIO }
	status.Sequence, status.LogSequence = 2, 2
	ack := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{httpTestRecord(job, 2, "refused")}}, http.StatusInternalServerError, 2)
	f.tx.recordSync.Lock()
	confirmed, open := f.tx.captureLimit, f.tx.records != nil
	f.tx.recordSync.Unlock()
	if ack.Failure == nil || ack.Failure.CaptureLimit || errors.Is(ack.Failure.err(), errRemoteCaptureLimit) || confirmed || open {
		t.Fatalf("uncertain marker write became confirmed cap: failure=%+v confirmed=%t open=%t", ack.Failure, confirmed, open)
	}
	f.ack(t, "/outcome", f.outcome(1, errors.New("record append uncertain")), http.StatusOK, 1)
	_ = f.result(t)
}

func TestHTTPStatusFragmentLimitDoesNotClaimTotalCap(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	f := newHTTPServerTestWithRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, root)
	f.pull(t)
	store, err := openAttemptRecordStoreWithLimit(root, job.Identity, job.Source.Head, 8192)
	if err != nil {
		t.Fatal(err)
	}
	store.eventLimit = 5
	f.tx.records = store
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	first := httpTestRecord(job, 1, "abc")
	first.More = true
	f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{first}}, http.StatusOK, 1)
	status.Sequence, status.LogSequence = 2, 2
	second := httpTestRecord(job, 2, "def")
	ack := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{second}}, http.StatusInternalServerError, 2)
	f.tx.recordSync.Lock()
	capSaved, stopSaved := f.tx.captureLimit, f.tx.records.stop
	f.tx.recordSync.Unlock()
	if ack.Failure == nil || ack.Failure.CaptureLimit || errors.Is(ack.Failure.err(), errRemoteCaptureLimit) ||
		capSaved || stopSaved {
		t.Fatalf("oversized logical event invented total cap: %+v", ack.Failure)
	}
	f.ack(t, "/outcome", f.outcome(1, errors.New("incomplete event")), http.StatusOK, 1)
	_ = f.result(t)
}
