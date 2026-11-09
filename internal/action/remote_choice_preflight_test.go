package action

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

type preflightBlockedWriter struct {
	*preflightGateWriter
	entered chan struct{}
	release chan struct{}
}

func (w *preflightBlockedWriter) Write(body []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.preflightGateWriter.Write(body)
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

func TestRemotePreflightACKHoldsFetchAndPullUntilReported(t *testing.T) {
	for _, result := range []string{"ok", "failed"} {
		for _, path := range []string{"/preflight", "/next"} {
			t.Run(result+path, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					executor, err := NewHTTPExecutor("remote")
					if err != nil {
						t.Fatal(err)
					}
					old := WorkerCredentials{CodexAPIKey: "old-key"}
					candidate := WorkerCredentials{CodexAPIKey: "new-key"}
					if err := executor.SetWorkerCredentials(old); err != nil {
						t.Fatal(err)
					}
					executor.remoteChoices = &remoteChoicePreflight{Version: ProtocolVersion, ExecutorID: "remote",
						Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}}
					executor.preflightGeneration = 1
					executor.preflightCandidate = candidate
					executor.preflightPending = true
					executor.preflightWait = make(chan error, 1)
					pull := httpPull{Version: ProtocolVersion, ExecutorID: "remote"}
					request := func(path string, value httpPull, ctx context.Context) *http.Request {
						t.Helper()
						body, err := httpEncode(value, httpControlBytes)
						if err != nil {
							t.Fatal(err)
						}
						r := httptest.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
						r.Header.Set("Content-Type", "application/json")
						return r
					}
					ack := pull
					ack.PreflightGeneration, ack.PreflightResult = 1, result
					blocked := &preflightBlockedWriter{
						preflightGateWriter: &preflightGateWriter{ResponseRecorder: httptest.NewRecorder(), executor: executor},
						entered:             make(chan struct{}), release: make(chan struct{}),
					}
					releaseACK := sync.OnceFunc(func() { close(blocked.release) })
					defer releaseACK()
					ackRequest := request("/preflight", ack, context.Background())
					ackDone := make(chan struct{})
					go func() {
						executor.ServeHTTP(blocked, ackRequest)
						close(ackDone)
					}()
					<-blocked.entered
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					observerRequest := request(path, pull, ctx)
					response := &preflightGateWriter{ResponseRecorder: httptest.NewRecorder(), executor: executor}
					observerDone := make(chan struct{})
					go func() {
						executor.ServeHTTP(response, observerRequest)
						close(observerDone)
					}()
					synctest.Wait()
					select {
					case <-observerDone:
						t.Fatalf("%s returned while %s ACK write was blocked: status=%d", path, result, response.Code)
					default:
					}
					executor.mu.Lock()
					fetched, pulling := executor.choicesFetched, executor.pull
					executor.mu.Unlock()
					if fetched || pulling {
						t.Fatalf("%s changed gate during blocked %s ACK: fetched=%t pulling=%t", path, result, fetched, pulling)
					}
					releaseACK()
					<-ackDone
					synctest.Wait()
					if path == "/next" && result == "ok" {
						executor.mu.Lock()
						pulling = executor.pull
						executor.mu.Unlock()
						if !pulling {
							t.Fatal("pull did not enter after successful ACK")
						}
						cancel()
						<-observerDone
						return
					}
					<-observerDone
					if path == "/next" {
						if response.Code != http.StatusPreconditionRequired {
							t.Fatalf("pull after failed ACK status=%d, want %d", response.Code, http.StatusPreconditionRequired)
						}
						return
					}
					var selection remoteChoicePreflight
					if err := httpDecode(response.Body, httpControlBytes, &selection); err != nil {
						t.Fatal(err)
					}
					want := candidate
					if result == "failed" {
						want = old
					}
					if response.Code != http.StatusOK || selection.Refresh || selection.Credentials != want {
						t.Fatalf("fetch after %s ACK: status=%d selection=%+v", result, response.Code, selection)
					}
				})
			})
		}
	}
}

func TestRemoteAuditCapabilityIsFrozenAcrossPreflight(t *testing.T) {
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
	}
}

func TestRemoteRejectedCredentialRestoresPriorPullGate(t *testing.T) {
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	old := WorkerCredentials{CodexAPIKey: "old-key"}
	if err := executor.SetWorkerCredentials(old); err != nil {
		t.Fatal(err)
	}
	executor.remoteChoices = &remoteChoicePreflight{Version: ProtocolVersion, ExecutorID: "remote",
		Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}}
	executor.choicesFetched = true
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	updated := make(chan error, 1)
	go func() {
		updated <- executor.updateIdleWorkerCredentials(ctx, WorkerCredentials{CodexAPIKey: "bad-key"})
	}()
	for {
		executor.mu.Lock()
		pending, changed := executor.preflightPending, executor.changed
		executor.mu.Unlock()
		if pending {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("credential candidate was not staged")
		}
	}
	post := func(path string, pull httpPull, value any) int {
		t.Helper()
		body, err := httpEncode(pull, httpControlBytes)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := &preflightGateWriter{ResponseRecorder: httptest.NewRecorder(), executor: executor}
		executor.ServeHTTP(response, request)
		if err := httpDecode(response.Body, httpControlBytes, value); err != nil {
			t.Fatal(err)
		}
		return response.Code
	}
	pull := httpPull{Version: ProtocolVersion, ExecutorID: "remote"}
	var candidate remoteChoicePreflight
	if status := post("/preflight", pull, &candidate); status != http.StatusOK || !candidate.Refresh || candidate.Credentials.CodexAPIKey != "bad-key" {
		t.Fatalf("candidate fetch: status=%d selection=%+v", status, candidate)
	}
	var ack httpAcknowledgement
	if status := post("/next", pull, &ack); status != http.StatusPreconditionRequired {
		t.Fatalf("pull during candidate preflight status=%d", status)
	}
	failed := pull
	failed.PreflightGeneration, failed.PreflightResult = candidate.PreflightGeneration, "arbitrary worker text"
	if status := post("/preflight", failed, &ack); status != http.StatusBadRequest {
		t.Fatalf("untrusted validation message status=%d", status)
	}
	select {
	case err := <-updated:
		t.Fatalf("untrusted validation message resolved the candidate: %v", err)
	default:
	}
	failed.PreflightResult = preflightCodexLoginUnavailable
	if status := post("/preflight", failed, &ack); status != http.StatusOK {
		t.Fatalf("failed validation ACK status=%d", status)
	}
	if err := <-updated; err == nil || !strings.Contains(err.Error(), preflightCodexLoginUnavailable) {
		t.Fatalf("rejected credential reason = %v", err)
	}
	executor.mu.Lock()
	gate, pending := executor.choicesFetched, executor.preflightPending
	executor.mu.Unlock()
	if !gate || pending {
		t.Fatalf("prior pull gate was not restored: gate=%t pending=%t", gate, pending)
	}
	failed.PreflightResult = "ok" // The successful ACK for the discarded generation must be stale.
	if status := post("/preflight", failed, &ack); status != http.StatusPreconditionRequired {
		t.Fatalf("stale candidate ACK status=%d", status)
	}
	var restored remoteChoicePreflight
	if status := post("/preflight", pull, &restored); status != http.StatusOK || restored.Refresh || restored.Credentials != old || !executor.choicesFetched {
		t.Fatalf("restored selection: status=%d selection=%+v gate=%t", status, restored, executor.choicesFetched)
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

func TestRemoteChoicePreflightReturnsACKErrorWithoutRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		selection, err := httpEncode(remoteChoicePreflight{Version: ProtocolVersion, ExecutorID: "remote",
			Refresh: true, PreflightGeneration: 1}, httpControlBytes)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: httpClientTestTransport(func(request *http.Request) (*http.Response, error) {
			var pull httpPull
			if err := httpDecode(request.Body, httpControlBytes, &pull); err != nil {
				return nil, err
			}
			select {
			case <-time.After(20 * time.Second):
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
			if pull.PreflightResult != "" {
				return nil, io.ErrUnexpectedEOF // The ACK did not reach the parent.
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader(selection)), Request: request}, nil
		})}
		finished := make(chan error, 1)
		started := time.Now()
		go func() {
			finished <- preflightRemoteChoices(t.Context(), client, "https://example.test", "remote", false, remoteChoicePreflightTimeout)
		}()
		synctest.Wait()
		if err := <-finished; !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("ACK error = %v, want unexpected EOF", err)
		}
		if elapsed := time.Since(started); elapsed != 40*time.Second {
			t.Fatalf("ACK elapsed %s, want 40s without another fetch", elapsed)
		}
	})
}
