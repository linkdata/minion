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
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/processgroup"
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
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 1)
	status.Sequence, status.Phase = 2, "upload"
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 2)
	if got, ok := f.executor.Status(); !ok || got != status {
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
			var saves atomic.Int32
			f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{
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
			f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
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
				if err := httpDecode(response.Body, httpJobBytes, &assignment); err != nil || !reflect.DeepEqual(assignment.Job, f.job) {
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

func (b *httpStatusHeldBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.ReadCloser.Read(p)
}

func TestHTTPExecutorReadingStatusDoesNotHoldPaidCallbackSlot(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
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

func TestHTTPExecutorCanceledPullDoesNotClaimReadyAssignment(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var canceled atomic.Bool
	f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/next" && !canceled.Swap(true) {
				r = r.WithContext(ctx)
			}
			next.ServeHTTP(w, r)
		})
	})
	response := f.request("/next", httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID})
	f.tx.mu.Lock()
	claimed, retained := f.tx.delivered, len(f.tx.body) != 0
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
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
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
	f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{after: func(CallCheckpoint) error { after.Add(1); return nil }}, nil)
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
			Canceled: true, Deadline: true, Unsafe: true, Descendants: true, Unconfirmed: true,
			ScopeUnconfirmed: true, Exhausted: true, OutcomeUnknown: true, RequestTransport: true,
			MissingExecutable: true, NoSpace: true, QuotaExceeded: true, OutputLimit: true}
	}
	assertWorkerFailure := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("worker failure became success")
		}
		for _, cause := range []error{monitor.ErrTerminalJob, context.Canceled, context.DeadlineExceeded,
			workroot.ErrUnsafe, processgroup.ErrDescendants, processgroup.ErrUnconfirmed,
			processgroup.ErrScopeUnconfirmed, processgroup.ErrExhausted, monitor.ErrExecutionOutcomeUnknown,
			monitor.ErrRequestTransport, exec.ErrNotFound, syscall.ENOSPC, syscall.EDQUOT,
			aicli.ErrOutputLimit} {
			if errors.Is(err, cause) {
				t.Fatalf("worker claim acquired parent policy authority for %v: %v", cause, err)
			}
		}
		var journal bytes.Buffer
		log.New(&journal, "", 0).Printf("worker failure: %v", err)
		line := journal.String()
		if strings.Count(line, "\n") != 1 || strings.ContainsAny(line, "\r\x1b") ||
			!strings.Contains(line, `\nFORGED JOURNAL ENTRY\x1b`) {
			t.Fatalf("worker diagnostic escaped the log line: %q", line)
		}
	}

	for _, paid := range []bool{false, true} {
		t.Run(fmt.Sprintf("paid=%t", paid), func(t *testing.T) {
			var returned atomic.Int32
			f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{
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
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := f.executor.Execute(ctx, f.job, runnerExecutionCheckpoints{}); !errors.Is(err, context.Canceled) {
				t.Fatalf("worker claims reserved the next assignment slot: %v", err)
			}
		})
	}

	t.Run("unreleased worker scope", func(t *testing.T) {
		f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
		f.pull(t)
		outcome := f.outcome(1, nil)
		outcome.Failure = workerFailure()
		outcome.Failure.Exhaustion = &processgroup.Exhaustion{
			MaxTasks: 128, MaxMemoryBytes: 3 << 30, PIDEventsMax: 1,
			Stop: "forced", Scope: "/worker\nFORGED SCOPE\x1b[2J", Released: false,
		}
		f.ack(t, "/outcome", outcome, http.StatusOK, 1)
		got := f.result(t)
		assertWorkerFailure(t, got.err)
		var scope *processgroup.Exhaustion
		var journal bytes.Buffer
		log.New(&journal, "", 0).Printf("worker failure: %v", got.err)
		if errors.As(got.err, &scope) || strings.Contains(journal.String(), "FORGED SCOPE") {
			t.Fatalf("worker scope path became parent process evidence or log content: error=%v log=%q", got.err, journal.String())
		}
	})

	t.Run("parent checkpoint", func(t *testing.T) {
		parentErr := fmt.Errorf("parent save: %w", workroot.ErrUnsafe)
		f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{
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
		f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
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
			f := newHTTPServerTest(t, context.Background(), withClaudeCritic(testJob(command.Comment)), sink, nil)
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

func TestHTTPExecutorStatusIsIndependentAndAllowsObservationGaps(t *testing.T) {
	var before atomic.Int32
	f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{before: func(CallCheckpoint) error { before.Add(1); return nil }}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/status" {
				w = httpServerFailWrite{w}
			}
			next.ServeHTTP(w, r)
		})
	})
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 2, WorkerState: "busy", Phase: "upload"}
	if got := f.request("/status", httpObservation{Status: status}); got.err == nil {
		t.Fatal("injected observation ACK failure unexpectedly succeeded")
	}
	if got, ok := f.executor.Status(); !ok || got != status || f.tx.ctx.Err() != nil {
		t.Fatalf("lost observation 1 or ACK changed action admission: status=%+v present=%t ctx=%v", got, ok, f.tx.ctx.Err())
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

func TestHTTPExecutorRejectsStaleAndForeignStatusWithoutChangingPaidSequence(t *testing.T) {
	f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 2, WorkerState: "busy", Phase: "upload"}
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusOK, 2)
	f.ack(t, "/status", httpObservation{Status: status}, http.StatusConflict, 2)
	foreign := status
	foreign.Sequence, foreign.Identity.AttemptID = 3, "foreign"
	f.ack(t, "/status", httpObservation{Status: foreign}, http.StatusConflict, 3)
	if got, ok := f.executor.Status(); !ok || got != status || f.tx.ctx.Err() != nil {
		t.Fatalf("rejected observation changed accepted status or canceled action: %+v %t %v", got, ok, f.tx.ctx.Err())
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)
	if got := f.result(t); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestHTTPExecutorBoundsRejectedStatusBinding(t *testing.T) {
	for _, malformed := range []string{"shape", "unknown field", "trailing value"} {
		t.Run(malformed, func(t *testing.T) {
			f := newHTTPServerTest(t, context.Background(), testJob(command.Comment), runnerExecutionCheckpoints{}, nil)
			f.pull(t)
			status := Status{Version: ProtocolVersion, Identity: f.job.Identity, SourceRef: f.job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "prepare"}
			status.Identity.JobID = strings.Repeat("<", 30_000)
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
			if _, ok := f.executor.Status(); ok {
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
	if !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(got.err, storeErr) || !got.result.matchesJob(f.job) || len(got.result.Calls) != 0 || saved != call.Checkpoint.Call {
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
			if !reflect.DeepEqual(got.result, outcome.Result) || (got.err == nil) != (runErr == nil) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || errors.Is(got.err, monitor.ErrTerminalJob) || errors.Is(got.err, context.DeadlineExceeded) {
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

// Simulate the interval after a deadline but before its cancellation callback runs.
type httpPendingDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c httpPendingDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestHTTPExecutionContextErrorPreservesPriorCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
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
			f := newHTTPServerTest(t, context.Background(), owner.job, owner.sink, nil)
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
	for _, test := range []struct {
		name    string
		failure error
	}{
		{name: "worker reports cancellation", failure: context.Canceled},
		{name: "worker reports success"},
	} {
		t.Run(test.name, func(t *testing.T) {
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
