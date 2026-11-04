package action

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"
)

type preflightGateWriter struct {
	*httptest.ResponseRecorder
	executor        *HTTPExecutor
	gateOpenOnWrite bool
}

func (*preflightGateWriter) SetWriteDeadline(time.Time) error { return nil }
func (*preflightGateWriter) SetReadDeadline(time.Time) error  { return nil }
func (*preflightGateWriter) FlushError() error                { return nil }

func (w *preflightGateWriter) Write(body []byte) (int, error) {
	w.executor.mu.Lock()
	w.gateOpenOnWrite = w.executor.choicesFetched
	w.executor.mu.Unlock()
	return w.ResponseRecorder.Write(body)
}

func TestRemoteChoicePreflightOpensGateBeforeResponse(t *testing.T) {
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	executor.remoteChoices = &remoteChoicePreflight{Version: ProtocolVersion, ExecutorID: "remote",
		Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}}
	body, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "remote"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/preflight", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	writer := &preflightGateWriter{ResponseRecorder: httptest.NewRecorder(), executor: executor}
	executor.serveRemoteChoices(writer, request)
	if writer.Code != http.StatusOK || !writer.gateOpenOnWrite || writer.Body.Len() == 0 {
		t.Fatalf("preflight response exposed before pull gate: status=%d gate=%t body=%q",
			writer.Code, writer.gateOpenOnWrite, writer.Body.String())
	}
}

func TestRemoteChoicePreflightBoundsUnresponsiveResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &http.Client{Transport: httpClientTestTransport(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: httpBlockedResponseBody{request.Context()}, Request: request}, nil
		})}
		finished := make(chan error, 1)
		started := time.Now()
		go func() { finished <- PreflightRemoteChoices(t.Context(), client, "https://example.test", "remote") }()
		synctest.Wait()
		if err := <-finished; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unresponsive preflight error = %v, want deadline", err)
		}
		if elapsed := time.Since(started); elapsed != httpControlTimeout {
			t.Fatalf("unresponsive preflight elapsed %s, want %s", elapsed, httpControlTimeout)
		}
	})
}
