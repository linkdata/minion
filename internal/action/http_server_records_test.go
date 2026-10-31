package action

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linkdata/minion/internal/command"
)

func httpTestRecord(job Job, sequence uint64, text string) attemptRecord {
	return attemptRecord{Identity: job.Identity, SourceRef: job.Source.Head, Sequence: sequence, Kind: attemptRecordDiscussion, Text: text}
}

func httpTestRecords(t *testing.T, root string, job Job) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "attempt-records", job.Identity.AttemptID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type statusSyncReceiver struct {
	runnerExecutionCheckpoints
	synced <-chan struct{}
}

func (r statusSyncReceiver) ReceiveCompletedResult(Result) error {
	select {
	case <-r.synced:
		return nil
	default:
		return errors.New("outcome receiver ran before Status sync")
	}
}

func TestHTTPStatusRecordsPersistBeforeOutcomeAndReplay(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	job.Main.Model = "pinned-main"
	f := newHTTPServerTestWithRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, root)
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	first := httpTestRecord(job, 1, "first")
	first.Call = "call-1"
	first.Role, first.Provider, first.Model, first.ObservedAt = roleMain, job.Main.Provider, job.Main.Model, "2026-10-31T12:34:56Z"
	f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{first}}, http.StatusOK, 1)
	status.Sequence, status.LogSequence = 2, 2
	logs := []attemptRecord{httpTestRecord(job, 2, "second")}
	if ack := f.ack(t, "/status", httpObservation{Status: status, Logs: logs}, http.StatusOK, 2); ack.LogSequence != 2 {
		t.Fatalf("durable highwater = %d, want 2", ack.LogSequence)
	}
	before := httpTestRecords(t, root, job)
	if bytes.Count(before, []byte{'\n'}) != 2 || !bytes.Contains(before, []byte(`"text":"first"`)) || !bytes.Contains(before, []byte(`"text":"second"`)) ||
		!bytes.Contains(before, []byte(`"provider":"codex","model":"pinned-main","observed_at":"2026-10-31T12:34:56Z"`)) {
		t.Fatalf("records were not durable before outcome: %q", before)
	}
	status.Sequence = 3
	if ack := f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 3); ack.LogSequence != 2 {
		t.Fatalf("empty heartbeat highwater = %d, want 2", ack.LogSequence)
	}
	status.Sequence = 4
	if ack := f.ack(t, "/status", httpObservation{Status: status, Logs: logs}, http.StatusOK, 4); ack.LogSequence != 2 {
		t.Fatalf("replay highwater = %d, want 2", ack.LogSequence)
	}
	if after := httpTestRecords(t, root, job); !bytes.Equal(after, before) {
		t.Fatalf("replay rewrote saved records: before=%q after=%q", before, after)
	}
	status.Sequence, status.LogSequence = 5, 3
	coverage := httpTestRecord(job, 3, "")
	coverage.Kind = attemptRecordCoverage
	if ack := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{coverage}}, http.StatusOK, 5); ack.LogSequence != 3 {
		t.Fatalf("coverage highwater = %d, want 3", ack.LogSequence)
	}
	if saved := httpTestRecords(t, root, job); bytes.Count(saved, []byte{'\n'}) != 3 || !bytes.Contains(saved, []byte(`"kind":"coverage"`)) {
		t.Fatalf("coverage was not durable before outcome: %q", saved)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPStatusRejectsMismatchedCallMetadata(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	job.Main.Model = "pinned-main"
	f := newHTTPServerTestWithRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, root)
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	first := httpTestRecord(job, 1, "first")
	first.Call = "call-1"
	first.Role, first.Provider, first.Model, first.ObservedAt = roleMain, job.Main.Provider, job.Main.Model, "2026-10-31T12:34:56Z"
	f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{first}}, http.StatusOK, 1)
	before := httpTestRecords(t, root, job)
	status.Sequence, status.LogSequence = 2, 2
	second := httpTestRecord(job, 2, "second")
	second.Call = "call-2"
	second.Role, second.Provider, second.Model, second.ObservedAt = first.Role, first.Provider, first.Model, first.ObservedAt
	for _, test := range []struct {
		name   string
		change func(*attemptRecord)
	}{
		{"wrong provider", func(r *attemptRecord) { r.Provider = "claude" }},
		{"wrong model", func(r *attemptRecord) { r.Model = "other-model" }},
		{"missing provider", func(r *attemptRecord) { r.Provider = "" }},
		{"missing model", func(r *attemptRecord) { r.Model = "" }},
		{"missing observed time", func(r *attemptRecord) { r.ObservedAt = "" }},
		{"time without provider or model", func(r *attemptRecord) { r.Provider, r.Model = "", "" }},
		{"call without provenance", func(r *attemptRecord) { r.Provider, r.Model, r.ObservedAt = "", "", "" }},
		{"unbound role", func(r *attemptRecord) { r.Role = "" }},
		{"unselected critic", func(r *attemptRecord) { r.Role = roleCritic }},
		{"malformed observed time", func(r *attemptRecord) { r.ObservedAt = "2026-10-31T13:34:56+01:00" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := second
			test.change(&changed)
			ack := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{changed}}, http.StatusConflict, 2)
			if ack.Failure == nil {
				t.Fatal("mismatched call metadata received success ACK")
			}
			if saved := httpTestRecords(t, root, job); !bytes.Equal(saved, before) {
				t.Fatalf("mismatched call metadata changed records: %q", saved)
			}
		})
	}
	if ack := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{second}}, http.StatusOK, 2); ack.LogSequence != 2 {
		t.Fatalf("valid call metadata highwater = %d", ack.LogSequence)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPStatusRecordsDistinguishFixtureSameProviderRoles(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	job.Main.Model = "main-model"
	job.Critic = &RoleSettings{Provider: job.Main.Provider, Model: "critic-model"}
	// Job.Validate intentionally rejects same-provider roles until #145; this isolates record matching.
	tx := &httpTransaction{job: job, recordRoot: root}
	t.Cleanup(func() {
		if tx.records != nil {
			_ = tx.records.Close()
		}
	})
	main := httpTestRecord(job, 1, "main call")
	main.Call = "call-1"
	main.Role, main.Provider, main.Model, main.ObservedAt = roleMain, job.Main.Provider, job.Main.Model, "2026-10-31T12:34:56Z"
	if ack, err := tx.appendStatusLogs(Status{LogSequence: 1}, []attemptRecord{main}, ""); err != nil || ack != 1 {
		t.Fatalf("main record: ack=%d error=%v", ack, err)
	}
	critic := httpTestRecord(job, 2, "critic call")
	critic.Call = "call-2"
	critic.Role, critic.Provider, critic.Model, critic.ObservedAt = roleCritic, job.Critic.Provider, job.Main.Model, "2026-10-31T12:34:57Z"
	if ack, err := tx.appendStatusLogs(Status{LogSequence: 2}, []attemptRecord{critic}, ""); ack != 1 || !errors.Is(err, errAttemptRecordConflict) {
		t.Fatalf("main model accepted for critic role: ack=%d error=%v", ack, err)
	}
	critic.Model = job.Critic.Model
	if ack, err := tx.appendStatusLogs(Status{LogSequence: 2}, []attemptRecord{critic}, ""); err != nil || ack != 2 {
		t.Fatalf("critic record: ack=%d error=%v", ack, err)
	}
	saved := httpTestRecords(t, root, job)
	if bytes.Count(saved, []byte{'\n'}) != 2 || !bytes.Contains(saved, []byte(`"role":"main","provider":"codex","model":"main-model"`)) ||
		!bytes.Contains(saved, []byte(`"role":"critic","provider":"codex","model":"critic-model"`)) {
		t.Fatalf("fixture roles were not retained distinctly: %q", saved)
	}
}

func TestHTTPStatusAcceptsConfiguredEmptyModelProvenance(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	f := newHTTPServerTestWithRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, root)
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	record := httpTestRecord(job, 1, "available call")
	record.Call = "call-1"
	record.Role, record.Provider, record.ObservedAt = roleMain, job.Main.Provider, "2026-10-31T12:34:56Z"
	if ack := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{record}}, http.StatusOK, 1); ack.LogSequence != 1 {
		t.Fatalf("empty configured model highwater = %d", ack.LogSequence)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPStatusLostACKReplaysPersistedRecord(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	var failed atomic.Bool
	f := newHTTPServerTestWithStopServerRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/status" && failed.CompareAndSwap(false, true) {
				w = httpServerFailWrite{w}
			}
			next.ServeHTTP(w, r)
		})
	}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, 0, root)
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	logs := []attemptRecord{httpTestRecord(job, 1, "once")}
	if got := f.request("/status", httpObservation{Status: status, Logs: logs}); got.err == nil {
		t.Fatalf("injected ACK failure was not observed: %+v", got)
	}
	before := httpTestRecords(t, root, job)
	if bytes.Count(before, []byte{'\n'}) != 1 {
		t.Fatalf("lost ACK also lost the sync: %q", before)
	}
	status.Sequence = 2
	if ack := f.ack(t, "/status", httpObservation{Status: status, Logs: logs}, http.StatusOK, 2); ack.LogSequence != 1 {
		t.Fatalf("replayed ACK highwater = %d, want 1", ack.LogSequence)
	}
	if after := httpTestRecords(t, root, job); !bytes.Equal(after, before) {
		t.Fatalf("lost ACK replay duplicated the record: before=%q after=%q", before, after)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPStatusRejectsInvalidLogChunks(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	f := newHTTPServerTestWithRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, root)
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	first := httpTestRecord(job, 1, "saved")
	f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{first}}, http.StatusOK, 1)
	before := httpTestRecords(t, root, job)
	status.Sequence = 2
	second := httpTestRecord(job, 2, "second")
	wrongSource := second
	wrongSource.SourceRef = strings.Repeat("b", 40)
	badKind := second
	badKind.Kind = "unknown"
	changedReplay := first
	changedReplay.Text = "changed"
	tests := []struct {
		name string
		seq  uint64
		logs []attemptRecord
	}{
		{"gap", 3, []attemptRecord{httpTestRecord(job, 3, "third")}},
		{"duplicate in chunk", 1, []attemptRecord{first, first}},
		{"changed replay", 1, []attemptRecord{changedReplay}},
		{"reported sequence mismatch", 3, []attemptRecord{second}},
		{"wrong source", 2, []attemptRecord{wrongSource}},
		{"invalid kind", 2, []attemptRecord{badKind}},
		{"empty status without highwater", 0, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := status
			candidate.LogSequence = test.seq
			ack := f.ack(t, "/status", httpObservation{Status: candidate, Logs: test.logs}, http.StatusConflict, candidate.Sequence)
			if ack.Failure == nil {
				t.Fatal("invalid record chunk received success ACK")
			}
			if saved := httpTestRecords(t, root, job); !bytes.Equal(saved, before) {
				t.Fatalf("invalid chunk changed the store: before=%q after=%q", before, saved)
			}
		})
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPStatusLogsRequireWorkRoot(t *testing.T) {
	job := testJob(command.Comment)
	f := newHTTPServerTestInMemory(t, t.Context(), job, runnerExecutionCheckpoints{})
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	for _, logs := range [][]attemptRecord{nil, {httpTestRecord(job, 1, "unrooted")}} {
		ack := f.ack(t, "/status", httpObservation{Status: status, Logs: logs}, http.StatusConflict, 1)
		if ack.Failure == nil {
			t.Fatal("unrooted server acknowledged logs or log sequence")
		}
	}
	status.LogSequence = 0
	status.Sequence = 2
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 2)
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPStatusEmptyHeartbeatDoesNotCreateRecordStore(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	f := newHTTPServerTestWithRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, root)
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "prepare"}
	if ack := f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 1); ack.LogSequence != 0 {
		t.Fatalf("empty heartbeat highwater = %d", ack.LogSequence)
	}
	if _, err := os.Stat(filepath.Join(root, "attempt-records")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty heartbeat opened record store: %v", err)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPStatusRedactsAuthenticatedBearerToken(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	f := newHTTPServerTestWithStopServerRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			next.ServeHTTP(w, r)
		})
	}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, 0, root)
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	log := httpTestRecord(job, 1, "token="+token)
	if ack := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{log}}, http.StatusOK, 1); ack.LogSequence != 1 {
		t.Fatalf("bearer record highwater = %d", ack.LogSequence)
	}
	if saved := httpTestRecords(t, root, job); bytes.Contains(saved, []byte(token)) || !bytes.Contains(saved, []byte("[redacted]")) {
		t.Fatalf("bearer token was not redacted: %q", saved)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPStatusRejectsCredentialJoinedAcrossFragments(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	const secret = "fixture-provider-secret"
	f := newHTTPServerTestWithStopServerRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, nil,
		func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, 0, root,
		WorkerCredentials{CodexAPIKey: secret})
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	first := httpTestRecord(job, 1, "fixture-provider-")
	first.More = true
	if ack := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{first}}, http.StatusOK, 1); ack.LogSequence != 1 {
		t.Fatalf("first fragment highwater = %d, want 1", ack.LogSequence)
	}
	before := httpTestRecords(t, root, job)
	if bytes.Count(before, []byte{'\n'}) != 1 || !bytes.Contains(before, []byte(`"more":true`)) {
		t.Fatalf("first fragment was not durable: %q", before)
	}
	status.Sequence, status.LogSequence = 2, 2
	second := httpTestRecord(job, 2, "secret")
	ack := f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{second}}, http.StatusConflict, 2)
	if ack.Failure == nil {
		t.Fatal("joined credential received success acknowledgement")
	}
	if saved := httpTestRecords(t, root, job); !bytes.Equal(saved, before) || len(f.tx.records.lines) != 1 {
		t.Fatalf("rejected continuation advanced highwater or added coverage: %q", saved)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPStatusReconcilesUncertainSyncOnRetry(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	f := newHTTPServerTestWithRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, root)
	f.pull(t)
	store, err := openAttemptRecordStore(root, job.Identity, job.Source.Head)
	if err != nil {
		t.Fatal(err)
	}
	f.tx.records = store
	var once sync.Once
	syncFile := f.tx.records.syncFile
	f.tx.records.syncFile = func() error {
		var err error
		once.Do(func() { err = errors.New("injected sync failure") })
		if err != nil {
			return err
		}
		return syncFile()
	}
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	logs := []attemptRecord{httpTestRecord(job, 1, "persisted despite failed sync")}
	if ack := f.ack(t, "/status", httpObservation{Status: status, Logs: logs}, http.StatusInternalServerError, 1); ack.Failure == nil {
		t.Fatal("uncertain sync received success ACK")
	}
	status.Sequence = 2
	if ack := f.ack(t, "/status", httpObservation{Status: status, Logs: logs}, http.StatusOK, 2); ack.LogSequence != 1 {
		t.Fatalf("reconciled highwater = %d, want 1", ack.LogSequence)
	}
	if saved := httpTestRecords(t, root, job); bytes.Count(saved, []byte{'\n'}) != 1 {
		t.Fatalf("retry duplicated uncertain write: %q", saved)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPStatusSyncDoesNotHoldPaidCallbackAndDelaysOutcome(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	statusEntered, release, synced, outcomeEntered := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f := newHTTPServerTestWithStopServerRoot(t, t.Context(), job, statusSyncReceiver{synced: synced}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" {
				close(outcomeEntered)
			}
			next.ServeHTTP(w, r)
		})
	}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, 0, root)
	f.pull(t)
	store, err := openAttemptRecordStore(root, job.Identity, job.Source.Head)
	if err != nil {
		t.Fatal(err)
	}
	f.tx.records = store
	syncFile := f.tx.records.syncFile
	f.tx.records.syncFile = func() error {
		close(statusEntered)
		<-release
		err := syncFile()
		close(synced)
		return err
	}
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	statusDone := make(chan httpServerTestExchange, 1)
	go func() {
		statusDone <- f.request("/status", httpObservation{Status: status, Logs: []attemptRecord{httpTestRecord(job, 1, "synced")}})
	}()
	<-statusEntered
	f.ack(t, "/before", f.call(1), http.StatusOK, 1)
	outcomeDone := make(chan httpServerTestExchange, 1)
	go func() { outcomeDone <- f.request("/outcome", f.outcome(2, nil)) }()
	<-outcomeEntered
	releaseOnce.Do(func() { close(release) })
	if got := <-statusDone; got.err != nil || got.status != http.StatusOK || got.ack.LogSequence != 1 {
		t.Fatalf("Status sync acknowledgement: %+v", got)
	}
	if got := <-outcomeDone; got.err != nil || got.status != http.StatusOK {
		t.Fatalf("outcome acknowledgement: %+v", got)
	}
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}
