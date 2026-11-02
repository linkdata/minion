package action

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestRunHTTPCaptureLimitStopsModelAndFailsWithoutCoverage(t *testing.T) {
	const privateText = "private-provider-discussion-must-not-escape"
	job := testJob(command.Comment)
	root := t.TempDir()
	receiver := &capResultReceiver{}
	f := newHTTPServerTestWithRoot(t, t.Context(), job, receiver, root)
	store, err := openAttemptRecordStoreWithLimit(root, job.Identity, job.Source.Head, 8192)
	if err != nil {
		t.Fatal(err)
	}
	// Leave space only for the reserved stop and coverage markers.
	store.limit = store.stopSpace + store.coverSpace + 1
	f.tx.records = store

	// Hold the watch body so the typed Status refusal, rather than the watch,
	// must cancel the model. The underlying watch still closes on worker exit.
	client := *f.server.Client()
	client.Timeout = 0
	underlying := client.Transport
	client.Transport = httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		response, err := underlying.RoundTrip(r)
		if err == nil && r.URL.Path == "/cancel" {
			response.Body = httpClientGatedWatchBody{ReadCloser: response.Body,
				reader: iotest.ErrReader(context.Canceled), trigger: r.Context().Done(), reads: &atomic.Int32{}}
		}
		return response, err
	})
	var logs bytes.Buffer
	modelCause := make(chan error, 1)
	runner := &Runner{WorkRoot: t.TempDir(), Logger: log.New(&logs, "", 0),
		runModel: func(ctx context.Context, request modelRun) (aicli.Result, error) {
			request.onProgress()
			request.onEvent(aicli.StreamEvent{NativeID: "session", Kind: "assistant", Text: privateText})
			// This timer is only a failure bound. A working cap cancels promptly.
			timer := time.NewTimer(10 * time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				modelCause <- context.Cause(ctx)
				return aicli.Result{}, ctx.Err()
			case <-timer.C:
				err := errors.New("capture cap did not stop the model")
				modelCause <- err
				return aicli.Result{}, err
			}
		}}
	err = RunHTTP(t.Context(), &client, f.server.URL, job.Identity.ExecutorID, runner)
	if !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, errRemoteCaptureLimit) {
		t.Fatalf("capture cap did not fail the worker terminally: %v", err)
	}
	select {
	case cause := <-modelCause:
		if !errors.Is(cause, errRemoteCaptureLimit) || !errors.Is(cause, monitor.ErrTerminalJob) {
			t.Fatalf("model was stopped by another cause: %v", cause)
		}
	default:
		t.Fatal("model did not return after the capture cap")
	}
	got := f.result(t)
	if receiver.received || !errors.Is(got.err, monitor.ErrTerminalJob) ||
		len(got.result.Diagnostics) == 0 || got.result.Diagnostics[len(got.result.Diagnostics)-1] !=
		(Diagnostic{Code: "capture_limit", Message: captureLimitMessage}) || got.result.Answer != "" {
		t.Fatalf("parent published or lost fixed cap diagnosis: received=%t result=%+v err=%v", receiver.received, got.result, got.err)
	}
	records := httpTestRecords(t, root, job)
	if bytes.Contains(records, []byte(privateText)) || !bytes.Contains(records, []byte(`"kind":"stop"`)) ||
		!bytes.Contains(records, []byte(`"text":"capture_limit"`)) ||
		bytes.Contains(records, []byte(`"kind":"coverage"`)) || strings.Contains(logs.String(), privateText) ||
		strings.Contains(err.Error(), privateText) {
		t.Fatalf("capture cap leaked private text or claimed coverage: records=%q logs=%q err=%v", records, logs.String(), err)
	}
}

func TestWorkerStatusConclusiveCaptureLimitKeepsStatusWithoutCoverage(t *testing.T) {
	job := testJob(command.Comment)
	var received []httpObservation
	respond := func(code int, ack httpAcknowledgement) (*http.Response, error) {
		data, err := httpEncode(ack, httpControlBytes)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader(data))}, nil
	}
	client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		var observation httpObservation
		if err := httpDecode(r.Body, httpControlBytes, &observation); err != nil {
			return nil, err
		}
		received = append(received, observation)
		ack := httpAcknowledgement{httpBinding: httpJobBinding(job), Kind: "status", Sequence: observation.Status.Sequence,
			LogSequence: observation.Status.LogSequence}
		if len(received) == 2 {
			return nil, errors.New("connection closed before the parent acknowledged Status")
		}
		if len(received) == 3 {
			ack.Failure = &httpFailure{Message: "record store reached limit", CaptureLimit: true}
			return respond(http.StatusInternalServerError, ack)
		}
		return respond(http.StatusOK, ack)
	})}
	base, _ := url.Parse("http://worker.test")
	status := newWorkerStatus(job, time.Now())
	stops := 0
	status.onCaptureLimit = func() {
		stops++
		status.stopping()
	}
	transport := &httpExecutionClient{client: client, base: base}
	if err := status.emit(t.Context(), transport, attemptRecord{Kind: attemptRecordDiscussion,
		Agent: "native", Text: "saved prefix"}); err != nil {
		t.Fatalf("acknowledged prefix: %v", err)
	}
	if err := status.emit(t.Context(), transport, attemptRecord{Kind: attemptRecordDiscussion,
		Agent: "native", Text: "private record content"}); !errors.Is(err, errRemoteCaptureLimit) {
		t.Fatalf("conclusive store refusal = %v", err)
	}
	if stops != 1 || !status.captureLimited.Load() || !status.incomplete || status.pending != nil || status.ack != 2 {
		t.Fatalf("cap did not stop capture and advance over saved marker: stops=%d limited=%t incomplete=%t pending=%+v ack=%d",
			stops, status.captureLimited.Load(), status.incomplete, status.pending, status.ack)
	}
	status.phase("cleanup", 0, "")
	if err := status.send(t.Context(), transport); err != nil {
		t.Fatalf("bounded Status after cap: %v", err)
	}
	if len(received) != 4 || len(received[0].Logs) != 1 || received[0].Logs[0].Sequence != 1 ||
		received[0].Logs[0].Text != "saved prefix" || len(received[1].Logs) != 1 ||
		len(received[2].Logs) != 1 || received[1].Logs[0] != received[2].Logs[0] ||
		received[1].Logs[0].Sequence != 2 || received[1].Status.Sequence == received[2].Status.Sequence ||
		len(received[3].Logs) != 0 || received[3].Status.LogSequence != 2 ||
		received[3].Status.WorkerState != "stopping" || received[3].Status.Phase != "cleanup" {
		t.Fatalf("Status lost or replayed rejected private text: %+v", received)
	}
	if err := status.drain(t.Context(), transport); err == nil || len(received) != 4 {
		t.Fatalf("cap falsely wrote coverage: error=%v requests=%d", err, len(received))
	}
}

func TestCaptureLimitDiagnosticKeepsCleanupWhenFull(t *testing.T) {
	diagnostics := make([]Diagnostic, maxDiagnostics)
	for i := range diagnostics {
		diagnostics[i] = Diagnostic{Code: "other", Message: "safe"}
	}
	diagnostics[len(diagnostics)-1] = Diagnostic{Code: "cleanup_failed", Message: "cleanup remains visible"}
	result := withCaptureLimitDiagnostic(Result{Answer: "would publish", Changes: []workspace.Change{{}}, Diagnostics: diagnostics})
	if result.Answer != "" || result.Changes != nil || len(result.Diagnostics) != maxDiagnostics {
		t.Fatalf("cap did not prevent publication or keep diagnostic bound: %+v", result)
	}
	var capture, cleanup int
	for _, diagnostic := range result.Diagnostics {
		switch diagnostic.Code {
		case "capture_limit":
			capture++
			if diagnostic.Message != captureLimitMessage {
				t.Fatalf("capture marker text = %q", diagnostic.Message)
			}
		case "cleanup_failed":
			cleanup++
		}
	}
	if capture != 1 || cleanup != 1 {
		t.Fatalf("full diagnostic list lost required markers: capture=%d cleanup=%d", capture, cleanup)
	}
}

func TestCaptureLimitSurvivesDegradedOutcome(t *testing.T) {
	job := testJob(command.Comment)
	var sent httpOutcome
	var wire []byte
	client := &http.Client{Transport: httpClientTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/outcome" {
			return nil, errors.New("unexpected worker request")
		}
		var err error
		wire, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if err := httpDecode(bytes.NewReader(wire), httpOutcomeBytes, &sent); err != nil {
			return nil, err
		}
		ack, err := httpEncode(httpAcknowledgement{httpBinding: httpJobBinding(job), Kind: "outcome", Sequence: sent.Sequence}, httpControlBytes)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewReader(ack))}, nil
	})}
	base, _ := url.Parse("http://worker.test")
	checkpoint := &httpClientCheckpoints{transport: &httpExecutionClient{client: client, base: base},
		control: t.Context(), binding: httpJobBinding(job)}
	result := Result{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head,
		Diagnostics: []Diagnostic{{Code: "cleanup_failed", Message: "private cleanup details"},
			{Code: "bad", Message: "\x00"}}}
	err, _ := checkpoint.outcomeWithCauseAndRunError(result, errors.Join(monitor.ErrTerminalJob, errRemoteCaptureLimit), nil)
	if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !checkpoint.transport.outcomeDegraded ||
		sent.Failure == nil || !sent.Failure.Terminal || len(sent.Result.Diagnostics) != 2 ||
		sent.Result.Diagnostics[0].Code != "cleanup_failed" || sent.Result.Diagnostics[1] !=
		(Diagnostic{Code: "capture_limit", Message: captureLimitMessage}) || bytes.Contains(wire, []byte("private cleanup details")) {
		t.Fatalf("degraded cap outcome lost fixed evidence: error=%v outcome=%+v wire=%q", err, sent, wire)
	}
}
