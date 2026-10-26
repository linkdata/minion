package action

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
)

type httpRepullRecorder struct{ *httptest.ResponseRecorder }

func (*httpRepullRecorder) SetReadDeadline(time.Time) error  { return nil }
func (*httpRepullRecorder) SetWriteDeadline(time.Time) error { return nil }

type httpPausedAssignmentWriter struct {
	http.ResponseWriter
	flushed chan struct{}
	resume  <-chan struct{}
}

func (w *httpPausedAssignmentWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *httpPausedAssignmentWriter) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
	close(w.flushed)
	<-w.resume
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
