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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
)

func adminTLSClient(t *testing.T, leaf *x509.Certificate) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, ForceAttemptHTTP2: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestAdminServerDeniesEveryInitialEntryPoint(t *testing.T) {
	t.Parallel()
	admin, err := startAdminServer(t.Context(), config.AdminConfig{Listen: "127.0.0.1:0"}, nil, "", nil)
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
			if resp.Header.Get("Content-Security-Policy") == "" {
				t.Fatal("denied response lacks Content-Security-Policy")
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
		if resp.StatusCode != http.StatusForbidden || !resp.Close {
			t.Fatalf("incomplete POST status = %d close=%t, want 403 and close", resp.StatusCode, resp.Close)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("server kept incomplete POST connection open: %v", err)
		}
	})
}

func TestAdminServerServesConfiguredTLS(t *testing.T) {
	t.Parallel()
	certFile, keyFile, leaf := remoteTestTLSFiles(t)
	admin, err := startAdminServer(t.Context(), config.AdminConfig{Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin server: %v", err)
		}
	})
	client := adminTLSClient(t, leaf)
	reused := false
	wantProto := 2
	serverH2Off, clientH2Off := false, false
	for _, setting := range strings.Split(os.Getenv("GODEBUG"), ",") {
		if value, ok := strings.CutPrefix(setting, "http2server="); ok {
			serverH2Off = value == "0"
		}
		if value, ok := strings.CutPrefix(setting, "http2client="); ok {
			clientH2Off = value == "0"
		}
	}
	if serverH2Off || clientH2Off {
		wantProto = 1
	}
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
		if resp.StatusCode != http.StatusForbidden || resp.ProtoMajor != wantProto {
			t.Fatalf("HTTPS response = %s %d, want HTTP/%d 403", resp.Proto, resp.StatusCode, wantProto)
		}
		if resp.Header.Get("Strict-Transport-Security") != "" {
			t.Fatal("admin response sets host-wide HSTS")
		}
	}
	if !reused {
		t.Fatal("denied HTTPS request did not reuse its connection")
	}
}

func TestAdminServerServesLiteralDollarTLSPath(t *testing.T) {
	t.Parallel()
	certFile, keyFile, leaf := remoteTestTLSFiles(t)
	certDir := filepath.Join(t.TempDir(), "literal$certs")
	if err := os.Mkdir(certDir, 0700); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(certDir, filepath.Base(certFile))
	if err := os.WriteFile(certFile, contents, 0600); err != nil {
		t.Fatal(err)
	}
	admin, err := startAdminServer(t.Context(), config.AdminConfig{Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin server: %v", err)
		}
	})
	client := adminTLSClient(t, leaf)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+admin.listener.Addr().String()+"/jaws/.ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.ProtoMajor != 1 {
		t.Fatalf("fallback HTTPS response = %s %d, want HTTP/1.1 403", resp.Proto, resp.StatusCode)
	}
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Fatal("fallback admin response sets host-wide HSTS")
	}
}

func TestAdminServerServesNonCleanSymlinkTLSPath(t *testing.T) {
	t.Parallel()
	if filepath.Separator == '\\' {
		t.Skip("symlink traversal semantics differ on Windows")
	}
	certFile, keyFile, leaf := remoteTestTLSFiles(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(filepath.Join(target, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	cert, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "cert.pem"), cert, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target, "sub"), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	certFile = root + "/link/../cert.pem"
	if _, err := loadTLSCertificate("admin", certFile, keyFile); err != nil {
		t.Fatalf("TLS preflight: %v", err)
	}
	admin, err := startAdminServer(t.Context(), config.AdminConfig{Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin server: %v", err)
		}
	})
	client := adminTLSClient(t, leaf)
	resp, err := client.Get("https://" + admin.listener.Addr().String() + "/jaws/.ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.ProtoMajor != 1 {
		t.Fatalf("HTTPS response = %s %d, want HTTP/1.1 403", resp.Proto, resp.StatusCode)
	}
}

func TestAdminServerReportsListenerFailure(t *testing.T) {
	t.Parallel()
	failed := make(chan struct{})
	admin, err := startAdminServer(t.Context(), config.AdminConfig{Listen: "127.0.0.1:0"}, nil, "", func() { close(failed) })
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

func TestAdminServerStopsWithParentContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	failed := make(chan struct{}, 1)
	admin, err := startAdminServer(ctx, config.AdminConfig{Listen: "127.0.0.1:0"}, nil, "", func() { failed <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	cancel()
	select {
	case <-admin.serveDone:
	case <-time.After(3 * time.Second):
		t.Fatal("admin server did not stop after parent cancellation")
	}
	if admin.serveErr != nil {
		t.Fatalf("parent cancellation reported as serving failure: %v", admin.serveErr)
	}
	select {
	case <-failed:
		t.Fatal("parent cancellation invoked serving failure callback")
	default:
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
	denied := jw.SecureHeadersMiddleware(adminGuard(mux, nil))
	allowed := jw.SecureHeadersMiddleware(adminGuard(mux, func(*http.Request) bool { return true }))
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
		req.TLS = &tls.ConnectionState{}
		if tc.upgrade {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
		}
		allowedResponse := httptest.NewRecorder()
		allowed.ServeHTTP(allowedResponse, req)
		if allowedResponse.Code != http.StatusNoContent {
			t.Errorf("%s: allowed status = %d, want 204", tc.path, allowedResponse.Code)
		}
		if allowedResponse.Header().Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: allowed response sets host-wide HSTS", tc.path)
		}
		deniedResponse := httptest.NewRecorder()
		denied.ServeHTTP(deniedResponse, req)
		if deniedResponse.Code != http.StatusForbidden {
			t.Errorf("%s: denied status = %d, want 403", tc.path, deniedResponse.Code)
		}
		if deniedResponse.Header().Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: denied response sets host-wide HSTS", tc.path)
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
	isolateHostPaths(t, t.TempDir())
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	certFile, keyFile, _ := remoteTestTLSFiles(t)
	cfg := config.Config{
		Main: "codex", BotLogin: "bot", StatePath: filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir(),
		Codex: config.CodexConfig{Model: "test-model", ReasoningEffort: "medium"},
		Admin: &config.AdminConfig{TLSCertFile: "missing-cert.pem", TLSKeyFile: "missing-key.pem"},
		RemoteExecutor: &config.RemoteExecutorConfig{
			ExecutorID: "remote", Listen: "127.0.0.1:8443",
			TLSCertFile: certFile, TLSKeyFile: keyFile,
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
	if code != 1 || gitChecks != 1 || !strings.Contains(output, "GitHub authentication:") ||
		strings.Contains(output, "admin TLS preflight:") || strings.Contains(output, "remote TLS preflight:") {
		t.Fatalf("cron startup was blocked by admin TLS: code=%d git_checks=%d output=%q", code, gitChecks, output)
	}
}
