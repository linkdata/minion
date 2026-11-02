package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
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
	client := &http.Client{Timeout: 5 * time.Second}
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
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}, Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+admin.listener.Addr().String()+"/jaws/.ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("HTTPS status = %d, want 403", resp.StatusCode)
	}
}

func TestAdminGuardCoversJaWSAndLaterRoutes(t *testing.T) {
	t.Parallel()
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); jw.Serve() }()
	t.Cleanup(func() { jw.Close(); <-done })
	mux := http.NewServeMux()
	mux.Handle("GET /jaws/", jw)
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
