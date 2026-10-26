package action

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func testLocalEndpoint(t *testing.T) (*HTTPExecutor, *LocalEndpoint) {
	t.Helper()
	core, err := NewHTTPExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := ListenLocalExecution(core)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := endpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	return core, endpoint
}

func testLocalPost(t *testing.T, client *http.Client, address string, body []byte) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, address+"/next", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	return client.Do(request)
}

func TestLocalEndpointPinsOriginCertificateAndAuthorizesBeforeBody(t *testing.T) {
	_, endpoint := testLocalEndpoint(t)
	access := endpoint.Access()
	u, err := url.Parse(access.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() != "127.0.0.1" {
		t.Fatalf("endpoint is not IPv4 loopback HTTPS: %q, %v", access.BaseURL, err)
	}
	invalidOrigin := access
	invalidOrigin.BaseURL += "?"
	if _, err := NewLocalExecutionClient(invalidOrigin); err == nil {
		t.Fatal("empty query marker passed endpoint validation")
	}
	certificate, err := x509.ParseCertificate(access.Certificate)
	if err != nil || certificate.NotAfter.Before(time.Now().Add(5*365*24*time.Hour)) {
		t.Fatalf("ephemeral endpoint certificate expires during ordinary long-running service: %v, %v", certificate, err)
	}
	client, err := NewLocalExecutionClient(access)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	response, err := testLocalPost(t, client, access.BaseURL, []byte("{"))
	if err != nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("pinned direct request: status=%v error=%v", response, err)
	}
	response.Body.Close()

	wrongToken := access
	wrongToken.Token = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0}, 32))
	unauthorized, err := NewLocalExecutionClient(wrongToken)
	if err != nil {
		t.Fatal(err)
	}
	defer unauthorized.CloseIdleConnections()
	response, err = testLocalPost(t, unauthorized, access.BaseURL, []byte("{"))
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token reached protocol decoder: status=%v error=%v", response, err)
	}
	response.Body.Close()

	if response, err := testLocalPost(t, client, "https://example.invalid", []byte("{}")); err == nil || response != nil || !strings.Contains(err.Error(), "refused another origin") {
		t.Fatalf("client sent a credential to another origin: response=%v error=%v", response, err)
	}

	var spoofedRequests atomic.Int32
	spoof := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spoofedRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer spoof.Close()
	spoofAccess := access
	spoofAccess.BaseURL = spoof.URL
	spoofClient, err := NewLocalExecutionClient(spoofAccess)
	if err != nil {
		t.Fatal(err)
	}
	defer spoofClient.CloseIdleConnections()
	if response, err := testLocalPost(t, spoofClient, spoof.URL, []byte("{}")); err == nil || response != nil || spoofedRequests.Load() != 0 {
		t.Fatalf("replacement listener received authenticated HTTP: response=%v error=%v requests=%d", response, err, spoofedRequests.Load())
	}
}

func TestLocalEndpointUnauthorizedUnfinishedBodyDoesNotHoldShutdown(t *testing.T) {
	_, endpoint := testLocalEndpoint(t)
	access := endpoint.Access()
	u, err := url.Parse(access.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(access.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", u.Host, &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(connection, "POST /next HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer wrong\r\nContent-Type: application/json\r\nContent-Length: 5\r\n\r\n{", u.Host); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unfinished unauthorized body: status=%d", response.StatusCode)
	}
	closed := make(chan error, 1)
	go func() { closed <- endpoint.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		connection.Close()
		<-closed
		t.Fatal("unfinished unauthorized body held endpoint shutdown")
	}
}

func TestLocalEndpointCloseWakesHeldPull(t *testing.T) {
	core, endpoint := testLocalEndpoint(t)
	access := endpoint.Access()
	client, err := NewLocalExecutionClient(access)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	data, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "local"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		status int
		err    error
	}
	done := make(chan outcome, 1)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, access.BaseURL+"/next", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	go func() {
		response, err := client.Do(request)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		response.Body.Close()
		done <- outcome{status: response.StatusCode}
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		core.mu.Lock()
		held, changed := core.pull, core.changed
		core.mu.Unlock()
		if held {
			break
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatal("worker pull was not held")
		}
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.status != http.StatusConflict {
			t.Fatalf("held pull did not receive shutdown response: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held pull did not finish after endpoint close")
	}
	if err := endpoint.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestLocalEndpointDrainsPaidCallbackBeforeShutdown(t *testing.T) {
	core, endpoint := testLocalEndpoint(t)
	access := endpoint.Access()
	client, err := NewLocalExecutionClient(access)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	job := testJob(command.Comment)
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := core.Execute(ctx, job, runnerExecutionCheckpoints{after: func(CallCheckpoint) error {
			close(entered)
			<-release
			return nil
		}})
		done <- outcome{result, err}
	}()
	var assignment httpAssignment
	localTestJSON(t, client, access.BaseURL, "/next", httpPull{Version: ProtocolVersion, ExecutorID: job.Identity.ExecutorID}, &assignment, httpJobBytes)
	if !sameAssignmentJob(job, assignment.Job) {
		t.Fatal("HTTPS pull changed the accepted Job")
	}
	core.mu.Lock()
	tx := core.active
	core.mu.Unlock()
	if tx == nil {
		t.Fatal("HTTPS pull did not admit a transaction")
	}
	call := CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 7, Output: 2}, Complete: true}
	fact := CallCheckpoint{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Call: callMetadata(call)}
	localTestAck(t, client, access.BaseURL, "/before", httpCall{Sequence: 1, Checkpoint: fact}, 1)
	fact.Call = call
	after := make(chan error, 1)
	go func() {
		var ack httpAcknowledgement
		status, err := localJSON(ctx, client, access.BaseURL, "/after", httpCall{Sequence: 2, Checkpoint: fact}, &ack, httpControlBytes)
		if err == nil && (status != http.StatusOK || ack.Sequence != 2 || ack.Failure != nil) {
			err = errors.New("paid callback was not acknowledged")
		}
		after <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("paid callback was not entered")
	}
	closed := make(chan error, 1)
	go func() { closed <- endpoint.Close() }()
	select {
	case <-tx.budget.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint Close did not cancel the accepted transaction")
	}
	select {
	case err := <-closed:
		t.Fatalf("endpoint closed during the paid callback: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-after:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("paid callback acknowledgement did not drain")
	}
	want := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "known paid result", Calls: []CallUsage{call}}
	localTestAck(t, client, access.BaseURL, "/outcome", httpOutcome{Sequence: 3, Result: want, Failure: newHTTPFailure(context.Canceled)}, 3)
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || got.result.Answer != want.Answer || len(got.result.Calls) != 1 {
			t.Fatalf("shutdown lost the known paid result: result=%+v error=%v", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accepted result did not finish")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint did not join after the paid result")
	}
}

func localJSON(ctx context.Context, client *http.Client, baseURL, path string, value, result any, limit int) (int, error) {
	data, err := httpEncode(value, limit)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, httpDecode(response.Body, limit, result)
}

func localTestJSON(t *testing.T, client *http.Client, baseURL, path string, value, result any, limit int) {
	t.Helper()
	status, err := localJSON(t.Context(), client, baseURL, path, value, result, limit)
	if err != nil || status != http.StatusOK {
		t.Fatalf("local HTTPS %s: status=%d error=%v", path, status, err)
	}
}

func localTestAck(t *testing.T, client *http.Client, baseURL, path string, value any, sequence uint64) {
	t.Helper()
	var ack httpAcknowledgement
	localTestJSON(t, client, baseURL, path, value, &ack, httpControlBytes)
	if ack.Sequence != sequence || ack.Failure != nil {
		t.Fatalf("local HTTPS %s acknowledgement: %+v", path, ack)
	}
}
