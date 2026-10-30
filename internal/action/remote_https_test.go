package action

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testRemoteCertificate(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	return server.TLS.Certificates[0], server.Certificate()
}

func testRemotePost(t *testing.T, client *http.Client, address string) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address+"/next", strings.NewReader("{"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, response.Body)
	return response.StatusCode, err
}

func TestRemoteExecutionRequiresTLSAndAuthorizesBeforeBody(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	certificate, leaf := testRemoteCertificate(t)
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := ListenRemoteExecution(executor, "127.0.0.1:0", certificate, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := endpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	origin := "https://" + endpoint.Addr().String()
	client, err := NewRemoteExecutionClient(origin, token)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	endpoint.server.ErrorLog = log.New(io.Discard, "", 0)
	if status, err := testRemotePost(t, client, origin); status != 0 || err == nil {
		t.Fatalf("untrusted server certificate accepted: status=%d error=%v", status, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	client.Transport.(*authenticatedExecutionTransport).TLSClientConfig.RootCAs = roots
	if status, err := testRemotePost(t, client, origin); status != http.StatusBadRequest || err != nil {
		t.Fatalf("authenticated request did not reach bounded decoder: status=%d error=%v", status, err)
	}
	wrongToken := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	unauthorized, err := NewRemoteExecutionClient(origin, wrongToken)
	if err != nil {
		t.Fatal(err)
	}
	defer unauthorized.CloseIdleConnections()
	unauthorized.Transport.(*authenticatedExecutionTransport).TLSClientConfig.RootCAs = roots
	if status, err := testRemotePost(t, unauthorized, origin); status != http.StatusUnauthorized || err != nil {
		t.Fatalf("wrong token reached decoder: status=%d error=%v", status, err)
	}
	if status, err := testRemotePost(t, client, "https://example.invalid"); status != 0 || err == nil || !strings.Contains(err.Error(), "refused another origin") {
		t.Fatalf("client sent token to another origin: status=%d error=%v", status, err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatalf("repeat close: %v", err)
	}
	select {
	case <-endpoint.Done():
	default:
		t.Fatal("closed listener did not close Done")
	}
}

func TestRemoteEndpointDoneClosesOnListenerFailure(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	certificate, _ := testRemoteCertificate(t)
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := ListenRemoteExecution(executor, "127.0.0.1:0", certificate, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	select {
	case <-endpoint.Done():
		t.Fatal("listener stopped before failure")
	default:
	}
	if err := endpoint.listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-endpoint.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("listener failure did not close Done")
	}
	if err := endpoint.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener failure was not returned by Close: %v", err)
	}
}

func TestRemoteClientBoundsConnectionSetupOnly(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	client, err := NewRemoteExecutionClient("https://example.com", token)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	transport := client.Transport.(*authenticatedExecutionTransport).Transport
	if transport.DialContext == nil || transport.TLSHandshakeTimeout != httpControlTimeout {
		t.Fatal("remote client does not bound TCP dial and TLS handshake setup")
	}
	if client.Timeout != 0 || transport.ResponseHeaderTimeout != 0 {
		t.Fatal("remote client bounds the held assignment wait")
	}
}

func TestRemoteExecutionRejectsInvalidConfiguration(t *testing.T) {
	certificate, _ := testRemoteCertificate(t)
	executor, err := NewHTTPExecutor("remote")
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	if _, err := ListenRemoteExecution(executor, "127.0.0.1:0", certificate, "short"); err == nil {
		t.Fatal("short token accepted")
	}
	for _, origin := range []string{"http://example.com", "https://user@example.com", "https://example.com/path", "https://example.com?", "https://example.com#fragment"} {
		if _, err := NewRemoteExecutionClient(origin, token); err == nil {
			t.Fatalf("invalid origin %q accepted", origin)
		}
	}
	if _, err := NewRemoteExecutionClient("https://example.com", "short"); err == nil {
		t.Fatal("short client token accepted")
	}
}
