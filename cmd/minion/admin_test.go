package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func adminTLSClient(t *testing.T, leaf *x509.Certificate) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, ForceAttemptHTTP2: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestAdminServerWithoutSignInServesHomeAndSetupPage(t *testing.T) {
	t.Parallel()
	admin, err := startAdminServer(t.Context(), config.AdminConfig{Listen: "127.0.0.1:0", Administrators: []config.AdminAccount{{Username: "admin"}}}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin server: %v", err)
		}
	})
	_, port, err := net.SplitHostPort(admin.listener.Addr().String())
	if err != nil || admin.entryURL != "http://localhost:"+port {
		t.Fatalf("HTTP listener URL = %q, port=%q: %v", admin.entryURL, port, err)
	}
	if admin.signInEnabled {
		t.Fatal("admin sign-in enabled without a password or OAuth provider")
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	home, err := client.Get("http://" + admin.listener.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = home.Body.Close() })
	homeBody, err := io.ReadAll(home.Body)
	if err != nil {
		t.Fatal(err)
	}
	if home.StatusCode != http.StatusOK || len(home.Cookies()) != 1 ||
		!strings.Contains(string(homeBody), "The admin password is not set") ||
		strings.Contains(string(homeBody), `href="/admin/login"`) || strings.Contains(string(homeBody), `href="/auth/login"`) {
		t.Fatalf("unconfigured home: status=%d cookies=%v body=%q", home.StatusCode, home.Cookies(), homeBody)
	}
	setup, err := client.Get("http://" + admin.listener.Addr().String() + "/admin/login")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setup.Body.Close() })
	setupBody, err := io.ReadAll(setup.Body)
	if err != nil {
		t.Fatal(err)
	}
	if setup.StatusCode != http.StatusOK || !strings.Contains(string(setupBody), "The admin password is not set") {
		t.Fatalf("unconfigured setup page: status=%d body=%q", setup.StatusCode, setupBody)
	}
	for _, path := range []string{"/jaws/.ping", bootstrapAssetPath(t, string(homeBody), ".css")} {
		response, err := client.Get("http://" + admin.listener.Addr().String() + path)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		want := http.StatusOK
		if path == "/jaws/.ping" {
			want = http.StatusNoContent
		}
		if response.StatusCode != want {
			t.Fatalf("home asset %s: status=%d, want %d", path, response.StatusCode, want)
		}
	}
	for _, tc := range []struct {
		name, path string
		upgrade    bool
	}{
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

func TestAdminServerServesSignedInUserPreferences(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	choice := config.RoleModelConfig{Model: "approved-main", Effort: "low"}
	criticChoice := config.RoleModelConfig{Model: "approved-critic", Effort: "high"}
	cfg := config.AdminConfig{Listen: "127.0.0.1:0",
		GitHubOAuth:      &config.OAuthClient{ClientID: "id", ClientSecret: "secret", RedirectURL: "http://127.0.0.1/auth/github/callback"},
		UserModelChoices: &config.UserModelChoices{Main: []config.RoleModelConfig{choice}, Critic: []config.RoleModelConfig{criticChoice}}}
	admin, err := startAdminServer(t.Context(), cfg, nil, statePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Error(err)
		}
	})
	if admin.preferences == nil {
		t.Fatal("admin server did not share the account preference store")
	}
	if !admin.signInEnabled {
		t.Fatal("OAuth-only configuration did not enable sign-in")
	}
	base := "http://" + admin.listener.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path string, form url.Values, cookie *http.Cookie) *http.Response {
		t.Helper()
		body := ""
		if form != nil {
			body = form.Encode()
		}
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if method == http.MethodPost {
			req.Header.Set("Origin", base)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := request(http.MethodGet, "/auth/preferences", nil, nil); response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/auth/login" {
		t.Fatalf("guest preferences status=%d location=%q", response.StatusCode, response.Header.Get("Location"))
	}
	sessionResponse := httptest.NewRecorder()
	sessionRequest := httptest.NewRequest(http.MethodGet, base+"/auth/login", nil)
	sessionRequest.RemoteAddr = "127.0.0.1:12345"
	session := admin.jaws.NewSession(sessionResponse, sessionRequest)
	if session == nil {
		t.Fatal("could not create a signed-in user session")
	}
	accountKey := (userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key()
	session.Set("oidc_claims.github", map[string]any{"iss": "https://github.com", "sub": "42"})
	session.Set("oauth2authexpiry.github", time.Now().Add(time.Hour))
	cookies := sessionResponse.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("signed-in session cookies=%v", cookies)
	}
	cookie := cookies[0]
	formIdentity := preferenceFormIdentity(accountKey)
	selected := url.Values{"main": {preferenceChoiceValue(choice)}, "critic": {preferenceChoiceValue(criticChoice)}, "identity": {formIdentity}}
	if response := request(http.MethodGet, "/auth/preferences", nil, cookie); response.StatusCode != http.StatusOK ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("signed-in preferences status=%d", response.StatusCode)
	}
	if response := request(http.MethodPost, "/auth/preferences", selected, cookie); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("save preferences status=%d", response.StatusCode)
	}
	if got, err := admin.preferences.preferencesForGitHubID(42); err != nil || got != (config.UserModelPreferences{Main: choice, Critic: criticChoice}) {
		t.Fatalf("monitor-visible preference: %+v, %v", got, err)
	}
	if response := request(http.MethodPost, "/auth/preferences", selected, cookie); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("identical preferences status=%d", response.StatusCode)
	}
	for _, invalid := range []struct {
		name string
		form url.Values
	}{
		{"unapproved main", url.Values{"main": {preferenceChoiceValue(config.RoleModelConfig{Model: "unknown", Effort: "low"})}, "critic": selected["critic"], "identity": {formIdentity}}},
		{"main choice as critic", url.Values{"main": selected["main"], "critic": {preferenceChoiceValue(choice)}, "identity": {formIdentity}}},
		{"missing critic", url.Values{"main": selected["main"], "identity": {formIdentity}}},
		{"extra field", url.Values{"main": selected["main"], "critic": selected["critic"], "identity": {formIdentity}, "extra": {"unexpected"}}},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			if response := request(http.MethodPost, "/auth/preferences", invalid.form, cookie); response.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid preferences status=%d", response.StatusCode)
			}
			if got, err := admin.preferences.preferencesForGitHubID(42); err != nil || got != (config.UserModelPreferences{Main: choice, Critic: criticChoice}) {
				t.Fatalf("invalid form changed saved preferences: %+v, %v", got, err)
			}
		})
	}
	// With every approved choice withdrawn, the saved choice must still be removable.
	cfg.UserModelChoices.Main = nil
	cfg.UserModelChoices.Critic = nil
	pageRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/auth/preferences", nil)
	if err != nil {
		t.Fatal(err)
	}
	pageRequest.AddCookie(cookie)
	page, err := client.Do(pageRequest)
	if err != nil {
		t.Fatal(err)
	}
	pageBody, err := io.ReadAll(page.Body)
	_ = page.Body.Close()
	if err != nil || page.StatusCode != http.StatusOK ||
		!strings.Contains(string(pageBody), `name="main"`) || !strings.Contains(string(pageBody), `name="critic"`) ||
		!strings.Contains(string(pageBody), `value="default"`) || !strings.Contains(string(pageBody), "Saved main choice is unavailable") ||
		!strings.Contains(string(pageBody), "Saved critic choice is unavailable") {
		t.Fatalf("withdrawn choice has no reset form: status=%d body=%q error=%v", page.StatusCode, pageBody, err)
	}
	reset := url.Values{"main": {"default"}, "critic": {"default"}, "identity": {formIdentity}}
	admin.preferences.mu.Lock()
	if admin.preferences.lastPreferenceWrite.IsZero() {
		admin.preferences.mu.Unlock()
		t.Fatal("HTTP preference save did not record its write limit")
	}
	admin.preferences.lastPreferenceWrite = time.Now()
	admin.preferences.mu.Unlock()
	if response := request(http.MethodPost, "/auth/preferences", reset, cookie); response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "2" {
		t.Fatalf("rapid changed preference status=%d retry=%q", response.StatusCode, response.Header.Get("Retry-After"))
	}
	admin.preferences.mu.Lock()
	admin.preferences.lastPreferenceWrite = time.Now().Add(-2 * preferenceWriteInterval)
	admin.preferences.mu.Unlock()
	if response := request(http.MethodPost, "/auth/preferences", reset, cookie); response.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset preferences status=%d", response.StatusCode)
	}
	reopened, err := openUserLinks(statePath + ".link")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.preferencesForGitHubID(42); err != nil || got != (config.UserModelPreferences{}) || len(reopened.preferences) != 0 {
		t.Fatalf("reset preference remained on disk: %+v, %v", got, err)
	}
	admin.preferences.mu.Lock()
	admin.preferences.bad = errors.New("synthetic storage fault")
	admin.preferences.mu.Unlock()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if response := request(method, "/auth/preferences", reset, cookie); response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("storage fault %s status=%d", method, response.StatusCode)
		}
	}
}

func TestOrchestrationWiresPreferencesOnlyWhenConfigured(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, loop := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%t/loop=%t", enabled, loop), func(t *testing.T) {
				statePath := filepath.Join(t.TempDir(), "state.json")
				store, err := state.Open(statePath, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				admin := &config.AdminConfig{Listen: freeAdminListen(t), Administrators: []config.AdminAccount{{Username: "admin"}},
					GitHubOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret", RedirectURL: "http://127.0.0.1/auth/github/callback"}}
				admin.GitHubOAuth.RedirectURL = "http://" + admin.Listen + "/auth/github/callback"
				choice := config.RoleModelConfig{Model: "approved", Effort: "low"}
				if enabled {
					admin.UserModelChoices = &config.UserModelChoices{Main: []config.RoleModelConfig{choice}}
					links, err := openUserLinks(statePath + ".link")
					if err != nil {
						t.Fatal(err)
					}
					if err := links.setPreferences((userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key(),
						config.UserModelPreferences{Main: choice}); err != nil {
						t.Fatal(err)
					}
				}
				cfg, path := writeModeConfig(t, config.Config{BotLogin: "bot", StatePath: statePath, Admin: admin,
					Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
					Repositories: []config.Repository{{Name: "o/r", ID: 1, OwnerID: 2}}})
				watcher := &monitor.Monitor{Config: cfg, ConfigPath: path}
				if code := runOrchestrateMode(t.Context(), nil, watcher, store, loop, log.New(io.Discard, "", 0), "", time.Second); code != 1 {
					t.Fatalf("missing owner did not stop startup: %d", code)
				}
				if got := watcher.UserPreferences != nil; got != enabled {
					t.Fatalf("preference lookup wired=%t, want %t", got, enabled)
				}
				if enabled {
					got, err := watcher.UserPreferences(42)
					if err != nil || got.Main != choice {
						t.Fatalf("preference lookup lost persisted choice: %+v, %v", got, err)
					}
				}
			})
		}
	}
}

func TestCertifiedListenURL(t *testing.T) {
	for _, test := range []struct {
		address, listen, want string
		certificateNames      []string
		certificateIPs        []net.IP
	}{
		{address: "127.0.0.1:8080", want: "https://admin.example:8080", certificateNames: []string{"admin.example"}},
		{address: "192.0.2.8:8080", want: "https://admin.example:8080", certificateNames: []string{"admin.example"}},
		{address: "127.0.0.1:8080", listen: "minion.example:8080", want: "https://minion.example:8080", certificateNames: []string{"admin.example", "minion.example"}},
		{address: "192.0.2.8:8080", listen: "minion.example.com:8080", want: "https://minion.example.com:8080", certificateNames: []string{"*.example.com"}},
		{address: "192.0.2.8:8080", listen: "minion.example", want: "https://minion.example:8080", certificateNames: []string{"other.example", "minion.example"}},
		{address: "192.0.2.8:8080", listen: "minion.example.com", want: "https://minion.example.com:8080", certificateNames: []string{"*.example.com"}},
		{address: "0.0.0.0:8080", want: "https://admin.example:8080", certificateNames: []string{"admin.example"}},
		{address: "[::1]:8080", want: "https://admin.example:8080", certificateNames: []string{"admin.example"}},
		{address: "127.0.0.1:8080", want: "https://127.0.0.1:8080", certificateIPs: []net.IP{net.ParseIP("127.0.0.1")}},
		{address: "127.0.0.1:8080", want: "https://127.0.0.1:8080", certificateNames: []string{"admin.example"}, certificateIPs: []net.IP{net.ParseIP("127.0.0.1")}},
		{address: "[::1]:8080", want: "https://localhost:8080", certificateNames: []string{"admin.example", "localhost"}},
		{address: "127.0.0.1:8080", want: "https://localhost:8080", certificateNames: []string{"admin.example", "localhost"}},
		{address: "127.0.0.2:8080", want: "https://admin.example:8080", certificateNames: []string{"admin.example", "localhost"}},
		{address: "127.0.0.2:8080", certificateNames: []string{"localhost"}},
		{address: "127.0.0.2:8080", certificateIPs: []net.IP{net.ParseIP("127.0.0.1")}},
		{address: "[::]:8080", listen: ":8080", want: "https://localhost:8080", certificateNames: []string{"localhost"}},
		{address: "[fe80::1%eth0]:8443", listen: "[fe80::1%eth0]:8443", certificateNames: []string{"localhost"}},
		{address: "0.0.0.0:8080", want: "https://127.0.0.1:8080", certificateIPs: []net.IP{net.ParseIP("127.0.0.1")}},
		{address: "0.0.0.0:8080", certificateIPs: []net.IP{net.ParseIP("0.0.0.0")}},
		{address: "[::]:8080", certificateIPs: []net.IP{net.ParseIP("::")}},
		{address: "0.0.0.0:8080", want: "https://127.0.0.1:8080", certificateIPs: []net.IP{net.ParseIP("0.0.0.0"), net.ParseIP("127.0.0.1")}},
		{address: "192.0.2.8:8080", want: "https://192.0.2.8:8080", certificateIPs: []net.IP{net.ParseIP("192.0.2.8")}},
		{address: "192.0.2.8:8080", want: "https://192.0.2.8:8080", certificateNames: []string{"other.example"}, certificateIPs: []net.IP{net.ParseIP("192.0.2.8")}},
		{address: "127.0.0.1:8080"},
		{address: "127.0.0.1:8080", certificateNames: []string{"*.example"}},
	} {
		certificate := &tls.Certificate{Leaf: &x509.Certificate{DNSNames: test.certificateNames, IPAddresses: test.certificateIPs}}
		if got := certifiedListenURL(test.address, certificate, config.AdminConfig{Listen: test.listen}); got != test.want {
			t.Errorf("TLS URL for %s = %q, want %q", test.address, got, test.want)
		}
	}
	if got := certifiedListenURL("127.0.0.1:8080", &tls.Certificate{}, config.AdminConfig{}); got != "" {
		t.Errorf("URL without parsed certificate = %q, want none", got)
	}
	callback := config.AdminConfig{GitHubOAuth: &config.OAuthClient{RedirectURL: "https://login.example:8443/auth/github/callback"}}
	certificate := &tls.Certificate{Leaf: &x509.Certificate{DNSNames: []string{"first.example", "login.example"}}}
	if got := certifiedListenURL("127.0.0.1:8080", certificate, callback); got != "https://login.example:8443" {
		t.Errorf("OAuth URL = %q, want callback origin", got)
	}
	gitlab := config.AdminConfig{GitLabOAuth: &config.OAuthClient{RedirectURL: "https://login.example:8443/auth/gitlab/callback"}}
	if got := certifiedListenURL("127.0.0.1:8080", certificate, gitlab); got != "https://login.example:8443" {
		t.Errorf("GitLab OAuth URL = %q, want callback origin", got)
	}
	certificate.Leaf.DNSNames = []string{"first.example"}
	if got := certifiedListenURL("127.0.0.1:8080", certificate, callback); got != "" {
		t.Errorf("uncovered OAuth URL = %q, want none", got)
	}
}

func TestAdminCertificateValidity(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name                string
		notBefore, notAfter time.Time
		wantError           bool
	}{
		{"current", now.Add(-time.Hour), now.Add(time.Hour), false},
		{"expired", now.Add(-2 * time.Hour), now.Add(-time.Hour), true},
		{"not yet valid", now.Add(time.Hour), now.Add(2 * time.Hour), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			certificate := &tls.Certificate{Leaf: &x509.Certificate{DNSNames: []string{"admin.example"}, NotBefore: test.notBefore, NotAfter: test.notAfter}}
			err := checkAdminCertificateURL(certificate, config.AdminConfig{WebservListenURL: "https://admin.example"})
			if (err != nil) != test.wantError {
				t.Fatalf("certificate validity error = %v, want error %t", err, test.wantError)
			}
			if err != nil && !strings.Contains(err.Error(), "not currently valid") {
				t.Fatalf("unexpected certificate validity error: %v", err)
			}
		})
	}
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
	_, port, err := net.SplitHostPort(admin.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://" + net.JoinHostPort(leaf.DNSNames[0], port); admin.entryURL != want {
		t.Fatalf("HTTPS listener URL = %q, want %q", admin.entryURL, want)
	}
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
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+admin.listener.Addr().String()+"/download/report", nil)
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
		t.Fatal("HTTPS request did not reuse its connection")
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
	if want := "https://" + admin.listener.Addr().String(); admin.entryURL != want {
		t.Fatalf("fallback HTTPS listener URL = %q, want %q", admin.entryURL, want)
	}
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
	if resp.StatusCode != http.StatusNoContent || resp.ProtoMajor != 1 {
		t.Fatalf("fallback HTTPS response = %s %d, want HTTP/1.1 204", resp.Proto, resp.StatusCode)
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
	if resp.StatusCode != http.StatusNoContent || resp.ProtoMajor != 1 {
		t.Fatalf("HTTPS response = %s %d, want HTTP/1.1 204", resp.Proto, resp.StatusCode)
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
				Admin:    &config.AdminConfig{TLSCertFile: tc.cert, TLSKeyFile: tc.key, Administrators: []config.AdminAccount{{Username: "admin"}}},
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
		Admin: &config.AdminConfig{TLSCertFile: "missing-cert.pem", TLSKeyFile: "missing-key.pem", Administrators: []config.AdminAccount{{Username: "admin"}}},
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
	}, "-oneshot")
	if code != 1 || gitChecks != 1 || !strings.Contains(output, "GitHub authentication:") ||
		strings.Contains(output, "admin TLS preflight:") || strings.Contains(output, "remote TLS preflight:") {
		t.Fatalf("cron startup was blocked by admin TLS: code=%d git_checks=%d output=%q", code, gitChecks, output)
	}
}
