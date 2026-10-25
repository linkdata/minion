package action

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
)

type httpRepullRecorder struct{ *httptest.ResponseRecorder }

func (*httpRepullRecorder) SetReadDeadline(time.Time) error  { return nil }
func (*httpRepullRecorder) SetWriteDeadline(time.Time) error { return nil }

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
		assignment := httpAssignment{Job: job, Expires: time.Now().Add(time.Minute).UnixNano()}
		body, err := httpEncode(assignment, httpJobBytes)
		if err != nil {
			t.Fatal(err)
		}
		executor.mu.Lock()
		executor.active = nil
		executor.notifyLocked()
		executor.mu.Unlock()
		synctest.Wait()
		executor.mu.Lock()
		executor.active = &httpTransaction{ctx: t.Context(), ready: true, body: body}
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
		if err := httpDecode(response.Body, httpJobBytes, &received); err != nil || response.StatusCode != http.StatusOK || !reflect.DeepEqual(received, assignment) {
			t.Fatalf("next assignment: status=%d value=%+v error=%v", response.StatusCode, received, err)
		}
	})
}
