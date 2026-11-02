package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
)

func TestAdminServerDeniesEveryInitialEntryPoint(t *testing.T) {
	t.Parallel()
	admin, err := startAdminServer(config.AdminConfig{Listen: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin server: %v", err)
		}
	})
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, tc := range []struct {
		name, path string
		upgrade    bool
	}{
		{name: "page", path: "/"},
		{name: "JaWS ping", path: "/jaws/.ping"},
		{name: "websocket callback", path: "/jaws/test-key", upgrade: true},
		{name: "OAuth callback", path: "/auth/callback"},
		{name: "download", path: "/download/report"},
		{name: "export", path: "/export/usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+admin.listener.Addr().String()+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.upgrade {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
	t.Run("general OPTIONS", func(t *testing.T) {
		conn, err := net.DialTimeout("tcp", admin.listener.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, "OPTIONS * HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodOptions})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("OPTIONS * status = %d, want 403", resp.StatusCode)
		}
	})
	t.Run("incomplete POST body", func(t *testing.T) {
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", admin.listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, "POST /jaws/invalid-key HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("incomplete POST status = %d, want 403", resp.StatusCode)
		}
	})
}

func TestAdminServerServesConfiguredTLS(t *testing.T) {
	t.Parallel()
	certFile, keyFile, leaf := remoteTestTLSFiles(t)
	admin, err := startAdminServer(config.AdminConfig{Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin server: %v", err)
		}
	})
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, ForceAttemptHTTP2: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	reused := false
	for i := range 2 {
		ctx := t.Context()
		if i == 1 {
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
			})
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+admin.listener.Addr().String()+"/jaws/.ping", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusForbidden || resp.ProtoMajor != 2 {
			t.Fatalf("HTTPS response = %s %d, want HTTP/2 403", resp.Proto, resp.StatusCode)
		}
	}
	if !reused {
		t.Fatal("denied HTTP/2 request did not reuse its connection")
	}
}

func TestAdminServerReportsListenerFailure(t *testing.T) {
	t.Parallel()
	failed := make(chan struct{})
	admin, err := startAdminServer(config.AdminConfig{Listen: "127.0.0.1:0"}, func() { close(failed) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("listener failure did not invoke onFailure")
	}
	if err := admin.Close(); err == nil {
		t.Fatal("Close did not report the listener failure")
	}
}

func TestAdminGuardCoversJaWSAndLaterRoutes(t *testing.T) {
	t.Parallel()
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(jw.Close)
	mux := http.NewServeMux()
	mux.Handle("/jaws/", jw)
	for _, path := range []string{"/", "/jaws/test-websocket", "/auth/callback", "/download/report", "/export/usage"} {
		pattern := "GET " + path
		if path == "/" {
			pattern = "GET /{$}"
		}
		mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	}
	denied := adminGuard(mux, nil)
	allowed := adminGuard(mux, func(*http.Request) bool { return true })
	for _, tc := range []struct {
		path    string
		upgrade bool
	}{
		{path: "/"},
		{path: "/jaws/.ping"}, // real JaWS route
		{path: "/jaws/test-websocket", upgrade: true},
		{path: "/auth/callback"},
		{path: "/download/report"},
		{path: "/export/usage"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(t.Context())
		if tc.upgrade {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
		}
		allowedResponse := httptest.NewRecorder()
		allowed.ServeHTTP(allowedResponse, req)
		if allowedResponse.Code != http.StatusNoContent {
			t.Errorf("%s: allowed status = %d, want 204", tc.path, allowedResponse.Code)
		}
		deniedResponse := httptest.NewRecorder()
		denied.ServeHTTP(deniedResponse, req)
		if deniedResponse.Code != http.StatusForbidden {
			t.Errorf("%s: denied status = %d, want 403", tc.path, deniedResponse.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/jaws/invalid-key", nil).WithContext(t.Context())
	allowedResponse := httptest.NewRecorder()
	allowed.ServeHTTP(allowedResponse, req)
	if allowedResponse.Code != http.StatusNotFound {
		t.Errorf("POST /jaws/invalid-key: allowed status = %d, want 404", allowedResponse.Code)
	}
	deniedResponse := httptest.NewRecorder()
	denied.ServeHTTP(deniedResponse, req)
	if deniedResponse.Code != http.StatusForbidden {
		t.Errorf("POST /jaws/invalid-key: denied status = %d, want 403", deniedResponse.Code)
	}
}

func TestCheckRejectsInvalidAdminTLS(t *testing.T) {
	certFile, _, _ := remoteTestTLSFiles(t)
	for _, tc := range []struct {
		name, cert, key string
	}{
		{name: "missing files", cert: "missing-cert.pem", key: "missing-key.pem"},
		{name: "invalid key", cert: certFile, key: certFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				BotLogin: "bot", StatePath: filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
				Admin:    &config.AdminConfig{TLSCertFile: tc.cert, TLSKeyFile: tc.key},
				Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
				Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
			}
			code, output := runStartupFixtureWithRun(t, cfg, func() int {
				return runWithGitCheck(func(context.Context, string) error { return nil })
			}, "-check")
			if code != 1 || !strings.Contains(output, "admin TLS preflight:") {
				t.Fatalf("-check code=%d, output=%q; want admin TLS failure", code, output)
			}
		})
	}
}

func TestCronStartupSkipsInvalidAdminTLSPreflight(t *testing.T) {
	cfg := config.Config{
		Main: "codex", BotLogin: "bot", StatePath: filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
		Codex: config.CodexConfig{Model: "test-model", ReasoningEffort: "medium"},
		Admin: &config.AdminConfig{TLSCertFile: "missing-cert.pem", TLSKeyFile: "missing-key.pem"},
		RemoteExecutor: &config.RemoteExecutorConfig{
			ExecutorID: "remote", Listen: "127.0.0.1:8443",
			TLSCertFile: "missing-remote-cert.pem", TLSKeyFile: "missing-remote-key.pem",
			Token: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
	}
	gitChecks := 0
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error {
			gitChecks++
			return nil
		})
	})
	if code != 1 || gitChecks != 1 || !strings.Contains(output, "remote TLS preflight:") ||
		strings.Contains(output, "admin TLS preflight:") {
		t.Fatalf("cron startup was blocked by admin TLS: code=%d git_checks=%d output=%q", code, gitChecks, output)
	}
}
