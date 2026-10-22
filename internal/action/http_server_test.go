package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

type httpServerTestResult struct {
	result Result
	err    error
}

type httpServerTestExchange struct {
	status int
	ack    httpAcknowledgement
	err    error
}

type httpServerTest struct {
	executor *HTTPExecutor
	server   *httptest.Server
	job      Job
	tx       *httpTransaction
	done     chan httpServerTestResult
}

func newHTTPServerTest(t *testing.T, ctx context.Context, job Job, sink Checkpoints, wrap func(http.Handler) http.Handler) *httpServerTest {
	t.Helper()
	executor, err := NewHTTPExecutor(job.Identity.ExecutorID)
	if err != nil {
		t.Fatal(err)
	}
	var handler http.Handler = executor
	if wrap != nil {
		handler = wrap(handler)
	}
	f := &httpServerTest{executor: executor, server: httptest.NewServer(handler), job: job, done: make(chan httpServerTestResult, 1)}
	f.server.Client().Timeout = 5 * time.Second
	t.Cleanup(func() {
		f.server.CloseClientConnections()
		if tx := executor.transaction(); tx != nil {
			// Expedite failed-test cleanup without waiting the real 30-second control grace.
			tx.mutation.Lock()
			tx.mu.Lock()
			tx.fenced = true
			select {
			case <-tx.done:
			default:
				close(tx.done)
			}
			tx.mu.Unlock()
			tx.mutation.Unlock()
		}
		_ = executor.Close()
		f.server.Close()
	})
	go func() {
		result, err := executor.Execute(ctx, job, sink)
		f.done <- httpServerTestResult{result, err}
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		executor.mu.Lock()
		tx, changed := executor.active, executor.changed
		if tx != nil {
			tx.mu.Lock()
			ready := tx.ready
			tx.mu.Unlock()
			if ready {
				f.tx = tx
				executor.mu.Unlock()
				return f
			}
		}
		executor.mu.Unlock()
		select {
		case <-changed:
		case result := <-f.done:
			t.Fatalf("Execute ended before assignment admission: %+v error=%v", result.result, result.err)
		case <-timer.C:
			t.Fatal("assignment was not made ready")
		}
	}
}

func (f *httpServerTest) pull(t *testing.T) {
	t.Helper()
	body, err := json.Marshal(httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID})
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.server.Client().Post(f.server.URL+"/next", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var assignment httpAssignment
	if err := httpDecode(response.Body, httpJobBytes, &assignment); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("assignment: status=%d error=%v", response.StatusCode, err)
	}
	if !reflect.DeepEqual(assignment.Job, f.job) || assignment.Expires <= time.Now().UnixNano() {
		t.Fatalf("assignment changed supplied Job or lost its remaining budget: %+v", assignment)
	}
}

func (f *httpServerTest) request(path string, value any) httpServerTestExchange {
	body, err := json.Marshal(value)
	if err != nil {
		return httpServerTestExchange{err: err}
	}
	return f.raw(path, bytes.NewReader(body))
}

func (f *httpServerTest) raw(path string, body io.Reader) httpServerTestExchange {
	response, err := f.server.Client().Post(f.server.URL+path, "application/json", body)
	if err != nil {
		return httpServerTestExchange{err: err}
	}
	defer response.Body.Close()
	result := httpServerTestExchange{status: response.StatusCode}
	result.err = httpDecode(response.Body, httpControlBytes, &result.ack)
	return result
}

func (f *httpServerTest) ack(t *testing.T, path string, value any, status int, sequence uint64) httpAcknowledgement {
	t.Helper()
	got := f.request(path, value)
	if got.err != nil || got.status != status || got.ack.Kind != strings.TrimPrefix(path, "/") || got.ack.Sequence != sequence {
		t.Fatalf("%s acknowledgement: %+v", path, got)
	}
	return got.ack
}

func (f *httpServerTest) result(t *testing.T) httpServerTestResult {
	t.Helper()
	select {
	case result := <-f.done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not finish after outcome or callback drain")
		return httpServerTestResult{}
	}
}

func (f *httpServerTest) call(sequence uint64) httpCall {
	return httpCall{Sequence: sequence, Checkpoint: CallCheckpoint{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head,
		Call: CallUsage{Provider: f.job.Main.Provider, Model: f.job.Main.Model, Effort: f.job.Main.Effort, Role: roleMain, Round: 1}}}
}

func (f *httpServerTest) outcome(sequence uint64, runErr error) httpOutcome {
	return httpOutcome{Sequence: sequence, Result: Result{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head}, Failure: newHTTPFailure(runErr)}
}

func TestHTTPExecutorBindingSequenceAndRawCheckpointFacts(t *testing.T) {
	var before, after, review atomic.Int32
	paid := make(chan CallUsage, 1)
	f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{
		before: func(CallCheckpoint) error { before.Add(1); return nil },
		after:  func(value CallCheckpoint) error { after.Add(1); paid <- value.Call; return nil },
		review: func(ReviewCheckpoint) error { review.Add(1); return nil },
	}, nil)
	f.pull(t)
	for _, change := range []struct {
		name string
		edit func(*CallCheckpoint)
	}{
		{"version", func(c *CallCheckpoint) { c.Version++ }},
		{"job", func(c *CallCheckpoint) { c.Identity.JobID += "-other" }},
		{"attempt", func(c *CallCheckpoint) { c.Identity.AttemptID += "-other" }},
		{"executor", func(c *CallCheckpoint) { c.Identity.ExecutorID += "-other" }},
		{"repository", func(c *CallCheckpoint) { c.Identity.Repository = "other/repo" }},
		{"repository ID", func(c *CallCheckpoint) { c.Identity.RepositoryID++ }},
		{"owner ID", func(c *CallCheckpoint) { c.Identity.OwnerID++ }},
		{"requester", func(c *CallCheckpoint) { c.Identity.Requester += "-other" }},
		{"requester ID", func(c *CallCheckpoint) { c.Identity.RequesterID++ }},
		{"number", func(c *CallCheckpoint) { c.Identity.Number++ }},
		{"subject", func(c *CallCheckpoint) { c.Identity.SubjectType = "PullRequest" }},
		{"source", func(c *CallCheckpoint) { c.SourceRef = strings.Repeat("a", 40) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			call := f.call(1)
			change.edit(&call.Checkpoint)
			ack := f.ack(t, "/before", call, http.StatusConflict, 1)
			if ack.Failure == nil {
				t.Fatal("foreign binding was acknowledged")
			}
		})
	}
	for _, sequence := range []uint64{0, 2} {
		f.ack(t, "/before", f.call(sequence), http.StatusConflict, sequence)
	}
	if before.Load() != 0 {
		t.Fatal("unbound or out-of-order request reached checkpoint sink")
	}
	f.ack(t, "/before", f.call(1), http.StatusOK, 1)
	f.ack(t, "/before", f.call(1), http.StatusConflict, 1)
	call := f.call(2)
	call.Checkpoint.Call.Tokens = usage.Tokens{Input: -1, CachedInput: 7, Output: 9}
	call.Checkpoint.Call.Approved, call.Checkpoint.Call.HasBlockers, call.Checkpoint.Call.NoBlockers = true, true, true
	f.ack(t, "/after", call, http.StatusOK, 2)
	if got := <-paid; got != call.Checkpoint.Call {
		t.Fatalf("transport changed raw failed-call evidence: got=%+v want=%+v", got, call.Checkpoint.Call)
	}
	checkpoint := ReviewCheckpoint{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Calls: []CallUsage{call.Checkpoint.Call}}
	foreign := checkpoint
	foreign.Identity.AttemptID += "-other"
	f.ack(t, "/review", httpReview{Sequence: 3, Checkpoint: foreign}, http.StatusConflict, 3)
	f.ack(t, "/review", httpReview{Sequence: 3, Checkpoint: checkpoint}, http.StatusOK, 3)
	status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "prepare"}
	f.ack(t, "/status", httpObservation{Sequence: 4, Status: status}, http.StatusOK, 4)
	status.Sequence, status.Phase = 2, "upload"
	f.ack(t, "/status", httpObservation{Sequence: 5, Status: status}, http.StatusOK, 5)
	if got, ok := f.executor.Status(); !ok || got != status {
		t.Fatalf("Status did not preserve its independent observation sequence: %+v %v", got, ok)
	}
	outcome := f.outcome(6, errors.New("known failed result"))
	outcome.Result.Calls = []CallUsage{call.Checkpoint.Call}
	f.ack(t, "/outcome", outcome, http.StatusOK, 6)
	got := f.result(t)
	if got.err == nil || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !reflect.DeepEqual(got.result, outcome.Result) || before.Load() != 1 || after.Load() != 1 || review.Load() != 1 {
		t.Fatalf("known outcome or checkpoint count changed: %+v error=%v counts=%d/%d/%d", got.result, got.err, before.Load(), after.Load(), review.Load())
	}
	if duplicate := f.request("/outcome", outcome); duplicate.status != http.StatusConflict || duplicate.err != nil {
		t.Fatalf("second outcome was admitted: %+v", duplicate)
	}
}

func TestHTTPExecutorEncodedRequestBoundary(t *testing.T) {
	for _, route := range []struct {
		path  string
		limit int
	}{
		{"/before", httpControlBytes},
		{"/review", httpReviewBytes},
		{"/outcome", httpOutcomeBytes},
	} {
		t.Run(route.path, func(t *testing.T) {
			var saves atomic.Int32
			f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{
				before: func(CallCheckpoint) error { saves.Add(1); return nil },
				review: func(ReviewCheckpoint) error { saves.Add(1); return nil },
			}, nil)
			f.pull(t)
			var value any = f.call(1)
			if route.path == "/review" {
				value = httpReview{Sequence: 1, Checkpoint: ReviewCheckpoint{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head}}
			} else if route.path == "/outcome" {
				value = f.outcome(1, errors.New("known failure"))
			}
			body, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			padding := bytes.Repeat([]byte(" "), route.limit-len(body)+1)
			oversize := f.raw(route.path, io.MultiReader(bytes.NewReader(body), bytes.NewReader(padding)))
			if oversize.err != nil || oversize.status != http.StatusRequestEntityTooLarge || oversize.ack.Failure == nil || saves.Load() != 0 {
				t.Fatalf("limit+1 body was admitted: %+v saves=%d", oversize, saves.Load())
			}
			exact := f.raw(route.path, io.MultiReader(bytes.NewReader(body), bytes.NewReader(padding[:len(padding)-1])))
			if exact.err != nil || exact.status != http.StatusOK || exact.ack.Failure != nil || exact.ack.Sequence != 1 {
				t.Fatalf("exact-limit body was refused: %+v", exact)
			}
			if route.path != "/outcome" {
				if saves.Load() != 1 {
					t.Fatalf("exact-limit body reached sink %d times", saves.Load())
				}
				f.ack(t, "/outcome", f.outcome(2, errors.New("known failure")), http.StatusOK, 2)
			}
			if got := f.result(t); got.err == nil || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
				t.Fatalf("known outcome was lost after body-bound rejection: %v", got.err)
			}
		})
	}
}

func TestHTTPExecutorBusyAndSingleAssignment(t *testing.T) {
	f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	f.pull(t)
	if result, err := f.executor.Execute(context.Background(), f.job, runnerExecutionCheckpoints{}); err == nil || !result.matchesJob(f.job) {
		t.Fatalf("busy executor admitted another action: %+v error=%v", result, err)
	}
	if got := f.request("/next", httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID}); got.err != nil || got.status != http.StatusConflict {
		t.Fatalf("assignment was delivered twice: %+v", got)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if result := f.result(t); result.err != nil {
		t.Fatal(result.err)
	}
}

func TestHTTPExecutorCanceledBeforeConsumesOnlyBoundSequence(t *testing.T) {
	var before atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newHTTPServerTest(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{before: func(CallCheckpoint) error { before.Add(1); return nil }}, nil)
	f.pull(t)
	cancel()
	<-f.tx.ctx.Done()
	foreign := f.call(1)
	foreign.Checkpoint.Identity.AttemptID += "-other"
	f.ack(t, "/before", foreign, http.StatusConflict, 1)
	malformed := f.raw("/before", strings.NewReader(`{"sequence":1,"unknown":true}`))
	if malformed.err != nil || malformed.status != http.StatusBadRequest {
		t.Fatalf("malformed body was admitted: %+v", malformed)
	}
	ack := f.ack(t, "/before", f.call(1), http.StatusConflict, 1)
	if ack.httpBinding != f.tx.binding || !errors.Is(ack.Failure.err(), context.Canceled) || before.Load() != 0 {
		t.Fatalf("canceled Before reached paid admission or lost its known cause: %+v saves=%d", ack, before.Load())
	}
	status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "upload"}
	f.ack(t, "/status", httpObservation{Sequence: 2, Status: status}, http.StatusOK, 2)
	outcome := f.outcome(3, context.Canceled)
	f.ack(t, "/outcome", outcome, http.StatusOK, 3)
	got := f.result(t)
	if !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrTerminalJob) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("known unpaid cancellation became ambiguous: %+v error=%v", got.result, got.err)
	}
}

func TestHTTPExecutorFencesAdmissionAndDrainsEnteredFailedSave(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var saved CallUsage
	storeErr := errors.New("After save failed")
	f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{after: func(value CallCheckpoint) error {
		close(entered)
		<-release
		saved = value.Call
		return storeErr
	}}, nil)
	t.Cleanup(unblock)
	f.pull(t)
	call := f.call(1)
	call.Checkpoint.Call.Tokens = usage.Tokens{Input: 19, CachedInput: 3, Output: 7}
	acknowledged := make(chan httpServerTestExchange, 1)
	go func() { acknowledged <- f.request("/after", call) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("After did not enter store")
	}
	if busy := f.request("/review", httpReview{Sequence: 2}); busy.err != nil || busy.status != http.StatusConflict {
		t.Fatalf("concurrent mutation queued behind store: %+v", busy)
	}
	// Drive the private grace fence without sleeping through the production 30-second grace.
	f.tx.mu.Lock()
	f.tx.fenced = true
	close(f.tx.done)
	f.tx.mu.Unlock()
	<-f.tx.ctx.Done()
	select {
	case result := <-f.done:
		t.Fatalf("Execute returned while an entered store could still mutate its adapter: %+v", result)
	default:
	}
	unblock()
	select {
	case ack := <-acknowledged:
		if ack.err != nil || ack.status != http.StatusInternalServerError || ack.ack.Sequence != 1 || ack.ack.Failure == nil || ack.ack.Failure.Message != storeErr.Error() {
			t.Fatalf("entered After lost its acknowledgement: %+v", ack)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("entered After did not drain")
	}
	got := f.result(t)
	if !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !got.result.matchesJob(f.job) || len(got.result.Calls) != 0 || saved != call.Checkpoint.Call {
		t.Fatalf("drain invented or lost paid evidence: result=%+v saved=%+v error=%v", got.result, saved, got.err)
	}
	if late := f.request("/after", f.call(2)); late.err != nil || late.status != http.StatusConflict {
		t.Fatalf("late callback reached finalized adapter: %+v", late)
	}
	if _, err := f.executor.Execute(context.Background(), f.job, runnerExecutionCheckpoints{}); err == nil {
		t.Fatal("unknown outcome released executor capacity")
	}
}

type httpServerFailWrite struct{ http.ResponseWriter }

func (w httpServerFailWrite) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w httpServerFailWrite) Write([]byte) (int, error)   { return 0, io.ErrClosedPipe }

func TestHTTPExecutorReceivedOutcomeSurvivesLostReceiptACK(t *testing.T) {
	for _, runErr := range []error{nil, errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded)} {
		t.Run(fmt.Sprint(runErr), func(t *testing.T) {
			f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/outcome" {
						w = httpServerFailWrite{w}
					}
					next.ServeHTTP(w, r)
				})
			})
			f.pull(t)
			outcome := f.outcome(1, runErr)
			outcome.Result.Answer = "Complete received answer"
			outcome.Result.Calls = []CallUsage{{Provider: "codex", Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 13, Output: 5}, Complete: true}}
			if ack := f.request("/outcome", outcome); ack.err == nil {
				t.Fatalf("receipt ACK unexpectedly survived injected write failure: %+v", ack)
			}
			got := f.result(t)
			if !reflect.DeepEqual(got.result, outcome.Result) || (got.err == nil) != (runErr == nil) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || errors.Is(got.err, monitor.ErrTerminalJob) != errors.Is(runErr, monitor.ErrTerminalJob) || errors.Is(got.err, context.DeadlineExceeded) != errors.Is(runErr, context.DeadlineExceeded) {
				t.Fatalf("lost receipt erased or changed conclusive outcome: %+v error=%v", got.result, got.err)
			}
			if _, err := f.executor.Execute(context.Background(), f.job, runnerExecutionCheckpoints{}); err == nil {
				t.Fatal("lost receipt ACK released quarantined capacity")
			}
		})
	}
}

func TestHTTPExecutorLateKnownOutcomeRetainsFactsAndActualContextCause(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%v", deadline), func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			f := newHTTPServerTest(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
			f.pull(t)
			if !deadline {
				cancel()
			}
			<-ctx.Done()
			<-f.tx.ctx.Done()
			outcome := f.outcome(1, nil)
			outcome.Result.Answer = "Received after the accepted action stopped"
			outcome.Result.Calls = []CallUsage{{Provider: "codex", Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 23, Output: 3}, Complete: true}}
			f.ack(t, "/outcome", outcome, http.StatusOK, 1)
			got := f.result(t)
			if !reflect.DeepEqual(got.result, outcome.Result) || !errors.Is(got.err, ctx.Err()) || errors.Is(got.err, monitor.ErrTerminalJob) != deadline || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
				t.Fatalf("late known outcome lost paid facts or fixed expiry: %+v error=%v", got.result, got.err)
			}
		})
	}
}

func TestHTTPExecutorKnownOutcomeResolvesInternalTransportCancellation(t *testing.T) {
	f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	f.pull(t)
	f.tx.stopExecution(io.ErrClosedPipe, true)
	<-f.tx.ctx.Done()
	outcome := f.outcome(1, nil)
	outcome.Result.Answer = "Conclusive result received within the accepted budget"
	f.ack(t, "/outcome", outcome, http.StatusOK, 1)
	if got := f.result(t); got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("internal ACK failure poisoned a conclusive received result: %+v error=%v", got.result, got.err)
	}
	f.executor.mu.Lock()
	unavailable := f.executor.unavailable
	f.executor.mu.Unlock()
	if unavailable {
		t.Fatal("resolved transport ambiguity invented a quarantine fact")
	}
}

func TestHTTPExecutorCloseKeepsWatchAndPaidReceiptGraceAvailable(t *testing.T) {
	paid := make(chan CallUsage, 1)
	f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{after: func(value CallCheckpoint) error {
		paid <- value.Call
		return nil
	}}, nil)
	f.pull(t)
	body, err := json.Marshal(f.tx.binding)
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.server.Client().Post(f.server.URL+"/cancel", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("watch admission headers were not flushed: %d %v", response.StatusCode, response.Header)
	}
	if duplicate := f.request("/cancel", f.tx.binding); duplicate.err != nil || duplicate.status != http.StatusConflict {
		t.Fatalf("concurrent cancellation watch was admitted: %+v", duplicate)
	}
	closed := make(chan error, 1)
	go func() { closed <- f.executor.Close() }()
	var cancellation httpCancellation
	if err := httpDecode(response.Body, httpControlBytes, &cancellation); err != nil || cancellation.httpBinding != f.tx.binding || cancellation.Reason != "canceled" {
		t.Fatalf("Close did not deliver bound known cancellation: %+v error=%v", cancellation, err)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close ended before receipt grace and outcome: %v", err)
	default:
	}
	call := f.call(1)
	call.Checkpoint.Call.Tokens = usage.Tokens{Input: 31, Output: 2}
	f.ack(t, "/after", call, http.StatusOK, 1)
	if got := <-paid; got != call.Checkpoint.Call {
		t.Fatalf("Close lost a paid receipt in grace: %+v", got)
	}
	f.ack(t, "/outcome", f.outcome(2, context.Canceled), http.StatusOK, 2)
	if got := f.result(t); !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("known Close cancellation became ambiguous: %v", got.err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join completed HTTP handlers")
	}
}

type httpServerHeldWrite struct {
	http.ResponseWriter
	entered chan struct{}
	release chan struct{}
}

func (w httpServerHeldWrite) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w httpServerHeldWrite) Write(body []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseWriter.Write(body)
}

func TestHTTPExecutorSealedOutcomeDoesNotAcquireDeadlineDuringACKDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f := newHTTPServerTest(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/outcome" {
				w = httpServerHeldWrite{ResponseWriter: w, entered: entered, release: release}
			}
			next.ServeHTTP(w, r)
		})
	})
	t.Cleanup(unblock)
	f.pull(t)
	outcome := f.outcome(1, nil)
	outcome.Result.Answer = "Sealed before expiry"
	acknowledged := make(chan httpServerTestExchange, 1)
	go func() { acknowledged <- f.request("/outcome", outcome) }()
	select {
	case <-entered:
		if ctx.Err() != nil {
			t.Fatal("fixture did not seal outcome before parent deadline")
		}
	case <-ctx.Done():
		t.Fatal("outcome did not enter ACK write before deadline")
	}
	<-ctx.Done()
	select {
	case result := <-f.done:
		t.Fatalf("Execute returned while its receipt ACK was still being written: %+v", result)
	default:
	}
	unblock()
	if ack := <-acknowledged; ack.err != nil || ack.status != http.StatusOK || ack.ack.Failure != nil {
		t.Fatalf("sealed outcome ACK did not drain: %+v", ack)
	}
	if got := f.result(t); got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("ACK drain invented a deadline after the outcome was already sealed: %+v error=%v", got.result, got.err)
	}
}

func TestHTTPExecutorMissingOutcomeClosesRealGraceAndQuarantines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newHTTPServerTest(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	f.pull(t)
	cancel()
	<-f.tx.ctx.Done()
	select {
	case got := <-f.done:
		t.Fatalf("delivered action skipped receipt grace: %+v error=%v", got.result, got.err)
	default:
	}
	select {
	case got := <-f.done:
		if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || errors.Is(got.err, monitor.ErrTerminalJob) || !got.result.matchesJob(f.job) || len(got.result.Calls) != 0 {
			t.Fatalf("missing outcome invented known execution facts: %+v error=%v", got.result, got.err)
		}
	case <-time.After(httpControlTimeout + 5*time.Second):
		t.Fatal("real control grace did not close missing-outcome admission")
	}
	if got := f.request("/outcome", f.outcome(1, nil)); got.err != nil || got.status != http.StatusConflict {
		t.Fatalf("outcome was accepted after grace finalization: %+v", got)
	}
	if _, err := f.executor.Execute(context.Background(), f.job, runnerExecutionCheckpoints{}); err == nil {
		t.Fatal("missing outcome released unproved worker capacity")
	}
}
