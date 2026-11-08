package action

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestRemoteAuditCapabilityIsFrozenAcrossPreflightAndPull(t *testing.T) {
	for _, supported := range []bool{false, true} {
		executor, err := NewHTTPExecutor("remote")
		if err != nil {
			t.Fatal(err)
		}
		executor.remoteChoices = &remoteChoicePreflight{Version: ProtocolVersion, ExecutorID: "remote",
			Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}}
		endpoint := &RemoteEndpoint{executor: executor}
		post := func(path string, capability bool) int {
			t.Helper()
			body, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "remote", AuditHostSupported: capability}, httpControlBytes)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := &preflightGateWriter{ResponseRecorder: httptest.NewRecorder(), executor: executor}
			executor.ServeHTTP(response, request)
			return response.Code
		}
		if got := post("/preflight", supported); got != http.StatusOK || endpoint.AuditHostSupported() != supported {
			t.Fatalf("capability %t preflight: status=%d stored=%t", supported, got, endpoint.AuditHostSupported())
		}
		if got := post("/preflight", !supported); got != http.StatusConflict || endpoint.AuditHostSupported() != supported {
			t.Fatalf("changed capability %t preflight: status=%d stored=%t", !supported, got, endpoint.AuditHostSupported())
		}
		if got := post("/next", !supported); got != http.StatusConflict || executor.pull {
			t.Fatalf("changed capability %t pull: status=%d pull=%t", !supported, got, executor.pull)
		}
	}
}

func TestRemoteChoicePreflightRejectsUnavailableExecutor(t *testing.T) {
	const secret = "private-preflight-credential"
	for _, state := range []string{"closed", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			executor, err := NewHTTPExecutor("remote")
			if err != nil {
				t.Fatal(err)
			}
			executor.remoteChoices = &remoteChoicePreflight{Version: ProtocolVersion, ExecutorID: "remote",
				Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}}
			executor.credentials = WorkerCredentials{CodexAccessToken: secret}
			if state == "closed" {
				if err := executor.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				executor.mu.Lock()
				executor.unavailable = true
				executor.mu.Unlock()
			}
			body, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "remote"}, httpControlBytes)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/preflight", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := &preflightGateWriter{ResponseRecorder: httptest.NewRecorder(), executor: executor}
			executor.serveRemoteChoices(response, request)
			if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), secret) || executor.choicesFetched {
				t.Fatalf("%s executor exposed preflight: status=%d body=%q gate=%t",
					state, response.Code, response.Body.String(), executor.choicesFetched)
			}
		})
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
		go func() {
			finished <- PreflightRemoteChoices(t.Context(), client, "https://example.test", "remote", false)
		}()
		synctest.Wait()
		if err := <-finished; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unresponsive preflight error = %v, want deadline", err)
		}
		if elapsed := time.Since(started); elapsed != httpControlTimeout {
			t.Fatalf("unresponsive preflight elapsed %s, want %s", elapsed, httpControlTimeout)
		}
	})
}
