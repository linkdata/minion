package action

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	t.Cleanup(func() { _ = endpoint.Close() })
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
