package action

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

type httpRepullRecorder struct{ *httptest.ResponseRecorder }

func (*httpRepullRecorder) SetReadDeadline(time.Time) error  { return nil }
func (*httpRepullRecorder) SetWriteDeadline(time.Time) error { return nil }

type httpPausedAssignmentWriter struct {
	http.ResponseWriter
	flushed chan struct{}
	resume  <-chan struct{}
}

func TestHTTPExecutorCanceledPullRechecksBeforeRepull(t *testing.T) {
	f := newHTTPServerTestInMemory(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{})
	entered, release, firstDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f.tx.mu.Lock()
	f.tx.beforeDelivery = func(Job) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return nil
		}
		return monitor.ErrAcceptedRetry
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
	firstResponse := &httpRepullRecorder{httptest.NewRecorder()}
	go func() {
		f.executor.serveNext(firstResponse, request)
		close(firstDone)
	}()
	wait, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	select {
	case <-entered:
	case <-wait.Done():
		t.Fatal("first pull did not reach assignment validation")
	}
	cancel()
	close(release)
	select {
	case <-firstDone:
	case <-wait.Done():
		t.Fatal("canceled pull kept the slot")
	}
	if firstResponse.Body.Len() != 0 || f.tx.delivered {
		t.Fatalf("canceled pull received job bytes: bytes=%d delivered=%t", firstResponse.Body.Len(), f.tx.delivered)
	}
	reply := f.request("/next", httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID})
	if reply.err != nil || reply.status != http.StatusInternalServerError || reply.ack.Failure == nil || f.tx.delivered || calls.Load() != 2 {
		t.Fatalf("repull skipped final validation or delivered bytes: reply=%+v delivered=%t calls=%d", reply, f.tx.delivered, calls.Load())
	}
	if got := f.result(t); !errors.Is(got.err, monitor.ErrAcceptedRetry) {
		t.Fatalf("repull lost transient refusal: %v", got.err)
	}
}

func (w *httpPausedAssignmentWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *httpPausedAssignmentWriter) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
	close(w.flushed)
	<-w.resume
}

func TestHTTPExecutorRetiredPullReleasesSlotBeforeACK(t *testing.T) {
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := &RemoteEndpoint{executor: executor, serveDone: make(chan struct{})}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	flushed, resume, firstDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	authenticated := authenticatedExecutionHandler(executor, token)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" && !first.Swap(true) {
			authenticated.ServeHTTP(&httpPausedAssignmentWriter{w, flushed, resume}, r)
			close(firstDone)
			return
		}
		authenticated.ServeHTTP(w, r)
	}))
	release := sync.OnceFunc(func() { close(resume) })
	defer server.Close()
	defer func() { _ = executor.Close() }()
	defer release()
	server.Client().Transport.(*http.Transport).DisableKeepAlives = true
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "remote"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	request := func(ctx context.Context) <-chan httpServerTestExchange {
		finished := make(chan httpServerTestExchange, 1)
		go func() {
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/next", bytes.NewReader(pull))
			if err != nil {
				finished <- httpServerTestExchange{err: err}
				return
			}
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token)
			response, err := server.Client().Do(r)
			got := httpServerTestExchange{err: err}
			if response != nil {
				got.status = response.StatusCode
				_ = response.Body.Close()
			}
			finished <- got
		}()
		return finished
	}
	executor.mu.Lock()
	changed := executor.changed
	executor.mu.Unlock()
	oldPull := request(ctx)
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatalf("old authenticated pull did not acquire a slot: %v", ctx.Err())
	}
	endpoint.RequireFreshPull()
	select {
	case <-flushed:
	case <-ctx.Done():
		t.Fatalf("retired pull did not flush its ACK: %v", ctx.Err())
	}
	select {
	case got := <-oldPull:
		if got.err != nil || got.status != http.StatusTooEarly {
			t.Fatalf("retired pull response = %+v", got)
		}
	case <-ctx.Done():
		t.Fatalf("worker did not receive retired pull ACK: %v", ctx.Err())
	}
	newCtx, stopNew := context.WithCancel(ctx)
	defer stopNew()
	newPull := request(newCtx)
	capacity := make(chan error, 1)
	go func() { capacity <- (&remoteWorker{executor: executor, endpoint: endpoint}).WaitCapacity(ctx) }()
	select {
	case got := <-newPull:
		t.Fatalf("fresh authenticated pull was rejected before old handler returned: %+v", got)
	case err := <-capacity:
		if err != nil {
			t.Fatalf("fresh authenticated pull did not restore capacity: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("fresh authenticated pull stalled: %v", ctx.Err())
	}
	release()
	select {
	case <-firstDone:
	case <-ctx.Done():
		t.Fatalf("retired handler did not return: %v", ctx.Err())
	}
	readyCtx, stopReady := context.WithTimeout(ctx, time.Second)
	defer stopReady()
	if err := (&remoteWorker{executor: executor, endpoint: endpoint}).WaitCapacity(readyCtx); err != nil {
		t.Fatalf("retired handler cleared its successor pull: %v", err)
	}
	stopNew()
	select {
	case <-newPull:
	case <-ctx.Done():
		t.Fatalf("canceled successor pull did not return: %v", ctx.Err())
	}
}

func TestHTTPExecutorNextPullSlotReleasedBeforeAssignmentHandlerReturns(t *testing.T) {
	flushed := make(chan struct{})
	resume := make(chan struct{})
	firstDone := make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	var first atomic.Bool
	f := newHTTPServerTest(t, t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/next" && !first.Swap(true) {
				next.ServeHTTP(&httpPausedAssignmentWriter{w, flushed, resume}, r)
				close(firstDone)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	// A paused handler cannot read a later request on its HTTP/1 connection.
	f.server.Client().Transport.(*http.Transport).DisableKeepAlives = true
	f.pull(t)
	select {
	case <-flushed:
	case <-time.After(5 * time.Second):
		t.Fatal("assignment response did not reach Flush")
	}
	f.ack(t, "/outcome", f.outcome(1, nil), http.StatusOK, 1)

	f.executor.mu.Lock()
	changed := f.executor.changed
	f.executor.mu.Unlock()
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+"/next", bytes.NewReader(pull))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	second := make(chan httpServerTestExchange, 1)
	go func() {
		response, err := f.server.Client().Do(request)
		result := httpServerTestExchange{err: err}
		if response != nil {
			result.status = response.StatusCode
			response.Body.Close()
		}
		second <- result
	}()
	select {
	case <-changed:
	case result := <-second:
		t.Fatalf("next pull returned while assignment handler was still flushing: %+v", result)
	case <-time.After(5 * time.Second):
		t.Fatal("next pull did not acquire its slot")
	}

	close(resume)
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("assignment handler did not return")
	}
	f.executor.mu.Lock()
	held := f.executor.pull
	f.executor.mu.Unlock()
	if !held {
		t.Fatal("old assignment handler cleared the new pull slot")
	}
	f.ack(t, "/next", httpPull{Version: ProtocolVersion, ExecutorID: f.job.Identity.ExecutorID}, http.StatusConflict, 0)
	f.result(t)
	cancel()
	select {
	case result := <-second:
		if result.err == nil {
			t.Fatalf("canceled next pull returned an unexpected response: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled next pull did not return")
	}
}

func TestHTTPExecutorNextPullWaitsForCompletedTransactionDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor, err := NewHTTPExecutor("local")
		if err != nil {
			t.Fatal(err)
		}
		previous := &httpTransaction{delivered: true, fenced: true, outcome: true, finished: make(chan struct{})}
		executor.active = previous
		pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "local"}, httpControlBytes)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/next", bytes.NewReader(pull))
		request.Header.Set("Content-Type", "application/json")
		writer := &httpRepullRecorder{httptest.NewRecorder()}
		done := make(chan struct{})
		go func() {
			executor.serveNext(writer, request)
			close(done)
		}()
		synctest.Wait()
		executor.mu.Lock()
		held := executor.pull
		executor.mu.Unlock()
		if !held {
			t.Fatal("completed transaction rejected the next pull before its handlers drained")
		}
		select {
		case <-done:
			t.Fatal("next pull returned before the completed transaction drained")
		default:
		}

		job := testJob(command.Comment)
		budget, cancelBudget := context.WithTimeout(t.Context(), time.Minute)
		defer cancelBudget()
		executor.mu.Lock()
		executor.active = nil
		executor.notifyLocked()
		executor.mu.Unlock()
		synctest.Wait()
		executor.mu.Lock()
		executor.active = &httpTransaction{job: job, ctx: budget, budget: budget, ready: true}
		executor.notifyLocked()
		executor.mu.Unlock()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("next assignment arrived before the prior transaction finished")
		default:
		}

		close(previous.finished)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("next pull stayed blocked after the previous transaction finished")
		}
		response := writer.Result()
		defer response.Body.Close()
		var received httpAssignment
		if err := httpDecode(response.Body, httpJobBytes, &received); err != nil || response.StatusCode != http.StatusOK ||
			received.Job.Identity != job.Identity || received.Job.RuntimeBudget <= 0 || received.Job.RuntimeBudget > time.Minute || received.HeldFor < 0 {
			t.Fatalf("next assignment: status=%d value=%+v error=%v", response.StatusCode, received, err)
		}
	})
}
