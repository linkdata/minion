package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workroot"
	"github.com/linkdata/minion/internal/workspace"
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

type refusingOutcomeCheckpoints struct{ runnerExecutionCheckpoints }

func (refusingOutcomeCheckpoints) ReceiveCompletedResult(Result) error {
	return monitor.ErrTerminalJob
}

type httpDeadlineAfterCheck struct {
	context.Context
	checks atomic.Int32
}

type httpAssignmentOrderWriter struct {
	*httpRepullRecorder
	saved *atomic.Bool
	early bool
}

func (w *httpAssignmentOrderWriter) WriteHeader(status int) {
	if status == http.StatusOK && !w.saved.Load() {
		w.early = true
	}
	w.ResponseRecorder.WriteHeader(status)
}

func (c *httpDeadlineAfterCheck) Err() error {
	if c.checks.Add(1) > 1 {
		return context.DeadlineExceeded
	}
	return nil
}

func newHTTPServerTest(t *testing.T, ctx context.Context, job Job, sink Checkpoints, wrap func(http.Handler) http.Handler, credentials ...WorkerCredentials) *httpServerTest {
	return newHTTPServerTestWithServer(t, ctx, job, sink, wrap, httptest.NewServer, credentials...)
}

func newHTTPServerTestInMemory(t *testing.T, ctx context.Context, job Job, sink Checkpoints) *httpServerTest {
	return newHTTPServerTestWithServer(t, ctx, job, sink, nil, func(h http.Handler) *httptest.Server {
		return httptest.NewTestServer(t, h)
	})
}

func newHTTPServerTestWithServer(t *testing.T, ctx context.Context, job Job, sink Checkpoints, wrap func(http.Handler) http.Handler, newServer func(http.Handler) *httptest.Server, credentials ...WorkerCredentials) *httpServerTest {
	return newHTTPServerTestWithStopServer(t, ctx, job, sink, wrap, newServer, 0, credentials...)
}

func newHTTPServerTestWithStopServer(t *testing.T, ctx context.Context, job Job, sink Checkpoints, wrap func(http.Handler) http.Handler, newServer func(http.Handler) *httptest.Server, stopBudget time.Duration, credentials ...WorkerCredentials) *httpServerTest {
	return newHTTPServerTestWithStopServerRoot(t, ctx, job, sink, wrap, newServer, stopBudget, "", credentials...)
}

func newHTTPServerTestWithRoot(t *testing.T, ctx context.Context, job Job, sink Checkpoints, workRoot string) *httpServerTest {
	return newHTTPServerTestWithStopServerRoot(t, ctx, job, sink, nil, func(h http.Handler) *httptest.Server {
		return httptest.NewTestServer(t, h)
	}, 0, workRoot)
}

func newHTTPServerTestWithStopServerRoot(t *testing.T, ctx context.Context, job Job, sink Checkpoints, wrap func(http.Handler) http.Handler, newServer func(http.Handler) *httptest.Server, stopBudget time.Duration, workRoot string, credentials ...WorkerCredentials) *httpServerTest {
	t.Helper()
	executor, err := NewHTTPExecutor(job.Identity.ExecutorID)
	if err != nil {
		t.Fatal(err)
	}
	if workRoot != "" {
		if err := executor.SetWorkRoot(workRoot); err != nil {
			t.Fatal(err)
		}
	}
	if len(credentials) > 0 {
		if err := executor.SetWorkerCredentials(credentials[0]); err != nil {
			t.Fatal(err)
		}
	}
	var handler http.Handler = executor
	if wrap != nil {
		handler = wrap(handler)
	}
	f := &httpServerTest{executor: executor, server: newServer(handler), job: job, done: make(chan httpServerTestResult, 1)}
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
		var result Result
		var err error
		if stopBudget == 0 {
			result, err = executor.Execute(ctx, job, sink)
		} else {
			result, err = executor.ExecuteWithStop(ctx, stopBudget, job, sink)
		}
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

func (f *httpServerTest) pull(t *testing.T) httpAssignment {
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
	if !sameAssignmentJob(f.job, assignment.Job) || assignment.HeldFor < 0 {
		t.Fatalf("assignment changed supplied Job or lost its remaining budget: job_matches=%t budget=%s held=%s", sameAssignmentJob(f.job, assignment.Job), assignment.Job.RuntimeBudget, assignment.HeldFor)
	}
	return assignment
}

func sameAssignmentJob(want, got Job) bool {
	if got.RuntimeBudget <= 0 || got.RuntimeBudget > want.RuntimeBudget {
		return false
	}
	got.RuntimeBudget = want.RuntimeBudget
	return reflect.DeepEqual(got, want)
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
		t.Fatalf("%s acknowledgement: %+v failure=%+v", path, got, got.ack.Failure)
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

func TestHTTPExecutorDispatchUsesRemainingRuntime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.RuntimeBudget = 3 * time.Second
		f := newHTTPServerTestInMemory(t, t.Context(), job, runnerExecutionCheckpoints{})
		time.Sleep(2 * time.Second)
		assignment := f.pull(t)
		if assignment.Job.RuntimeBudget <= 0 || assignment.Job.RuntimeBudget > time.Second || assignment.HeldFor < 0 {
			t.Fatalf("dispatch renewed or lost action runtime: budget=%s held=%s", assignment.Job.RuntimeBudget, assignment.HeldFor)
		}
		f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
		if result := f.result(t); result.err != nil {
			t.Fatalf("known outcome lost after reduced dispatch budget: %v", result.err)
		}
	})
}

func TestHTTPExecutorBeforeDeliveryPrecedesAssignmentBytes(t *testing.T) {
	job := testJob(command.Comment)
	f := newHTTPServerTestInMemory(t, t.Context(), job, runnerExecutionCheckpoints{})
	var saved atomic.Bool
	var calls atomic.Int32
	f.tx.mu.Lock()
	f.tx.beforeDelivery = func(got Job) error {
		calls.Add(1)
		if !reflect.DeepEqual(got, job) {
			return errors.New("pre-delivery Job differs from admitted Job")
		}
		saved.Store(true)
		return nil
	}
	f.tx.mu.Unlock()
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: job.Identity.ExecutorID}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/next", bytes.NewReader(pull))
	request.Header.Set("Content-Type", "application/json")
	response := &httpAssignmentOrderWriter{httpRepullRecorder: &httpRepullRecorder{httptest.NewRecorder()}, saved: &saved}
	f.executor.serveNext(response, request)
	var assignment httpAssignment
	if err := httpDecode(response.Body, httpJobBytes, &assignment); err != nil || response.Code != http.StatusOK || response.early || !saved.Load() || calls.Load() != 1 || !sameAssignmentJob(job, assignment.Job) {
		t.Fatalf("assignment preceded save or callback repeated: status=%d early=%t saved=%t calls=%d error=%v", response.Code, response.early, saved.Load(), calls.Load(), err)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if result := f.result(t); result.err != nil {
		t.Fatalf("saved assignment lost known outcome: %v", result.err)
	}
}

func TestHTTPExecutorBeforeDeliveryFailureSendsNoAssignment(t *testing.T) {
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	var replaced atomic.Bool
	saveErr := errors.New("directory sync failed after replacement")
	f.tx.mu.Lock()
	f.tx.beforeDelivery = func(Job) error {
		replaced.Store(true)
		return saveErr
	}
	f.tx.mu.Unlock()
	reply := f.request("/next", httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID})
	if reply.err != nil || reply.status != http.StatusInternalServerError || reply.ack.Kind != "next" || reply.ack.Failure == nil || !replaced.Load() {
		t.Fatalf("failed save emitted assignment or lost state uncertainty: reply=%+v replaced=%t", reply, replaced.Load())
	}
	got := f.result(t)
	if !errors.Is(got.err, saveErr) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || f.tx.delivered || !replaced.Load() {
		t.Fatalf("failed save claimed delivery or rolled back possible replacement: error=%v delivered=%t replaced=%t", got.err, f.tx.delivered, replaced.Load())
	}
}

func TestHTTPExecutorAssignmentRevalidationFailureSendsNoJobBytes(t *testing.T) {
	for _, cause := range []error{monitor.ErrAcceptedDiscarded, monitor.ErrAcceptedRetry} {
		t.Run(cause.Error(), func(t *testing.T) {
			f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
			f.tx.mu.Lock()
			f.tx.beforeDelivery = func(Job) error { return cause }
			f.tx.mu.Unlock()
			reply := f.request("/next", httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID})
			if reply.err != nil || reply.status != http.StatusInternalServerError || reply.ack.Kind != "next" || reply.ack.Failure == nil || f.tx.delivered {
				t.Fatalf("failed final check sent assignment: reply=%+v delivered=%t", reply, f.tx.delivered)
			}
			if got := f.result(t); !errors.Is(got.err, cause) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
				t.Fatalf("pre-delivery refusal changed cause or claimed model work: %v", got.err)
			}
		})
	}
}

func TestHTTPExecutorChargesRuntimeForAssignmentRevalidation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		job.RuntimeBudget = 5 * time.Second
		f := newHTTPServerTestInMemory(t, t.Context(), job, runnerExecutionCheckpoints{})
		f.tx.mu.Lock()
		f.tx.beforeDelivery = func(Job) error {
			time.Sleep(2 * time.Second)
			return nil
		}
		f.tx.mu.Unlock()
		pullStarted := time.Now()
		assignment := f.pull(t)
		transit := time.Since(pullStarted) - assignment.HeldFor
		remaining := assignment.Job.RuntimeBudget - transit
		if remaining > 3*time.Second || remaining <= 0 {
			t.Fatalf("final check did not consume the original runtime: budget=%s transit=%s remaining=%s", assignment.Job.RuntimeBudget, transit, remaining)
		}
		f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
		if result := f.result(t); result.err != nil {
			t.Fatal(result.err)
		}
	})
}

func TestHTTPExecutorStopBeforeAssignmentCannotSucceed(t *testing.T) {
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	f.tx.forceStop()
	got := f.result(t)
	if !errors.Is(got.err, monitor.ErrRequestTransport) || f.tx.delivered {
		t.Fatalf("stopped before assignment reported success: error=%v delivered=%t", got.err, f.tx.delivered)
	}
}

func TestHTTPExecutorCancellationDrainsBeforeDelivery(t *testing.T) {
	for _, test := range []struct {
		name    string
		saveErr error
	}{
		{name: "saved"},
		{name: "save failed", saveErr: errors.New("late assignment save failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				f := newHTTPServerTestWithStopServer(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{}, nil,
					func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, time.Millisecond)
				entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				f.tx.mu.Lock()
				f.tx.beforeDelivery = func(Job) error {
					close(entered)
					<-release
					return test.saveErr
				}
				f.tx.mu.Unlock()
				pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID}, httpControlBytes)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/next", bytes.NewReader(pull))
				request.Header.Set("Content-Type", "application/json")
				response := &httpRepullRecorder{httptest.NewRecorder()}
				go func() {
					f.executor.serveNext(response, request)
					close(finished)
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("pre-delivery callback did not enter")
				}
				cancel()
				select {
				case <-f.tx.stop:
				case <-time.After(5 * time.Second):
					t.Fatal("pre-delivery callback blocked transaction stop")
				}
				select {
				case result := <-f.done:
					t.Fatalf("Execute returned before entered callback drained: %+v", result)
				default:
				}
				unblock()
				<-finished
				var ack httpAcknowledgement
				wantStatus := http.StatusConflict
				if test.saveErr != nil {
					wantStatus = http.StatusInternalServerError
				}
				if err := httpDecode(response.Body, httpControlBytes, &ack); err != nil || response.Code != wantStatus || ack.Failure == nil || f.tx.delivered {
					t.Fatalf("canceled callback delivered assignment: status=%d ack=%+v delivered=%t error=%v", response.Code, ack, f.tx.delivered, err)
				}
				if got := f.result(t); !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, test.saveErr) && test.saveErr != nil || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
					t.Fatalf("canceled callback changed pre-delivery outcome: %v", got.err)
				}
			})
		})
	}
}

func TestHTTPExecutorDisconnectedPullDoesNotClaimSavedAssignment(t *testing.T) {
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	f.tx.mu.Lock()
	f.tx.beforeDelivery = func(Job) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	f.tx.mu.Unlock()
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/next", bytes.NewReader(pull))
	request.Header.Set("Content-Type", "application/json")
	response := &httpRepullRecorder{httptest.NewRecorder()}
	go func() {
		f.executor.serveNext(response, request)
		close(finished)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("pre-delivery callback did not enter")
	}
	cancel()
	unblock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnected pull did not release its slot")
	}
	if response.Body.Len() != 0 || f.tx.delivered {
		t.Fatalf("disconnected pull emitted assignment: bytes=%d delivered=%t", response.Body.Len(), f.tx.delivered)
	}
	f.pull(t)
	if calls.Load() != 2 {
		t.Fatalf("fresh pull bypassed pre-delivery callback: calls=%d", calls.Load())
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if result := f.result(t); result.err != nil {
		t.Fatalf("disconnected pull poisoned later assignment: %v", result.err)
	}
}

func TestHTTPExecutorRejectsExpiryDuringAssignmentEncoding(t *testing.T) {
	job := testJob(command.Comment)
	executor, err := NewHTTPExecutor(job.Identity.ExecutorID)
	if err != nil {
		t.Fatal(err)
	}
	budget, stop := context.WithTimeout(t.Context(), time.Minute)
	defer stop()
	tx := &httpTransaction{job: job, ctx: &httpDeadlineAfterCheck{Context: budget}, budget: budget, ready: true}
	executor.active = tx
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: job.Identity.ExecutorID}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/next", bytes.NewReader(pull))
	request.Header.Set("Content-Type", "application/json")
	response := &httpRepullRecorder{httptest.NewRecorder()}
	executor.serveNext(response, request)
	var ack httpAcknowledgement
	if err := httpDecode(response.Body, httpControlBytes, &ack); err != nil || response.Code != http.StatusConflict || ack.Kind != "next" || ack.Failure == nil || !ack.Failure.Deadline || tx.delivered {
		t.Fatalf("expired assignment was claimed or rejected without a typed deadline: code=%d ack=%+v delivered=%t err=%v", response.Code, ack, tx.delivered, err)
	}
}

func TestHTTPExecutorCancellationWhileEncodingAssignment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const stopBudget = time.Millisecond
		f := newHTTPServerTestWithStopServer(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{}, nil,
			func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, stopBudget)
		pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID}, httpControlBytes)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/next", bytes.NewReader(pull))
		request.Header.Set("Content-Type", "application/json")
		response := &httpRepullRecorder{httptest.NewRecorder()}
		entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		go func() {
			f.executor.serveNextEncoded(response, request, func(assignment httpAssignment) ([]byte, error) {
				close(entered)
				<-release
				return httpEncode(assignment, httpJobBytes)
			})
			close(finished)
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("assignment encoding did not start")
		}
		canceledAt := time.Now()
		cancel()
		select {
		case <-f.tx.stop:
		case <-time.After(2 * stopBudget):
			t.Fatal("assignment encoding held the stop path")
		}
		if elapsed := time.Since(canceledAt); elapsed > 2*stopBudget {
			t.Fatalf("stop was delayed by assignment encoding: %s", elapsed)
		}
		if got := f.result(t); !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("unclaimed assignment changed cancellation: %+v error=%v", got.result, got.err)
		}
		unblock()
		<-finished
		var ack httpAcknowledgement
		if err := httpDecode(response.Body, httpControlBytes, &ack); err != nil || response.Code != http.StatusConflict || ack.Failure == nil || !ack.Failure.Canceled || f.tx.delivered {
			t.Fatalf("canceled encoding claimed assignment: status=%d ack=%+v delivered=%t error=%v", response.Code, ack, f.tx.delivered, err)
		}
	})
}

func TestHTTPExecutorBindingSequenceAndRawCheckpointFacts(t *testing.T) {
	var before, after, review atomic.Int32
	paid := make(chan CallUsage, 1)
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{
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
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 1)
	status.Sequence, status.Phase = 2, "upload"
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 2)
	if got, _, ok := f.executor.Status(); !ok || got != status {
		t.Fatalf("Status did not preserve its independent observation sequence: %+v %v", got, ok)
	}
	outcome := f.outcome(4, errors.New("known failed result"))
	outcome.Result.Calls = []CallUsage{call.Checkpoint.Call}
	f.ack(t, "/outcome", outcome, http.StatusOK, 4)
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
		{"/after", httpControlBytes},
		{"/status", httpControlBytes},
		{"/review", httpReviewBytes},
		{"/outcome", httpOutcomeBytes},
	} {
		t.Run(route.path, func(t *testing.T) {
			t.Parallel()
			var saves atomic.Int32
			f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{
				before: func(CallCheckpoint) error { saves.Add(1); return nil },
				after:  func(CallCheckpoint) error { saves.Add(1); return nil },
				review: func(ReviewCheckpoint) error { saves.Add(1); return nil },
			}, nil)
			f.server.Client().Timeout = httpControlTimeout + 5*time.Second
			f.pull(t)
			var value any = f.call(1)
			if route.path == "/status" {
				value = httpObservation{Status: Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "prepare"}}
			} else if route.path == "/review" {
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
				t.Fatalf("exact-limit body was refused: status=%d ack=%+v error=%v", exact.status, exact.ack, exact.err)
			}
			if route.path != "/outcome" {
				wantSaves, nextSequence := int32(1), uint64(2)
				if route.path == "/status" {
					wantSaves, nextSequence = 0, 1
				}
				if saves.Load() != wantSaves {
					t.Fatalf("exact-limit body reached sink %d times", saves.Load())
				}
				f.ack(t, "/outcome", f.outcome(nextSequence, errors.New("known failure")), http.StatusOK, nextSequence)
			}
			if got := f.result(t); got.err == nil || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
				t.Fatalf("known outcome was lost after body-bound rejection: %v", got.err)
			}
		})
	}
}

func TestHTTPExecutorPullAndCancelEncodedRequestBoundary(t *testing.T) {
	for _, path := range []string{"/next", "/cancel"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
			var value any = httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID}
			if path == "/cancel" {
				f.pull(t)
				value = f.tx.binding
			}
			body, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			padding := strings.Repeat(" ", httpControlBytes-len(body))
			oversize := f.raw(path, io.MultiReader(bytes.NewReader(body), strings.NewReader(padding+" ")))
			if oversize.err != nil || oversize.status != http.StatusRequestEntityTooLarge || oversize.ack.Failure == nil {
				t.Fatalf("limit+1 %s was not rejected: status=%d error=%v ack=%+v", path, oversize.status, oversize.err, oversize.ack)
			}
			response, err := f.server.Client().Post(f.server.URL+path, "application/json", io.MultiReader(bytes.NewReader(body), strings.NewReader(padding)))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("exact-limit %s refused: %d", path, response.StatusCode)
			}
			if path == "/next" {
				var assignment httpAssignment
				if err := httpDecode(response.Body, httpJobBytes, &assignment); err != nil || !sameAssignmentJob(f.job, assignment.Job) {
					t.Fatalf("exact pull did not preserve supplied assignment: error=%v", err)
				}
			}
			f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
			if path == "/cancel" {
				var cancellation httpCancellation
				if err := httpDecode(response.Body, httpControlBytes, &cancellation); err != nil || cancellation.httpBinding != f.tx.binding || cancellation.Reason != "completed" {
					t.Fatalf("exact watch lost bound completion: %+v error=%v", cancellation, err)
				}
			}
			if got := f.result(t); got.err != nil {
				t.Fatal(got.err)
			}
		})
	}
}

type httpStatusHeldBody struct {
	io.ReadCloser
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type httpSecondReadBody struct {
	io.ReadCloser
	reads   atomic.Int32
	entered chan struct{}
}

func (b *httpSecondReadBody) Read(p []byte) (int, error) {
	if b.reads.Add(1) == 2 {
		close(b.entered)
	}
	return b.ReadCloser.Read(p)
}

func (b *httpStatusHeldBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.ReadCloser.Read(p)
}

func TestHTTPExecutorReadingStatusDoesNotHoldPaidCallbackSlot(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/status" {
				r.Body = &httpStatusHeldBody{ReadCloser: r.Body, entered: entered, release: release}
			}
			next.ServeHTTP(w, r)
		})
	})
	t.Cleanup(unblock)
	f.pull(t)
	observation := httpObservation{Status: Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "prepare"}}
	done := make(chan httpServerTestExchange, 1)
	go func() { done <- f.request("/status", observation) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("observation did not enter its bounded body read")
	}
	f.ack(t, "/before", f.call(1), http.StatusOK, 1)
	unblock()
	if got := <-done; got.err != nil || got.status != http.StatusOK {
		t.Fatalf("observation did not drain: status=%d error=%v", got.status, got.err)
	}
	f.ack(t, "/outcome", f.outcome(2, nil), http.StatusOK, 2)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPExecutorRejectsStatusWhoseBodyFinishesAfterOutcome(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var hold atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/status" && hold.Load() {
				r.Body = &httpStatusHeldBody{ReadCloser: r.Body, entered: entered, release: release}
			}
			next.ServeHTTP(w, r)
		})
	})
	t.Cleanup(unblock)
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "prepare"}
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 1)
	_, receivedAt, _ := f.executor.Status()
	hold.Store(true)
	late := status
	late.Sequence = 2
	done := make(chan httpServerTestExchange, 1)
	go func() { done <- f.request("/status", httpObservation{Status: late}) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("late observation did not enter its body read")
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
	select {
	case got := <-done:
		t.Fatalf("late observation body finished before release: %+v", got)
	default:
	}
	unblock()
	if got := <-done; got.err != nil || got.status != http.StatusConflict || got.ack.Failure == nil {
		t.Fatalf("late observation was admitted after outcome: %+v", got)
	}
	if got, gotAt, ok := f.executor.Status(); !ok || got != status || !gotAt.Equal(receivedAt) {
		t.Fatalf("late observation replaced last accepted status or contact: status=%+v received=%v present=%t", got, gotAt, ok)
	}
}

func TestHTTPExecutorBusyAndSingleAssignment(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(30*time.Second))
	defer cancel()
	deadline, _ := ctx.Deadline()
	f := newHTTPServerTest(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	f.ack(t, "/next", httpPull{Version: ProtocolVersion - 1, ExecutorID: f.job.Identity.ExecutorID}, http.StatusConflict, 0)
	f.ack(t, "/next", httpPull{Version: ProtocolVersion, ExecutorID: "foreign"}, http.StatusConflict, 0)
	beforePull := time.Now()
	if assignment := f.pull(t); assignment.Job.RuntimeBudget > deadline.Sub(beforePull) {
		t.Fatalf("assignment exceeded parent deadline: %s", assignment.Job.RuntimeBudget)
	}
	if result, err := f.executor.Execute(t.Context(), f.job, runnerExecutionCheckpoints{}); err == nil || !result.matchesJob(f.job) {
		t.Fatalf("busy executor admitted another action: %+v error=%v", result, err)
	}
	if got := f.request("/next", httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID}); got.err != nil || got.status != http.StatusConflict || got.ack.Failure == nil || errors.Is(got.ack.Failure.err(), context.Canceled) {
		t.Fatalf("assignment was delivered twice: %+v", got)
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if result := f.result(t); result.err != nil {
		t.Fatal(result.err)
	}
}

func TestHTTPExecutorClosedPullCancellationDiffersFromUnavailable(t *testing.T) {
	executor, err := NewHTTPExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	f := &httpServerTest{executor: executor, server: httptest.NewServer(executor)}
	f.server.Client().Timeout = 5 * time.Second
	defer f.server.Close()
	pull := httpPull{Version: ProtocolVersion, ExecutorID: "local"}
	executor.mu.Lock()
	executor.unavailable = true
	executor.mu.Unlock()
	unavailable := f.request("/next", pull)
	if unavailable.err != nil || unavailable.status != http.StatusConflict || unavailable.ack.Failure == nil || errors.Is(unavailable.ack.Failure.err(), context.Canceled) {
		t.Fatalf("unavailable pull looked like shutdown: %+v", unavailable)
	}
	if err := executor.Close(); err != nil {
		t.Fatal(err)
	}
	closed := f.request("/next", pull)
	if closed.err != nil || closed.status != http.StatusConflict || closed.ack.Failure == nil || !errors.Is(closed.ack.Failure.err(), context.Canceled) {
		t.Fatalf("closed pull did not signal shutdown: %+v", closed)
	}
}

func TestHTTPExecutorCancelRequiresFullBinding(t *testing.T) {
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	f.pull(t)
	for _, test := range []struct {
		name string
		edit func(*httpBinding)
	}{
		{"version", func(b *httpBinding) { b.Version++ }},
		{"job", func(b *httpBinding) { b.Identity.JobID += "-other" }},
		{"attempt", func(b *httpBinding) { b.Identity.AttemptID += "-other" }},
		{"executor", func(b *httpBinding) { b.Identity.ExecutorID += "-other" }},
		{"repository", func(b *httpBinding) { b.Identity.Repository = "other/repo" }},
		{"repository ID", func(b *httpBinding) { b.Identity.RepositoryID++ }},
		{"owner ID", func(b *httpBinding) { b.Identity.OwnerID++ }},
		{"requester", func(b *httpBinding) { b.Identity.Requester += "-other" }},
		{"requester ID", func(b *httpBinding) { b.Identity.RequesterID++ }},
		{"number", func(b *httpBinding) { b.Identity.Number++ }},
		{"subject", func(b *httpBinding) { b.Identity.SubjectType = "PullRequest" }},
		{"source", func(b *httpBinding) { b.SourceRef = strings.Repeat("a", 40) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding := f.tx.binding
			test.edit(&binding)
			ack := f.ack(t, "/cancel", binding, http.StatusConflict, 0)
			f.tx.mu.Lock()
			watch := f.tx.watch
			f.tx.mu.Unlock()
			if ack.Failure == nil || watch || f.tx.ctx.Err() != nil {
				t.Fatalf("foreign watch changed bound action: ack=%+v watch=%t ctx=%v", ack, watch, f.tx.ctx.Err())
			}
		})
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPExecutorCanceledPullDoesNotClaimReadyAssignment(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var canceled atomic.Bool
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/next" && !canceled.Swap(true) {
				r = r.WithContext(ctx)
			}
			next.ServeHTTP(w, r)
		})
	})
	f.executor.mu.Lock()
	changed := f.executor.changed
	f.executor.mu.Unlock()
	response := f.request("/next", httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID})
	select {
	case <-changed:
		t.Fatal("canceled pull was announced as held")
	default:
	}
	f.tx.mu.Lock()
	claimed, retained := f.tx.delivered, f.tx.ready
	f.tx.mu.Unlock()
	if claimed || !retained || response.status != http.StatusOK || !errors.Is(response.err, io.EOF) {
		t.Fatalf("canceled pull claimed or wrote the assignment: claimed=%v retained=%v response=%+v", claimed, retained, response)
	}
	f.pull(t)
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatalf("live pull lost the unclaimed assignment: %v", got.err)
	}
}

func TestHTTPExecutorChecksCancellationBeforeAssignmentWork(t *testing.T) {
	job := testJob(command.Comment)
	job.Description = strings.Repeat("d", workspace.MaxBytes-len(job.Title)-len(job.Notes))
	if err := job.Validate(); err != nil || httpJobBound(job) <= httpJobBytes {
		t.Fatalf("fixture must be a valid Job above the conservative wire bound: %v", err)
	}
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			}
			cancel()
			executor, err := NewHTTPExecutor(job.Identity.ExecutorID)
			if err != nil {
				t.Fatal(err)
			}
			defer executor.Close()
			result, err := executor.Execute(ctx, job, runnerExecutionCheckpoints{})
			if !errors.Is(err, ctx.Err()) || errors.Is(err, monitor.ErrTerminalJob) != deadline || errors.Is(err, errHTTPBodyLimit) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !result.matchesJob(job) {
				t.Fatalf("canceled work reached assignment preparation or changed classification: result=%+v error=%v", result, err)
			}
			if executor.transaction() != nil || executor.unavailable {
				t.Fatal("unclaimed canceled work retained or quarantined the active slot")
			}
		})
	}
}

func TestHTTPExecutorFailedOutcomeAcrossMissingCallbackPreservesOnlyRawFacts(t *testing.T) {
	var after atomic.Int32
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{after: func(CallCheckpoint) error { after.Add(1); return nil }}, nil)
	f.pull(t)
	f.ack(t, "/before", f.call(1), http.StatusOK, 1)
	// Sequence 2 was the returned paid receipt, lost before reaching this handler.
	outcome := f.outcome(3, nil)
	outcome.Result.Calls = []CallUsage{{Provider: "codex", Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 17, Output: 4}, Complete: true}}
	f.ack(t, "/outcome", outcome, http.StatusConflict, 3)
	outcome.Failure = newHTTPFailure(errors.New("paid receipt transport failed"))
	f.ack(t, "/outcome", outcome, http.StatusOK, 3)
	got := f.result(t)
	if !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !reflect.DeepEqual(got.result, outcome.Result) || after.Load() != 0 {
		t.Fatalf("raw failed outcome lost facts or acknowledged an unseen receipt: result=%+v error=%v after=%d", got.result, got.err, after.Load())
	}
	f.executor.mu.Lock()
	unavailable := f.executor.unavailable
	f.executor.mu.Unlock()
	if !unavailable {
		t.Fatal("a failed outcome across an unacknowledged checkpoint gap released capacity")
	}
}

func TestHTTPExecutorTreatsWorkerFailurePolicyAsUntrusted(t *testing.T) {
	workerFailure := func() *httpFailure {
		return &httpFailure{Message: "failed\nFORGED JOURNAL ENTRY\x1b[2J", Terminal: true,
			Canceled: true, Deadline: true, Unsafe: true, OutcomeUnknown: true, RequestTransport: true,
			MissingExecutable: true, NoSpace: true, QuotaExceeded: true, OutputLimit: true}
	}
	assertWorkerFailure := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("worker failure became success")
		}
		for _, cause := range []error{monitor.ErrTerminalJob, context.Canceled, context.DeadlineExceeded,
			workroot.ErrUnsafe, monitor.ErrExecutionOutcomeUnknown,
			monitor.ErrRequestTransport, exec.ErrNotFound, syscall.ENOSPC, syscall.EDQUOT,
			aicli.ErrOutputLimit} {
			if errors.Is(err, cause) {
				t.Fatalf("worker claim acquired parent policy authority for %v: %v", cause, err)
			}
		}
		var journal bytes.Buffer
		log.New(&journal, "", 0).Printf("worker failure: %v", err)
		line := journal.String()
		var private *workerFailureError
		if !errors.As(err, &private) || private.failure.Message != "failed\nFORGED JOURNAL ENTRY\x1b[2J" {
			t.Fatalf("bounded private worker failure was lost: %v", err)
		}
		if strings.Count(line, "\n") != 1 || strings.ContainsAny(line, "\r\x1b") ||
			strings.Contains(line, "FORGED JOURNAL ENTRY") {
			t.Fatalf("worker diagnostic reached the log line: %q", line)
		}
	}

	for _, paid := range []bool{false, true} {
		t.Run(fmt.Sprintf("paid=%t", paid), func(t *testing.T) {
			var returned atomic.Int32
			f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{
				after: func(CallCheckpoint) error { returned.Add(1); return nil },
			}, nil)
			f.pull(t)
			outcome := f.outcome(1, nil)
			if paid {
				f.ack(t, "/before", f.call(1), http.StatusOK, 1)
				receipt := f.call(2)
				receipt.Checkpoint.Call.Tokens = usage.Tokens{Input: 17, Output: 4}
				receipt.Checkpoint.Call.Complete = true
				f.ack(t, "/after", receipt, http.StatusOK, 2)
				outcome.Sequence = 3
				outcome.Result.Calls = []CallUsage{receipt.Checkpoint.Call}
			}
			outcome.Failure = workerFailure()
			f.ack(t, "/outcome", outcome, http.StatusOK, outcome.Sequence)
			got := f.result(t)
			assertWorkerFailure(t, got.err)
			if !reflect.DeepEqual(got.result.Calls, outcome.Result.Calls) || returned.Load() != map[bool]int32{false: 0, true: 1}[paid] {
				t.Fatalf("worker failure lost acknowledged paid facts: result=%+v callbacks=%d", got.result, returned.Load())
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := f.executor.Execute(ctx, f.job, runnerExecutionCheckpoints{}); !errors.Is(err, context.Canceled) {
				t.Fatalf("worker claims reserved the next assignment slot: %v", err)
			}
		})
	}

	t.Run("parent checkpoint", func(t *testing.T) {
		parentErr := fmt.Errorf("parent save: %w", workroot.ErrUnsafe)
		f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{
			before: func(CallCheckpoint) error { return parentErr },
		}, nil)
		f.pull(t)
		f.ack(t, "/before", f.call(1), http.StatusInternalServerError, 1)
		outcome := f.outcome(2, nil)
		outcome.Failure = workerFailure()
		f.ack(t, "/outcome", outcome, http.StatusOK, 2)
		if got := f.result(t); !errors.Is(got.err, parentErr) || !errors.Is(got.err, workroot.ErrUnsafe) {
			t.Fatalf("parent checkpoint lost its typed failure: %v", got.err)
		}
	})

	t.Run("parent budget", func(t *testing.T) {
		f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
		f.pull(t)
		f.tx.mu.Lock()
		f.tx.budget = httpPendingDeadlineContext{Context: f.tx.budget, deadline: time.Now().Add(-time.Second)}
		f.tx.mu.Unlock()
		outcome := f.outcome(1, nil)
		outcome.Failure = workerFailure()
		f.ack(t, "/outcome", outcome, http.StatusOK, 1)
		got := f.result(t)
		if !errors.Is(got.err, context.DeadlineExceeded) || !errors.Is(got.err, monitor.ErrTerminalJob) {
			t.Fatalf("parent budget lost its typed failure: %v", got.err)
		}
	})
}

func TestHTTPExecutorStoreRefusalRemainsFinalIfWorkerContinues(t *testing.T) {
	for _, refused := range []string{"before", "after", "review"} {
		t.Run(refused, func(t *testing.T) {
			storeErr := errors.New("actual parent store refusal")
			var before, after, review atomic.Int32
			sink := runnerExecutionCheckpoints{
				before: func(CallCheckpoint) error {
					before.Add(1)
					if refused == "before" {
						return storeErr
					}
					return nil
				},
				after: func(CallCheckpoint) error {
					after.Add(1)
					if refused == "after" {
						return storeErr
					}
					return nil
				},
				review: func(ReviewCheckpoint) error {
					review.Add(1)
					if refused == "review" {
						return storeErr
					}
					return nil
				},
			}
			f := newHTTPServerTest(t, t.Context(), withClaudeCritic(testJob(command.Comment)), sink, nil)
			f.pull(t)
			sequence := uint64(1)
			if refused == "after" {
				f.ack(t, "/before", f.call(sequence), http.StatusOK, sequence)
				sequence++
			}
			checkpoint := ReviewCheckpoint{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head}
			var failed any = f.call(sequence)
			if refused == "review" {
				failed = httpReview{Sequence: sequence, Checkpoint: checkpoint}
			}
			ack := f.ack(t, "/"+refused, failed, http.StatusInternalServerError, sequence)
			if ack.Failure == nil || ack.Failure.Message != storeErr.Error() {
				t.Fatalf("store refusal lost its actual cause: %+v", ack)
			}
			counts := []int32{before.Load(), after.Load(), review.Load()}
			for _, kind := range []string{"before", "after", "review"} {
				sequence++
				var value any = f.call(sequence)
				if kind == "review" {
					value = httpReview{Sequence: sequence, Checkpoint: checkpoint}
				}
				ack := f.ack(t, "/"+kind, value, http.StatusConflict, sequence)
				if ack.Failure == nil || ack.Failure.Message != storeErr.Error() {
					t.Fatalf("continued callback replaced original refusal: %+v", ack)
				}
			}
			sequence++
			// A faulty worker's successful claim cannot override a parent-owned failed save.
			outcome := f.outcome(sequence, nil)
			outcome.Result.Answer = "Unaccepted worker claim"
			f.ack(t, "/outcome", outcome, http.StatusOK, sequence)
			got := f.result(t)
			if !errors.Is(got.err, storeErr) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !reflect.DeepEqual(got.result, outcome.Result) || !reflect.DeepEqual(counts, []int32{before.Load(), after.Load(), review.Load()}) {
				t.Fatalf("failed save was forgotten or later callbacks reached sink: counts=%v now=%v result=%+v error=%v", counts, []int32{before.Load(), after.Load(), review.Load()}, got.result, got.err)
			}
		})
	}
}

func TestHTTPExecutorStoreRefusalWakesWatchAfterACK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const stopBudget = 10 * time.Millisecond
		storeErr := errors.New("parent checkpoint save refused")
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		f := newHTTPServerTestWithStopServer(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{
			before: func(CallCheckpoint) error { return storeErr },
		}, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/before" {
					w = httpServerHeldWrite{ResponseWriter: w, entered: entered, release: release}
				}
				next.ServeHTTP(w, r)
			})
		}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, stopBudget)
		f.pull(t)
		body, err := json.Marshal(f.tx.binding)
		if err != nil {
			t.Fatal(err)
		}
		watch, err := f.server.Client().Post(f.server.URL+"/cancel", "application/json", bytes.NewReader(body))
		if err != nil || watch.StatusCode != http.StatusOK {
			t.Fatalf("watch admission: response=%v error=%v", watch, err)
		}
		defer watch.Body.Close()
		watchDone := make(chan struct {
			cancellation httpCancellation
			err          error
		}, 1)
		go func() {
			var cancellation httpCancellation
			err := httpDecode(watch.Body, httpControlBytes, &cancellation)
			watchDone <- struct {
				cancellation httpCancellation
				err          error
			}{cancellation, err}
		}()
		ackDone := make(chan httpServerTestExchange, 1)
		go func() { ackDone <- f.request("/before", f.call(1)) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("failed checkpoint ACK did not enter its write")
		}
		<-f.tx.stop
		stopUntil := f.tx.controlDeadline()
		time.Sleep(2 * time.Millisecond)
		select {
		case got := <-watchDone:
			t.Fatalf("watch preempted failed checkpoint ACK: %+v", got)
		default:
		}
		unblock()
		ack := <-ackDone
		if ack.err != nil || ack.status != http.StatusInternalServerError || ack.ack.Failure == nil || ack.ack.Failure.Message != storeErr.Error() {
			t.Fatalf("failed checkpoint ACK lost store refusal: %+v", ack)
		}
		gotWatch := <-watchDone
		if gotWatch.err != nil || gotWatch.cancellation.Reason != "canceled" || gotWatch.cancellation.StopRemaining <= 0 || gotWatch.cancellation.StopRemaining >= stopBudget {
			t.Fatalf("watch missed bounded stop after failed ACK: %+v", gotWatch)
		}
		if later := f.tx.controlDeadline(); !later.Equal(stopUntil) {
			t.Fatalf("watch renewed stop deadline: first=%s later=%s", stopUntil, later)
		}
		got := f.result(t)
		if time.Now().After(stopUntil.Add(2*time.Millisecond)) || !errors.Is(got.err, storeErr) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("failed save outlived fixed stop: result=%+v error=%v deadline=%s", got.result, got.err, stopUntil)
		}
	})
}

func TestHTTPExecutorStatusIsIndependentAndAllowsObservationGaps(t *testing.T) {
	var before atomic.Int32
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{before: func(CallCheckpoint) error { before.Add(1); return nil }}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/status" {
				w = httpServerFailWrite{w}
			}
			next.ServeHTTP(w, r)
		})
	})
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 2, WorkerState: "busy", Phase: "prepare"}
	if got := f.request("/status", httpObservation{Status: status}); got.err == nil {
		t.Fatal("injected observation ACK failure unexpectedly succeeded")
	}
	if got, receivedAt, ok := f.executor.Status(); !ok || got != status || receivedAt.IsZero() || f.tx.ctx.Err() != nil {
		t.Fatalf("lost observation 1 or ACK changed action admission: status=%+v received=%v present=%t ctx=%v", got, receivedAt, ok, f.tx.ctx.Err())
	}
	f.ack(t, "/before", f.call(1), http.StatusOK, 1)
	f.ack(t, "/outcome", f.outcome(2, nil), http.StatusOK, 2)
	if got := f.result(t); got.err != nil || before.Load() != 1 {
		t.Fatalf("status failure altered required checkpoint/outcome stream: error=%v before=%d", got.err, before.Load())
	}
	f.executor.mu.Lock()
	unavailable := f.executor.unavailable
	f.executor.mu.Unlock()
	if unavailable {
		t.Fatal("observation ACK failure quarantined a conclusive transaction")
	}
}

func TestHTTPExecutorStatusReceiptRevivesWithoutInventingProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		f := newHTTPServerTestInMemory(t, t.Context(), job, runnerExecutionCheckpoints{})
		f.pull(t)
		first := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
			Sequence: 1, ProgressSequence: 1, WorkerState: "busy", Phase: "prepare"}
		f.ack(t, "/status", httpObservation{Status: first}, http.StatusOK, 1)
		got, receivedAt, ok := f.executor.Status()
		if !ok || got != first || receivedAt.IsZero() {
			t.Fatalf("first receipt: status=%+v received=%v present=%t", got, receivedAt, ok)
		}
		time.Sleep(20 * time.Second)
		if age := time.Since(receivedAt); age != 20*time.Second {
			t.Fatalf("stale contact age = %s", age)
		}
		second := first
		second.Sequence = 2
		second.ProgressAge = 20 * time.Second
		f.ack(t, "/status", httpObservation{Status: second}, http.StatusOK, 2)
		got, revivedAt, ok := f.executor.Status()
		if !ok || got != second || !revivedAt.After(receivedAt) || time.Since(revivedAt) != 0 || got.ProgressSequence != first.ProgressSequence {
			t.Fatalf("heartbeat contact and progress conflated: status=%+v received=%v present=%t", got, revivedAt, ok)
		}
		f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
		if got := f.result(t); got.err != nil {
			t.Fatal(got.err)
		}
	})
}

func TestHTTPExecutorRejectsStaleAndRegressingStatusWithoutChangingPaidSequence(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(30*time.Second))
	defer cancel()
	deadline, _ := ctx.Deadline()
	job := testJob(command.Comment)
	job.Critic = &RoleSettings{Provider: "claude"}
	job.CriticMaxRounds = 2
	f := newHTTPServerTestWithRoot(t, ctx, job, runnerExecutionCheckpoints{}, t.TempDir())
	f.pull(t)
	f.tx.mu.Lock()
	f.tx.started = f.tx.started.Add(-time.Second) // Keep synthetic nanosecond ages below measured elapsed time on Windows.
	f.tx.mu.Unlock()
	status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head,
		Sequence: 3, ProgressSequence: 4, ProgressAge: 2 * time.Nanosecond, LogSequence: 3, WorkerState: "busy", Phase: "prepare"}
	record := func(sequence uint64) attemptRecord {
		return attemptRecord{Identity: job.Identity, SourceRef: job.Source.Head, Sequence: sequence, Kind: attemptRecordDiscussion, Text: "observed"}
	}
	for sequence := uint64(1); sequence <= 2; sequence++ {
		prior := status
		prior.Sequence, prior.LogSequence, prior.ProgressAge = sequence, sequence, 0
		f.ack(t, "/status", httpObservation{Status: prior, Logs: []attemptRecord{record(sequence)}}, http.StatusOK, sequence)
	}
	f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{record(3)}}, http.StatusOK, 3)
	reject := func(name string, edit func(*Status)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			_, receivedAt, _ := f.executor.Status()
			candidate := status
			candidate.Sequence++
			edit(&candidate)
			ack := f.ack(t, "/status", httpObservation{Status: candidate}, http.StatusConflict, candidate.Sequence)
			if got, gotAt, ok := f.executor.Status(); ack.Failure == nil || !ok || got != status || !gotAt.Equal(receivedAt) || f.tx.ctx.Err() != nil {
				t.Fatalf("rejected observation changed accepted status, contact or action: ack=%+v status=%+v received=%v present=%t ctx=%v", ack, got, gotAt, ok, f.tx.ctx.Err())
			}
		})
	}
	reject("replay", func(s *Status) { s.Sequence = status.Sequence })
	reject("stale", func(s *Status) { s.Sequence = status.Sequence - 1 })
	reject("foreign attempt", func(s *Status) { s.Identity.AttemptID = "foreign" })
	reject("impossible progress age", func(s *Status) { s.ProgressAge = time.Hour })
	reject("critic before call", func(s *Status) { s.Phase, s.Round, s.ReviewPhase = "critic", 1, reviewFull })
	reject("upload before completion", func(s *Status) { s.Phase = "upload" })
	var paidSequence uint64
	var paidCalls []CallUsage
	checkpoint := func(kind, role string, round int, phase string) {
		t.Helper()
		paidSequence++
		call := f.call(paidSequence)
		call.Checkpoint.Call.Role, call.Checkpoint.Call.Round, call.Checkpoint.Call.ReviewPhase = role, round, phase
		if role == roleCritic {
			call.Checkpoint.Call.Provider = f.job.Critic.Provider
		}
		f.ack(t, "/"+kind, call, http.StatusOK, paidSequence)
		if kind == "after" {
			paidCalls = append(paidCalls, call.Checkpoint.Call)
		}
	}
	checkpoint("before", roleMain, 1, "")
	checkpoint("after", roleMain, 1, "")
	checkpoint("before", roleCritic, 1, reviewFull)
	status.Sequence, status.Phase, status.Round, status.ReviewPhase = 5, "critic", 1, reviewFull
	status.ProgressSequence, status.ProgressAge, status.LogSequence = 5, time.Nanosecond, 4
	f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{record(4)}}, http.StatusOK, 5)
	reject("phase rollback", func(s *Status) { s.Phase, s.Round, s.ReviewPhase = "main", 1, "" })
	reject("progress rollback", func(s *Status) { s.ProgressSequence-- })
	reject("log rollback", func(s *Status) { s.LogSequence-- })
	reject("progress age rollback", func(s *Status) { s.ProgressAge-- })
	status.Sequence, status.ProgressAge, status.LogSequence = 6, 2*time.Nanosecond, 5
	f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{record(5)}}, http.StatusOK, 6)
	checkpoint("after", roleCritic, 1, reviewFull)
	checkpoint("before", roleMain, 2, "")
	status.Sequence, status.Phase, status.Round, status.ReviewPhase = 7, "main", 2, ""
	status.ProgressSequence, status.ProgressAge = 6, 0
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 7)
	reject("round rollback", func(s *Status) { s.Round = 1 })
	checkpoint("after", roleMain, 2, "")
	checkpoint("before", roleCritic, 2, reviewVerification)
	status.Sequence, status.Phase, status.ReviewPhase = 8, "critic", reviewVerification
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 8)
	checkpoint("after", roleCritic, 2, reviewVerification)
	checkpoint("before", roleCritic, 2, reviewFull)
	status.Sequence, status.ReviewPhase = 9, reviewFull
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 9)
	reject("review phase rollback", func(s *Status) { s.ReviewPhase = reviewVerification })
	checkpoint("after", roleCritic, 2, reviewFull)
	paidSequence++
	f.ack(t, "/review", httpReview{Sequence: paidSequence, Checkpoint: ReviewCheckpoint{
		Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head,
		Calls: paidCalls, CriticStopReason: "round_cap",
	}}, http.StatusOK, paidSequence)
	status.Sequence, status.Phase, status.Round, status.ReviewPhase = 10, "cleanup", 0, ""
	status.WorkerState = "stopping"
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 10)
	reject("worker state rollback", func(s *Status) { s.WorkerState = "busy" })
	status.Sequence, status.WorkerState = 11, "ready"
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 11)
	reject("terminal state rollback", func(s *Status) { s.WorkerState = "stopping" })
	reject("terminal state changes", func(s *Status) { s.WorkerState = "unclean" })
	if got, ok := f.tx.ctx.Deadline(); !ok || !got.Equal(deadline) {
		t.Fatalf("observations changed parent deadline: got=%v present=%t want=%v", got, ok, deadline)
	}
	f.ack(t, "/outcome", f.outcome(paidSequence+1, nil), http.StatusOK, paidSequence+1)
	if got := f.result(t); got.err != nil {
		t.Fatalf("observations changed paid sequence or outcome: %v", got.err)
	}
	f.executor.mu.Lock()
	active, unavailable := f.executor.active, f.executor.unavailable
	f.executor.mu.Unlock()
	if got, _, ok := f.executor.Status(); active != nil || unavailable || !ok || got != status {
		t.Fatalf("observations changed capacity or recoverable status: active=%v unavailable=%t status=%+v present=%t", active, unavailable, got, ok)
	}
}

func TestHTTPExecutorEarlyCriticDecisionDoesNotUnlockLaterStatusPhases(t *testing.T) {
	job := testJob(command.Comment)
	job.Critic = &RoleSettings{Provider: "claude"}
	job.CriticMaxRounds = 2
	f := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil)
	f.pull(t)
	main := f.call(1)
	f.ack(t, "/before", main, http.StatusOK, 1)
	main.Sequence = 2
	f.ack(t, "/after", main, http.StatusOK, 2)
	critic := f.call(3)
	critic.Checkpoint.Call.Provider, critic.Checkpoint.Call.Role, critic.Checkpoint.Call.ReviewPhase = job.Critic.Provider, roleCritic, reviewFull
	f.ack(t, "/before", critic, http.StatusOK, 3)
	critic.Sequence = 4
	critic.Checkpoint.Call.Approved, critic.Checkpoint.Call.NoBlockers, critic.Checkpoint.Call.ReviewComplete = true, true, true
	f.ack(t, "/after", critic, http.StatusOK, 4)
	f.ack(t, "/review", httpReview{Sequence: 5, Checkpoint: ReviewCheckpoint{
		Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head,
		Calls: []CallUsage{main.Checkpoint.Call, critic.Checkpoint.Call}, CriticStopReason: "approved",
	}}, http.StatusOK, 5)
	status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head,
		Sequence: 1, WorkerState: "busy", Phase: "main", Round: 2}
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusConflict, 1)
	status.Sequence, status.Phase, status.Round = 2, "upload", 0
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 2)
	f.ack(t, "/outcome", f.outcome(6, nil), http.StatusOK, 6)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPExecutorBoundsRejectedStatusBinding(t *testing.T) {
	for _, malformed := range []string{"shape", "unknown field", "trailing value"} {
		t.Run(malformed, func(t *testing.T) {
			f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
			f.pull(t)
			status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "prepare"}
			if malformed == "shape" {
				status.Identity.JobID = strings.Repeat("<", 30_000)
			}
			var buffer bytes.Buffer
			encoder := json.NewEncoder(&buffer)
			encoder.SetEscapeHTML(false) // Small valid input can expand beyond the ACK cap when echoed.
			if err := encoder.Encode(httpObservation{Status: status}); err != nil {
				t.Fatal(err)
			}
			body := buffer.Bytes()
			if malformed == "unknown field" {
				body = bytes.Replace(body, []byte(`"status":{`), []byte(`"status":{"unknown":true,`), 1)
			} else if malformed == "trailing value" {
				body = append(body, []byte(`{}`)...)
			}
			if len(body) >= httpControlBytes {
				t.Fatal("fixture must reach shape/decode refusal rather than the encoded input cap")
			}
			got := f.raw("/status", bytes.NewReader(body))
			if got.err != nil || got.status != http.StatusBadRequest || got.ack.Kind != "status" || got.ack.Sequence != status.Sequence || got.ack.Failure == nil || got.ack.httpBinding != (httpBinding{}) {
				t.Fatalf("malformed Status caused an unbounded or successful reply: status=%d ack=%+v error=%v", got.status, got.ack, got.err)
			}
			if _, _, ok := f.executor.Status(); ok {
				t.Fatal("rejected Status became an accepted observation")
			}
			f.ack(t, "/before", f.call(1), http.StatusOK, 1)
			f.ack(t, "/outcome", f.outcome(2, nil), http.StatusOK, 2)
			if got := f.result(t); got.err != nil {
				t.Fatal(got.err)
			}
		})
	}
}

func TestHTTPExecutorCanceledBeforeConsumesOnlyBoundSequence(t *testing.T) {
	var before atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
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
	status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "cleanup"}
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 1)
	outcome := f.outcome(2, context.Canceled)
	f.ack(t, "/outcome", outcome, http.StatusOK, 2)
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
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{after: func(value CallCheckpoint) error {
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
	blocked := make(chan httpServerTestExchange, 1)
	go func() { blocked <- f.request("/review", httpReview{Sequence: 2}) }()
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
	case reply := <-blocked:
		if reply.err != nil || reply.status != http.StatusConflict {
			t.Fatalf("fenced concurrent mutation: %+v", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fenced concurrent mutation did not drain")
	}
	select {
	case ack := <-acknowledged:
		if ack.err != nil || ack.status != http.StatusInternalServerError || ack.ack.Sequence != 1 || ack.ack.Failure == nil || ack.ack.Failure.Message != storeErr.Error() {
			t.Fatalf("entered After lost its acknowledgement: %+v", ack)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("entered After did not drain")
	}
	got := f.result(t)
	if !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(got.err, storeErr) || !got.result.matchesJob(f.job) || len(got.result.Calls) != 0 || saved != call.Checkpoint.Call {
		t.Fatalf("drain invented or lost paid evidence: result=%+v saved=%+v error=%v", got.result, saved, got.err)
	}
	if late := f.request("/after", f.call(2)); late.err != nil || late.status != http.StatusConflict {
		t.Fatalf("late callback reached finalized adapter: %+v", late)
	}
	if _, err := f.executor.Execute(t.Context(), f.job, runnerExecutionCheckpoints{}); err == nil {
		t.Fatal("unknown outcome released executor capacity")
	}
}

type httpServerFailWrite struct{ http.ResponseWriter }

func (w httpServerFailWrite) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w httpServerFailWrite) Write([]byte) (int, error)   { return 0, io.ErrClosedPipe }

func TestHTTPExecutorRequiredReplyFailureCannotBecomeSuccess(t *testing.T) {
	for _, path := range []string{"/next", "/before", "/after", "/review"} {
		t.Run(path, func(t *testing.T) {
			job := testJob(command.Comment)
			if path == "/review" {
				job = withClaudeCritic(job)
			}
			owner := newExecutionAdapterFixture(t, job)
			store := owner.startUsage()
			var failReply atomic.Bool
			failReply.Store(path != "/review")
			f := newHTTPServerTest(t, t.Context(), owner.job, owner.sink, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == path && failReply.Load() {
						w = httpServerFailWrite{w}
					}
					next.ServeHTTP(w, r)
				})
			})
			outcome := f.outcome(1, nil)
			switch path {
			case "/next":
				if reply := f.request(path, httpPull{Version: ProtocolVersion, ExecutorID: owner.job.Identity.ExecutorID}); reply.err == nil {
					t.Fatalf("assignment delivery unexpectedly succeeded: %+v", reply)
				}
			case "/before", "/after":
				f.pull(t)
				before := f.call(1)
				if path == "/before" {
					if reply := f.request(path, before); reply.err == nil {
						t.Fatalf("call admission acknowledgement unexpectedly succeeded: %+v", reply)
					}
				} else {
					f.ack(t, "/before", before, http.StatusOK, 1)
				}
				paid := f.call(2)
				paid.Checkpoint.Call.Tokens, paid.Checkpoint.Call.Complete = usage.Tokens{Input: 9, Output: 2}, true
				if path == "/before" {
					f.ack(t, "/after", paid, http.StatusOK, 2)
				} else if reply := f.request(path, paid); reply.err == nil {
					t.Fatalf("paid callback acknowledgement unexpectedly succeeded: %+v", reply)
				}
				outcome.Sequence = 3
				outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
			case "/review":
				f.pull(t)
				f.ack(t, "/review", httpReview{Sequence: 1, Checkpoint: owner.review(nil, nil, "")}, http.StatusOK, 1)
				f.ack(t, "/before", f.call(2), http.StatusOK, 2)
				main := f.call(3)
				main.Checkpoint.Call.Tokens, main.Checkpoint.Call.Complete = usage.Tokens{Input: 9, Output: 2}, true
				f.ack(t, "/after", main, http.StatusOK, 3)
				f.ack(t, "/review", httpReview{Sequence: 4, Checkpoint: owner.review([]CallUsage{main.Checkpoint.Call}, nil, "")}, http.StatusOK, 4)
				critic := f.call(5)
				critic.Checkpoint.Call = CallUsage{Provider: owner.job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull}
				f.ack(t, "/before", critic, http.StatusOK, 5)
				critic.Sequence = 6
				critic.Checkpoint.Call.Tokens, critic.Checkpoint.Call.Complete = usage.Tokens{Input: 4, Output: 1}, true
				critic.Checkpoint.Call.Approved, critic.Checkpoint.Call.NoBlockers, critic.Checkpoint.Call.ReviewComplete = true, true, true
				f.ack(t, "/after", critic, http.StatusOK, 6)
				calls := []CallUsage{main.Checkpoint.Call, critic.Checkpoint.Call}
				f.ack(t, "/review", httpReview{Sequence: 7, Checkpoint: owner.review(calls, nil, "")}, http.StatusOK, 7)
				failReply.Store(true)
				if reply := f.request(path, httpReview{Sequence: 8, Checkpoint: owner.review(calls, nil, "approved")}); reply.err == nil {
					t.Fatalf("decision acknowledgement unexpectedly succeeded: %+v", reply)
				}
				if owner.sink.review.CriticStopReason != "approved" {
					t.Fatal("decision checkpoint was not saved before its reply failed")
				}
				outcome.Sequence, outcome.Result.Calls, outcome.Result.CriticStopReason = 9, calls, "approved"
			}
			outcome.Result.Answer = "Forged success after failed required reply"
			if path != "/next" {
				if err := outcome.Result.Validate(owner.job); err != nil {
					t.Fatalf("success claim is not otherwise publishable: %v", err)
				}
			}
			f.ack(t, "/outcome", outcome, http.StatusOK, outcome.Sequence)
			got := f.result(t)
			if !reflect.DeepEqual(got.result, outcome.Result) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(owner.sink.finish(got.result, got.err), monitor.ErrExecutionOutcomeUnknown) {
				t.Fatalf("failed required reply became publishable: result=%+v error=%v", got.result, got.err)
			}
			records, err := usage.ReadAll(store.Dir)
			wantCodex, wantClaude := 0, 0
			wantTokens := usage.Tokens{}
			if path != "/next" {
				wantCodex = 1
				wantTokens = usage.Tokens{Input: 9, Output: 2}
			}
			if path == "/review" {
				wantClaude = 1
				wantTokens = usage.Tokens{Input: 13, Output: 3}
			}
			if err != nil || len(records) != 1 || records[0].CodexCalls != wantCodex || records[0].ClaudeCalls != wantClaude || records[0].Tokens != wantTokens {
				t.Fatalf("required reply failure lost saved usage: records=%+v error=%v", records, err)
			}
			if _, err := f.executor.Execute(t.Context(), f.job, owner.sink); err == nil {
				t.Fatal("failed required reply released worker capacity")
			}
		})
	}
}

func TestHTTPExecutorRefusedOutcomeReleasesCancellationWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newHTTPServerTestInMemory(t, t.Context(), testJob(command.Comment), refusingOutcomeCheckpoints{})
		f.pull(t)
		body, err := json.Marshal(f.tx.binding)
		if err != nil {
			t.Fatal(err)
		}
		watch, err := f.server.Client().Post(f.server.URL+"/cancel", "application/json", bytes.NewReader(body))
		if err != nil || watch.StatusCode != http.StatusOK {
			t.Fatalf("cancellation watch admission: response=%v error=%v", watch, err)
		}
		defer watch.Body.Close()
		ack := f.ack(t, "/outcome", f.outcome(1, nil), http.StatusInternalServerError, 1)
		if ack.Failure == nil {
			t.Fatal("refused outcome was acknowledged as success")
		}
		if got := f.result(t); !errors.Is(got.err, monitor.ErrTerminalJob) {
			t.Fatalf("refused outcome did not finish promptly: %v", got.err)
		}
		var cancellation httpCancellation
		if err := httpDecode(watch.Body, httpControlBytes, &cancellation); err != nil || cancellation.Reason != "completed" {
			t.Fatalf("cancellation watch did not finish: %+v error=%v", cancellation, err)
		}
	})
}

func TestHTTPExecutorReceivedOutcomeSurvivesLostReceiptACK(t *testing.T) {
	for _, runErr := range []error{nil, errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded)} {
		t.Run(fmt.Sprint(runErr), func(t *testing.T) {
			f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
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
			if !reflect.DeepEqual(got.result, outcome.Result) || (got.err == nil) != (runErr == nil) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || errors.Is(got.err, monitor.ErrTerminalJob) || errors.Is(got.err, context.DeadlineExceeded) {
				t.Fatalf("lost receipt erased or changed conclusive outcome: %+v error=%v", got.result, got.err)
			}
			if _, err := f.executor.Execute(t.Context(), f.job, runnerExecutionCheckpoints{}); err == nil {
				t.Fatal("lost receipt ACK released quarantined capacity")
			}
		})
	}
}

func TestHTTPExecutorLateKnownOutcomeRetainsFactsAndActualContextCause(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%v", deadline), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var ctx context.Context
				var cancel context.CancelFunc
				if deadline {
					ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				} else {
					ctx, cancel = context.WithCancel(t.Context())
				}
				defer cancel()
				f := newHTTPServerTestInMemory(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{})
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
		})
	}
}

// Simulate the interval after a deadline but before its cancellation callback runs.
type httpPendingDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c httpPendingDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestHTTPExecutionContextErrorPreservesPriorCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ctx = httpPendingDeadlineContext{Context: ctx, deadline: time.Now().Add(-time.Second)}
	if err := httpExecutionContextError(ctx); !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("a later deadline replaced the original cancellation: %v", err)
	}
}

func TestHTTPExecutorChecksDeadlineBeforeCancellationNotification(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprintf("before=%t", before), func(t *testing.T) {
			owner := newExecutionAdapterFixture(t, testJob(command.Comment))
			store := owner.startUsage()
			f := newHTTPServerTest(t, t.Context(), owner.job, owner.sink, nil)
			f.pull(t)
			sequence := uint64(1)
			var calls []CallUsage
			tokens := usage.Tokens{Input: 17, CachedInput: 3, Output: 5}
			if !before {
				f.ack(t, "/before", f.call(sequence), http.StatusOK, sequence)
				sequence++
				paid := f.call(sequence)
				paid.Checkpoint.Call.Tokens, paid.Checkpoint.Call.Complete = tokens, true
				f.ack(t, "/after", paid, http.StatusOK, sequence)
				calls = []CallUsage{paid.Checkpoint.Call}
				sequence++
			}
			f.tx.mu.Lock()
			deadline := time.Now().Add(-time.Second)
			f.tx.ctx = httpPendingDeadlineContext{Context: f.tx.ctx, deadline: deadline}
			f.tx.budget = httpPendingDeadlineContext{Context: f.tx.budget, deadline: deadline}
			err := f.tx.ctx.Err()
			f.tx.mu.Unlock()
			if err != nil {
				t.Fatalf("fixture already notified cancellation: %v", err)
			}
			if before {
				ack := f.ack(t, "/before", f.call(sequence), http.StatusConflict, sequence)
				if err := ack.Failure.err(); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) || owner.sink.pending != nil {
					t.Fatalf("expired call reached the sink or lost classification: failure=%v pending=%+v", err, owner.sink.pending)
				}
				sequence++
			}
			outcome := f.outcome(sequence, nil)
			outcome.Result.Calls = calls
			if !before {
				outcome.Result.Answer = "Completed paid result before late upload"
			}
			f.ack(t, "/outcome", outcome, http.StatusOK, sequence)
			got := f.result(t)
			finished := owner.sink.finish(got.result, got.err)
			if !errors.Is(finished, context.DeadlineExceeded) || !errors.Is(finished, monitor.ErrTerminalJob) || errors.Is(finished, monitor.ErrExecutionOutcomeUnknown) || !reflect.DeepEqual(got.result, outcome.Result) {
				t.Fatalf("expired receipt lost its cause or raw facts: result=%+v error=%v", got.result, finished)
			}
			records, err := usage.ReadAll(store.Dir)
			wantTokens, wantCalls := tokens, 1
			if before {
				wantTokens, wantCalls = usage.Tokens{}, 0
			}
			if err != nil || len(records) != 1 || records[0].Tokens != wantTokens || records[0].CodexCalls != wantCalls || !records[0].UsageComplete {
				t.Fatalf("deadline handling changed paid evidence: records=%+v error=%v", records, err)
			}
		})
	}
}

func TestHTTPExecutorKnownOutcomeResolvesWatchTransportCancellation(t *testing.T) {
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	f.pull(t)
	f.tx.stopExecution(io.ErrClosedPipe, true)
	<-f.tx.ctx.Done()
	outcome := f.outcome(1, nil)
	outcome.Result.Answer = "Conclusive result received within the accepted budget"
	f.ack(t, "/outcome", outcome, http.StatusOK, 1)
	if got := f.result(t); got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("watch transport failure poisoned a conclusive received result: %+v error=%v", got.result, got.err)
	}
	f.executor.mu.Lock()
	unavailable := f.executor.unavailable
	f.executor.mu.Unlock()
	if unavailable {
		t.Fatal("resolved transport ambiguity invented a quarantine fact")
	}
}

func TestHTTPExecutorRejectedBoundBeforeSurvivesWatchOutcome(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%t", foreign), func(t *testing.T) {
			owner := newExecutionAdapterFixture(t, testJob(command.Comment))
			store := owner.startUsage()
			f := newHTTPServerTest(t, t.Context(), owner.job, owner.sink, nil)
			f.pull(t)
			f.ack(t, "/before", f.call(1), http.StatusOK, 1)
			paid := f.call(2)
			paid.Checkpoint.Call.Tokens, paid.Checkpoint.Call.Complete = usage.Tokens{Input: 11, Output: 3}, true
			f.ack(t, "/after", paid, http.StatusOK, 2)
			f.tx.stopExecution(io.ErrClosedPipe, true)
			<-f.tx.ctx.Done()
			before := f.call(3)
			if foreign {
				before.Checkpoint.Identity.AttemptID += "-other"
			}
			f.ack(t, "/before", before, http.StatusConflict, 3)
			sequence := uint64(4)
			if foreign {
				sequence = 3
			}
			outcome := f.outcome(sequence, nil)
			outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
			outcome.Result.Answer = "Claimed complete answer"
			if err := outcome.Result.validate(owner.job, true); err != nil {
				t.Fatalf("fixture did not produce a publishable complete Result: %v", err)
			}
			f.ack(t, "/outcome", outcome, http.StatusOK, sequence)
			got := f.result(t)
			finalErr := owner.sink.finish(got.result, got.err)
			if !reflect.DeepEqual(got.result, outcome.Result) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) == foreign || errors.Is(finalErr, monitor.ErrExecutionOutcomeUnknown) == foreign || (got.err == nil) != foreign || (finalErr == nil) != foreign {
				t.Fatalf("bound refusal was erased or foreign request poisoned outcome: result=%+v error=%v finish=%v", got.result, got.err, finalErr)
			}
			records, err := usage.ReadAll(store.Dir)
			if err != nil || len(records) != 1 || records[0].CodexCalls != 1 || records[0].Tokens != paid.Checkpoint.Call.Tokens {
				t.Fatalf("saved paid receipt changed: records=%+v error=%v", records, err)
			}
			f.executor.mu.Lock()
			unavailable := f.executor.unavailable
			f.executor.mu.Unlock()
			if unavailable == foreign {
				t.Fatalf("worker capacity disagrees with bound refusal: unavailable=%t foreign=%t", unavailable, foreign)
			}
		})
	}
}

func TestHTTPExecutorConcurrentBoundBeforeSurvivesWatchOutcome(t *testing.T) {
	owner := newExecutionAdapterFixture(t, testJob(command.Comment))
	store := owner.startUsage()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	sink := runnerExecutionCheckpoints{
		before: owner.sink.BeforeCall,
		after: func(value CallCheckpoint) error {
			close(entered)
			<-release
			return owner.sink.AfterCall(value)
		},
		review: owner.sink.Review,
	}
	arrived := make(chan struct{})
	var beforeCount atomic.Int32
	f := newHTTPServerTest(t, t.Context(), owner.job, sink, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/before" && beforeCount.Add(1) == 2 {
				close(arrived)
			}
			next.ServeHTTP(w, r)
		})
	})
	f.pull(t)
	f.ack(t, "/before", f.call(1), http.StatusOK, 1)
	paid := f.call(2)
	paid.Checkpoint.Call.Tokens, paid.Checkpoint.Call.Complete = usage.Tokens{Input: 11, Output: 3}, true
	afterReply := make(chan httpServerTestExchange, 1)
	go func() { afterReply <- f.request("/after", paid) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("After did not enter its save")
	}
	f.tx.stopExecution(io.ErrClosedPipe, true)
	<-f.tx.ctx.Done()
	beforeReply := make(chan httpServerTestExchange, 1)
	go func() { beforeReply <- f.request("/before", f.call(3)) }()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Before did not reach the receiver")
	}
	// A concurrent callback must wait for the held save, not take the old busy-rejection path.
	select {
	case before := <-beforeReply:
		t.Fatalf("concurrent Before replied while After save was held: %+v", before)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	var before httpServerTestExchange
	select {
	case before = <-beforeReply:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Before did not finish after save")
	}
	if before.err != nil || before.status != http.StatusConflict {
		t.Fatalf("concurrent Before response: %+v", before)
	}
	select {
	case after := <-afterReply:
		if after.err != nil || after.status != http.StatusOK || after.ack.Sequence != 2 {
			t.Fatalf("saved After response: %+v", after)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("saved After did not finish")
	}
	f.tx.mu.Lock()
	sequence := f.tx.sequence + 1
	f.tx.mu.Unlock()
	outcome := f.outcome(sequence, nil)
	outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
	outcome.Result.Answer = "Claimed complete answer"
	if err := outcome.Result.validate(owner.job, true); err != nil {
		t.Fatalf("fixture did not produce a publishable Result: %v", err)
	}
	f.ack(t, "/outcome", outcome, http.StatusOK, sequence)
	got := f.result(t)
	if !reflect.DeepEqual(got.result, outcome.Result) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(owner.sink.finish(got.result, got.err), monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("concurrent required refusal became publishable: result=%+v error=%v", got.result, got.err)
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 || records[0].CodexCalls != 1 || records[0].Tokens != paid.Checkpoint.Call.Tokens {
		t.Fatalf("saved paid receipt changed: records=%+v error=%v", records, err)
	}
	f.executor.mu.Lock()
	unavailable := f.executor.unavailable
	f.executor.mu.Unlock()
	if !unavailable {
		t.Fatal("concurrent required refusal released worker capacity")
	}
}

func TestHTTPExecutorCloseKeepsWatchAndPaidReceiptGraceAvailable(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
	}{
		{name: "worker reports cancellation", failure: context.Canceled},
		{name: "worker reports success"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				paid := make(chan CallUsage, 1)
				f := newHTTPServerTestInMemory(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{after: func(value CallCheckpoint) error {
					paid <- value.Call
					return nil
				}})
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
				if err := httpDecode(response.Body, httpControlBytes, &cancellation); err != nil || cancellation.httpBinding != f.tx.binding || cancellation.Reason != "canceled" || cancellation.StopRemaining <= 0 || cancellation.StopRemaining > f.tx.stopGrace {
					t.Fatalf("Close did not deliver bound known cancellation: %+v error=%v", cancellation, err)
				}
				synctest.Wait()
				select {
				case err := <-closed:
					t.Fatalf("Close ended before receipt grace and outcome: %v", err)
				default:
				}
				call := f.call(1)
				call.Checkpoint.Call.Tokens, call.Checkpoint.Call.Complete = usage.Tokens{Input: 31, Output: 2}, true
				f.ack(t, "/after", call, http.StatusOK, 1)
				if got := <-paid; got != call.Checkpoint.Call {
					t.Fatalf("Close lost a paid receipt in grace: %+v", got)
				}
				outcome := f.outcome(2, test.failure)
				outcome.Result.Answer, outcome.Result.Calls = "Completed paid result", []CallUsage{call.Checkpoint.Call}
				f.ack(t, "/outcome", outcome, http.StatusOK, 2)
				if got := f.result(t); !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || errors.Is(got.err, monitor.ErrTerminalJob) || !reflect.DeepEqual(got.result, outcome.Result) {
					t.Fatalf("Close lost host cancellation or paid facts: result=%+v error=%v", got.result, got.err)
				}
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("Close did not join completed HTTP handlers")
				}
			})
		})
	}
}

type httpServerHeldWrite struct {
	http.ResponseWriter
	entered chan struct{}
	release chan struct{}
}

type httpDeadlineHeldWrite struct {
	http.ResponseWriter
	entered   chan struct{}
	deadlines chan time.Time
}

func (w *httpDeadlineHeldWrite) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *httpDeadlineHeldWrite) SetWriteDeadline(deadline time.Time) error {
	w.deadlines <- deadline
	return nil
}

func (w *httpDeadlineHeldWrite) Write([]byte) (int, error) {
	close(w.entered)
	deadline := <-w.deadlines
	for {
		if deadline.IsZero() {
			deadline = <-w.deadlines
			continue
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case deadline = <-w.deadlines:
			timer.Stop()
		case <-timer.C:
			return 0, context.DeadlineExceeded
		}
	}
}

func (w httpServerHeldWrite) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w httpServerHeldWrite) Write(body []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseWriter.Write(body)
}

func TestHTTPExecutorSealedOutcomeDoesNotAcquireDeadlineDuringACKDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		f := newHTTPServerTestWithServer(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/outcome" {
					w = httpServerHeldWrite{ResponseWriter: w, entered: entered, release: release}
				}
				next.ServeHTTP(w, r)
			})
		}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) })
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
		synctest.Wait()
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
	})
}

func TestHTTPExecutorCloseBoundsSealedOutcomeACK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const stopBudget = 10 * time.Millisecond
		entered := make(chan struct{})
		f := newHTTPServerTestWithStopServer(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{},
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/outcome" {
						w = &httpDeadlineHeldWrite{ResponseWriter: w, entered: entered, deadlines: make(chan time.Time, 4)}
					}
					next.ServeHTTP(w, r)
				})
			}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, stopBudget)
		f.pull(t)
		outcome := f.outcome(1, nil)
		outcome.Result.Answer = "Sealed before explicit Close"
		acknowledged := make(chan httpServerTestExchange, 1)
		go func() { acknowledged <- f.request("/outcome", outcome) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("outcome did not seal before ACK write")
		}
		f.tx.mu.Lock()
		sealed := f.tx.outcome
		f.tx.mu.Unlock()
		if !sealed {
			t.Fatal("fixture entered an outcome rejection before sealing")
		}
		closed := make(chan error, 1)
		stoppedAt := time.Now()
		go func() { closed <- f.executor.Close() }()
		<-f.tx.stop
		got := f.result(t)
		if elapsed := time.Since(stoppedAt); elapsed > 2*stopBudget {
			t.Fatalf("sealed ACK outlived explicit Close bound: %s", elapsed)
		}
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		if got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
			t.Fatalf("lost ACK erased sealed outcome: %+v error=%v", got.result, got.err)
		}
		if ack := <-acknowledged; ack.err == nil && ack.status == http.StatusOK && ack.ack.Failure == nil {
			t.Fatalf("blocked ACK was reported delivered: %+v", ack)
		}
		f.executor.mu.Lock()
		unavailable := f.executor.unavailable
		f.executor.mu.Unlock()
		if !unavailable {
			t.Fatal("lost ACK did not quarantine worker capacity")
		}
	})
}

func TestHTTPExecutorCallerCancellationBoundsSealedOutcomeACK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const stopBudget = 10 * time.Millisecond
		entered := make(chan struct{})
		f := newHTTPServerTestWithStopServer(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{},
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/outcome" {
						w = &httpDeadlineHeldWrite{ResponseWriter: w, entered: entered, deadlines: make(chan time.Time, 4)}
					}
					next.ServeHTTP(w, r)
				})
			}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, stopBudget)
		f.pull(t)
		outcome := f.outcome(1, nil)
		outcome.Result.Answer = "Sealed before caller cancellation"
		acknowledged := make(chan httpServerTestExchange, 1)
		go func() { acknowledged <- f.request("/outcome", outcome) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("outcome did not seal before ACK write")
		}
		f.tx.mu.Lock()
		sealed := f.tx.outcome
		f.tx.mu.Unlock()
		if !sealed {
			t.Fatal("fixture entered an outcome rejection before sealing")
		}
		canceledAt := time.Now()
		cancel()
		<-f.tx.stop
		got := f.result(t)
		if elapsed := time.Since(canceledAt); elapsed > 2*stopBudget {
			t.Fatalf("sealed ACK outlived caller cancellation bound: %s", elapsed)
		}
		if got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
			t.Fatalf("lost ACK erased sealed outcome: %+v error=%v", got.result, got.err)
		}
		if ack := <-acknowledged; ack.err == nil && ack.status == http.StatusOK && ack.ack.Failure == nil {
			t.Fatalf("blocked ACK was reported delivered: %+v", ack)
		}
		f.executor.mu.Lock()
		unavailable, closed := f.executor.unavailable, f.executor.closed
		f.executor.mu.Unlock()
		if !unavailable || closed {
			t.Fatalf("lost ACK capacity = unavailable:%t closed:%t", unavailable, closed)
		}
	})
}

func TestHTTPExecutorCloseBoundsCompletedWatchReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const stopBudget = 10 * time.Millisecond
		entered := make(chan struct{})
		f := newHTTPServerTestWithStopServer(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{},
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/cancel" {
						w = &httpDeadlineHeldWrite{ResponseWriter: w, entered: entered, deadlines: make(chan time.Time, 4)}
					}
					next.ServeHTTP(w, r)
				})
			}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, stopBudget)
		f.pull(t)
		body, err := json.Marshal(f.tx.binding)
		if err != nil {
			t.Fatal(err)
		}
		watch, err := f.server.Client().Post(f.server.URL+"/cancel", "application/json", bytes.NewReader(body))
		if err != nil || watch.StatusCode != http.StatusOK {
			t.Fatalf("watch admission: response=%v error=%v", watch, err)
		}
		defer watch.Body.Close()
		outcome := f.outcome(1, nil)
		outcome.Result.Answer = "Sealed before completed watch reply"
		f.ack(t, "/outcome", outcome, http.StatusOK, 1)
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("completed watch reason did not enter its write")
		}
		closed := make(chan error, 1)
		stoppedAt := time.Now()
		go func() { closed <- f.executor.Close() }()
		<-f.tx.stop
		got := f.result(t)
		if elapsed := time.Since(stoppedAt); elapsed > 2*stopBudget {
			t.Fatalf("completed watch reason outlived Close bound: %s", elapsed)
		}
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		if got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
			t.Fatalf("failed completed watch reason erased known outcome: %+v error=%v", got.result, got.err)
		}
	})
}

func TestHTTPExecutorMissingOutcomeClosesRealGraceAndQuarantines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f := newHTTPServerTestInMemory(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{})
		f.pull(t)
		ready := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 1, WorkerState: "ready", Phase: "cleanup"}
		f.ack(t, "/status", httpObservation{Status: ready}, http.StatusOK, 1)
		cancel()
		canceledAt := time.Now()
		<-f.tx.ctx.Done()
		select {
		case got := <-f.done:
			if elapsed := time.Since(canceledAt); elapsed < httpControlTimeout {
				t.Fatalf("missing outcome closed receipt grace after %s; want at least %s", elapsed, httpControlTimeout)
			}
			if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || errors.Is(got.err, monitor.ErrTerminalJob) || !got.result.matchesJob(f.job) || len(got.result.Calls) != 0 {
				t.Fatalf("missing outcome invented known execution facts: %+v error=%v", got.result, got.err)
			}
		case <-time.After(httpControlTimeout + 5*time.Second):
			t.Fatal("real control grace did not close missing-outcome admission")
		}
		if got := f.request("/outcome", f.outcome(1, nil)); got.err != nil || got.status != http.StatusConflict {
			t.Fatalf("outcome was accepted after grace finalization: %+v", got)
		}
		if _, err := f.executor.Execute(t.Context(), f.job, runnerExecutionCheckpoints{}); err == nil {
			t.Fatal("missing outcome released unproved worker capacity")
		}
		if got, _, ok := f.executor.Status(); !ok || got != ready {
			t.Fatalf("forged ready observation was not retained as an untrusted label: status=%+v present=%t", got, ok)
		}
	})
}

func TestHTTPExecutorShortStopBudgetDoesNotRenewForSerialControls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const stopBudget = 10 * time.Millisecond
		paid := make(chan CallUsage, 1)
		f := newHTTPServerTestWithStopServer(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{
			after: func(value CallCheckpoint) error { paid <- value.Call; return nil },
		}, nil,
			func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, stopBudget)
		if assignment := f.pull(t); assignment.StopGrace != stopBudget {
			t.Fatalf("assignment stop budget = %s, want %s", assignment.StopGrace, stopBudget)
		}
		f.ack(t, "/before", f.call(1), http.StatusOK, 1)
		stoppedAt := time.Now()
		cancel()
		<-f.tx.stop
		for sequence := uint64(1); sequence <= 2; sequence++ {
			if sequence == 2 {
				time.Sleep(stopBudget - 2*time.Millisecond)
				receipt := f.call(2)
				receipt.Checkpoint.Call.Tokens = usage.Tokens{Input: 17}
				receipt.Checkpoint.Call.Complete = true
				f.ack(t, "/after", receipt, http.StatusOK, 2)
				if got := <-paid; got != receipt.Checkpoint.Call {
					t.Fatalf("late paid receipt changed before stop expiry: got=%+v want=%+v", got, receipt.Checkpoint.Call)
				}
			}
			status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head,
				Sequence: sequence, WorkerState: "ready", Phase: "cleanup"}
			want := http.StatusOK
			if sequence > 1 {
				want = http.StatusConflict // A repeated ready label is invalid, but still consumes a control reply.
			}
			f.ack(t, "/status", httpObservation{Status: status}, want, sequence)
		}
		got := f.result(t)
		if elapsed := time.Since(stoppedAt); elapsed < stopBudget || elapsed > stopBudget+2*time.Millisecond {
			t.Fatalf("serial controls changed stop budget: elapsed=%s budget=%s", elapsed, stopBudget)
		}
		if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("missing final outcome changed cancellation: result=%+v error=%v", got.result, got.err)
		}
		if late := f.request("/outcome", f.outcome(1, nil)); late.err != nil || late.status != http.StatusConflict {
			t.Fatalf("late result entered after fixed stop deadline: %+v", late)
		}
		if _, err := f.executor.Execute(t.Context(), f.job, runnerExecutionCheckpoints{}); err == nil {
			t.Fatal("missing outcome released worker capacity")
		}
	})
}

func TestHTTPExecutorRejectsInvalidStopBudgetBeforeAdmission(t *testing.T) {
	job := testJob(command.Comment)
	for _, stopBudget := range []time.Duration{0, -time.Nanosecond, config.MaxActionRuntime + time.Nanosecond} {
		executor, err := NewHTTPExecutor(job.Identity.ExecutorID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := executor.ExecuteWithStop(t.Context(), stopBudget, job, runnerExecutionCheckpoints{})
		if !errors.Is(err, monitor.ErrTerminalJob) || !result.matchesJob(job) || executor.transaction() != nil {
			t.Fatalf("stop budget %s admitted work: result=%+v error=%v", stopBudget, result, err)
		}
	}
}

func TestHTTPExecutorStopContractsEnteredMutationBodyRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered := make(chan struct{})
		const stopBudget = 10 * time.Millisecond
		f := newHTTPServerTestWithStopServer(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{},
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/after" {
						r.Body = &httpSecondReadBody{ReadCloser: r.Body, entered: entered}
					}
					next.ServeHTTP(w, r)
				})
			}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, stopBudget)
		f.pull(t)
		reader, writer := io.Pipe()
		defer writer.Close()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/after", reader)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		responseDone := make(chan error, 1)
		go func() {
			response, err := f.server.Client().Do(request)
			if response != nil {
				_ = response.Body.Close()
			}
			responseDone <- err
		}()
		if _, err := writer.Write([]byte(`{"sequence":`)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("mutation did not enter its partial body read")
		}
		stoppedAt := time.Now()
		cancel()
		<-f.tx.stop
		got := f.result(t)
		if elapsed := time.Since(stoppedAt); elapsed > 2*stopBudget {
			t.Fatalf("entered network read outlived stop budget: %s", elapsed)
		}
		if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("partial mutation changed final outcome: %+v error=%v", got.result, got.err)
		}
		_ = writer.Close()
		select {
		case <-responseDone:
		case <-time.After(5 * time.Second):
			t.Fatal("client did not finish after stop")
		}
	})
}

func TestHTTPExecutorCloseUsesSameShortStopBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const stopBudget = 10 * time.Millisecond
		f := newHTTPServerTestWithStopServer(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil,
			func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, stopBudget)
		f.pull(t)
		closed := make(chan error, 1)
		stoppedAt := time.Now()
		go func() { closed <- f.executor.Close() }()
		<-f.tx.stop
		got := f.result(t)
		if err := <-closed; err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		if elapsed := time.Since(stoppedAt); elapsed < stopBudget || elapsed > 2*stopBudget {
			t.Fatalf("Close changed stop budget: elapsed=%s budget=%s", elapsed, stopBudget)
		}
		if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("shutdown lost unknown outcome: %+v error=%v", got.result, got.err)
		}
	})
}

func TestHTTPExecutorCloseReusesCancellationStopDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const stopBudget = 10 * time.Millisecond
		f := newHTTPServerTestWithStopServer(t, ctx, testJob(command.Comment), runnerExecutionCheckpoints{}, nil,
			func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, stopBudget)
		f.pull(t)
		canceledAt := time.Now()
		cancel()
		<-f.tx.stop
		stopUntil := f.tx.controlDeadline()
		time.Sleep(stopBudget - 2*time.Millisecond)
		closed := make(chan error, 1)
		go func() { closed <- f.executor.Close() }()
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		got := f.result(t)
		if deadline := f.tx.controlDeadline(); !deadline.Equal(stopUntil) {
			t.Fatalf("Close renewed stop deadline: first=%s later=%s", stopUntil, deadline)
		}
		if elapsed := time.Since(canceledAt); elapsed > stopBudget+2*time.Millisecond {
			t.Fatalf("Close renewed cancellation budget: %s", elapsed)
		}
		if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("missing final outcome changed cancellation: %+v error=%v", got.result, got.err)
		}
	})
}

func TestHTTPExecutorCloseCancelsLateStopTimer(t *testing.T) {
	e, err := NewHTTPExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	// Execute has already captured a nil stopCancel, but its transaction is still active.
	tx := &httpTransaction{stopBudget: func() {}, stopGrace: time.Hour, stop: make(chan struct{}), finished: make(chan struct{})}
	e.active = tx
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	<-tx.stop
	close(tx.finished)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := tx.stopContext.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close retained a late stop timer: %v", err)
	}
}

func TestHTTPServerStopKeepsEarlierRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tx := &httpTransaction{stop: make(chan struct{})}
		original := time.Now().Add(10 * time.Millisecond)
		updated := make(chan time.Time, 1)
		finish := httpServerDeadlineWatch([]*httpTransaction{tx}, original, func(deadline time.Time) error {
			updated <- deadline
			return nil
		})
		time.Sleep(9 * time.Millisecond)
		tx.mu.Lock()
		tx.stopUntil = time.Now().Add(30 * time.Millisecond)
		close(tx.stop)
		tx.mu.Unlock()
		if deadline := <-updated; !deadline.Equal(original) {
			t.Fatalf("stop extended request deadline: original=%s new=%s", original, deadline)
		}
		if err := finish(); err != nil {
			t.Fatal(err)
		}
	})
}
