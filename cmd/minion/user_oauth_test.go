package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
)

type oauthTransportFunc func(*http.Request) (*http.Response, error)

func (fn oauthTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func oauthTestResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func oauthRequest(handler http.Handler, method, path, origin string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://admin.example"+path, nil)
	r.TLS = &tls.ConnectionState{}
	r.RemoteAddr = "192.0.2.7:12345"
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	for _, cookie := range cookies {
		if cookie != nil {
			r.AddCookie(cookie)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func oauthCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response did not set %q cookie", name)
	return nil
}

func oauthStart(t *testing.T, handler http.Handler, method, path string, session *http.Cookie) (*http.Cookie, url.Values) {
	t.Helper()
	response := oauthRequest(handler, method, path, "https://admin.example", session)
	var target string
	if method == http.MethodPost {
		body := response.Body.String()
		_, after, found := strings.Cut(body, `<a href="`)
		if response.Code != http.StatusOK || response.Header().Get("Location") != "" ||
			response.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
			response.Header().Get("Cache-Control") != "no-store" ||
			!strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action 'self'") || !found {
			t.Fatalf("link handoff: status=%d headers=%v body=%q", response.Code, response.Header(), body)
		}
		escaped, _, found := strings.Cut(after, `"`)
		if !found || !strings.Contains(body, `http-equiv="refresh" content="0;url=`+escaped+`"`) {
			t.Fatalf("link handoff refresh and fallback differ: %q", body)
		}
		target = html.UnescapeString(escaped)
	} else {
		if response.Code != http.StatusSeeOther {
			t.Fatalf("start %s: status=%d body=%q", path, response.Code, response.Body.String())
		}
		target = response.Header().Get("Location")
	}
	stateCookie := oauthCookie(t, response, oauthStateCookie)
	if !stateCookie.Secure || !stateCookie.HttpOnly || stateCookie.SameSite != http.SameSiteLaxMode || stateCookie.Path != "/" || stateCookie.Domain != "" {
		t.Fatalf("state cookie attributes: %+v", stateCookie)
	}
	location, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	query := location.Query()
	wantAuthorize, wantScope := "https://github.com/login/oauth/authorize", "read:user"
	if strings.HasSuffix(path, "gitlab") {
		wantAuthorize, wantScope = "https://gitlab.example/oauth/authorize", "read_user"
	}
	if location.Scheme+"://"+location.Host+location.Path != wantAuthorize || query.Get("state") != stateCookie.Value ||
		query.Get("client_id") == "" || query.Get("scope") != wantScope || query.Get("response_type") != "code" ||
		query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" ||
		query.Get("redirect_uri") != "https://admin.example/auth/callback" {
		t.Fatalf("OAuth authorization URL %q or parameters %v", target, query)
	}
	return stateCookie, query
}

func TestOAuthRedirectMatchesBrowserDefaultPort(t *testing.T) {
	for _, test := range []struct {
		name, redirectURL, requestURL string
		want                          bool
	}{
		{"HTTPS default port", "https://admin.example:443/auth/callback", "https://admin.example/auth/callback", true},
		{"HTTPS request default port", "https://admin.example/auth/callback", "https://admin.example:443/auth/callback", true},
		{"HTTPS padded default port", "https://admin.example:0443/auth/callback", "https://admin.example/auth/callback", true},
		{"loopback HTTP default port", "http://127.0.0.1:80/auth/callback", "http://127.0.0.1/auth/callback", true},
		{"loopback HTTP request default port", "http://127.0.0.1/auth/callback", "http://127.0.0.1:80/auth/callback", true},
		{"IPv6 request default port", "https://[::1]/auth/callback", "https://[::1]:443/auth/callback", true},
		{"other host", "https://admin.example:443/auth/callback", "https://other.example/auth/callback", false},
		{"other port", "https://admin.example:443/auth/callback", "https://admin.example:8443/auth/callback", false},
		{"other scheme", "https://admin.example:443/auth/callback", "http://admin.example/auth/callback", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, test.requestURL, nil)
			if strings.HasPrefix(test.requestURL, "https://") {
				r.TLS = &tls.ConnectionState{}
			}
			if got := (oauthProvider{redirectURL: test.redirectURL}).matchesRequest(r); got != test.want {
				t.Fatalf("redirect %q with request host %q: matched=%t, want %t", test.redirectURL, r.Host, got, test.want)
			}
		})
	}
	for _, host := range []string{"admin.example:0", "admin.example:65536", "admin.example:invalid", "admin.example:", "other.example@admin.example"} {
		t.Run("invalid request host "+host, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "https://admin.example/auth/callback", nil)
			r.TLS = &tls.ConnectionState{}
			r.Host = host
			if (oauthProvider{redirectURL: "https://admin.example/auth/callback"}).matchesRequest(r) {
				t.Fatalf("accepted invalid request host %q", host)
			}
		})
	}
}

func TestOAuthGitLabPortSpellingKeepsLinksAcrossRestart(t *testing.T) {
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()
	statePath := filepath.Join(t.TempDir(), "state.json")
	githubKey := (userIdentity{Provider: "github", Host: "github.com", ID: 9}).key()
	for i, port := range []string{":08443", ":8443"} {
		cfg := config.Config{BotLogin: "bot", Admin: &config.AdminConfig{
			TLSCertFile: "cert.pem", TLSKeyFile: "key.pem",
			GitLabOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret",
				RedirectURL: "https://admin.example/auth/callback", BaseURL: "https://gitlab.example" + port}},
			Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
		if err := cfg.Validate(t.TempDir()); err != nil {
			t.Fatal(err)
		}
		oauth, err := newOAuthAccess(jw, *cfg.Admin, statePath, nil)
		if err != nil {
			t.Fatal(err)
		}
		gitlabKey := (userIdentity{Provider: "gitlab", Host: oauth.providers["gitlab"].host, ID: 7}).key()
		if i == 0 {
			if err := oauth.links.link(githubKey, gitlabKey); err != nil {
				t.Fatal(err)
			}
		} else if oauth.links.account(gitlabKey) != githubKey {
			t.Fatalf("restarted link for %q did not survive port spelling change", gitlabKey)
		}
	}
}

func TestOAuthCallbackReleasesPerIPBudget(t *testing.T) {
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/callback"}}
	client := &http.Client{Transport: oauthTransportFunc(func(*http.Request) (*http.Response, error) {
		return oauthTestResponse(`{}`), nil
	})}
	a, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), client)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, cfg, nil, a)
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, remote string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "https://admin.example"+path, nil)
		r.TLS = &tls.ConnectionState{}
		r.RemoteAddr = remote
		return r
	}
	for i := range 24 {
		remote := fmt.Sprintf("192.0.2.%d:1234", i+1)
		start := httptest.NewRecorder()
		handler.ServeHTTP(start, request("/auth/github", remote))
		if start.Code != http.StatusSeeOther {
			t.Fatalf("IP %d sign-in start: %d", i, start.Code)
		}
		state := oauthCookie(t, start, oauthStateCookie)
		callback := request("/auth/callback?state="+state.Value+"&code=invalid", remote)
		callback.AddCookie(state)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, callback)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("IP %d failed provider exchange: %d", i, response.Code)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inflight != 0 || len(a.byIP) != 0 {
		t.Fatalf("completed exchanges retained in-flight budgets: global=%d IPs=%d", a.inflight, len(a.byIP))
	}
}

func TestOAuthSignInLinkAndRevocation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config.AdminConfig{
		Administrators: []config.AdminAccount{testAdminAccount()},
		GitHubOAuth:    &config.OAuthClient{ClientID: "github-id", ClientSecret: "github-secret", RedirectURL: "https://admin.example/auth/callback"},
		GitLabOAuth:    &config.OAuthClient{ClientID: "gitlab-id", ClientSecret: "gitlab-secret", RedirectURL: "https://admin.example/auth/callback", BaseURL: "https://gitlab.example"},
	}
	challenges := make(map[string]string)
	redeemed := make(map[string]bool)
	providerCalls := 0
	profileStarted := make(chan struct{})
	resumeProfile := make(chan struct{})
	client := &http.Client{Transport: oauthTransportFunc(func(r *http.Request) (*http.Response, error) {
		providerCalls++
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token", "https://gitlab.example/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			code := r.PostForm.Get("code")
			if redeemed[code] {
				t.Fatalf("authorization code %q redeemed twice", code)
			}
			redeemed[code] = true
			challenge := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if challenges[code] != base64.RawURLEncoding.EncodeToString(challenge[:]) ||
				r.PostForm.Get("redirect_uri") != "https://admin.example/auth/callback" ||
				r.Header.Get("Accept") != "application/json" {
				t.Fatalf("invalid token exchange for %q: form=%v", code, r.PostForm)
			}
			if r.URL.Host == "github.com" && r.PostForm.Get("client_secret") != "github-secret" ||
				r.URL.Host == "gitlab.example" && (r.PostForm.Get("client_secret") != "gitlab-secret" || r.PostForm.Get("grant_type") != "authorization_code") {
				t.Fatal("wrong provider token exchange")
			}
			return oauthTestResponse(`{"access_token":"synthetic-` + code + `","token_type":"Bearer"}`), nil
		case "https://api.github.com/user":
			if r.Header.Get("Authorization") != "Bearer synthetic-gh1" && r.Header.Get("Authorization") != "Bearer synthetic-gh2" {
				t.Fatal("GitHub profile missing exchanged token")
			}
			if r.Header.Get("Authorization") == "Bearer synthetic-gh2" {
				return oauthTestResponse(`{"id":42,"login":"<script>bad()</script>"}`), nil
			}
			return oauthTestResponse(`{"id":42,"login":"same-name","email":"same@example.test"}`), nil
		case "https://gitlab.example/api/v4/user":
			switch r.Header.Get("Authorization") {
			case "Bearer synthetic-gl1", "Bearer synthetic-glLink":
				return oauthTestResponse(`{"id":42,"username":"same-name","email":"same@example.test"}`), nil
			case "Bearer synthetic-gl2":
				close(profileStarted)
				<-resumeProfile
				return oauthTestResponse(`{"id":84,"username":"another-name"}`), nil
			}
		}
		t.Fatalf("unexpected provider request: %s", r.URL)
		return nil, nil
	})}
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	jw.CookieName = "__Host-minion_session"
	oauth, err := newOAuthAccess(jw, cfg, statePath, client)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, cfg, nil, oauth)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })

	if response := oauthRequest(handler, http.MethodGet, "/auth/callback?state=bad&code=gh1", ""); response.Code != http.StatusBadRequest || providerCalls != 0 {
		t.Fatalf("unproven callback: status=%d calls=%d", response.Code, providerCalls)
	}
	if response := oauthRequest(handler, http.MethodPost, "/auth/link/gitlab", "https://admin.example"); response.Code != http.StatusForbidden {
		t.Fatalf("guest link status=%d, want 403", response.Code)
	}

	ghState, ghQuery := oauthStart(t, handler, http.MethodGet, "/auth/github", nil)
	if ghQuery.Get("client_id") != "github-id" {
		t.Fatalf("GitHub authorization client ID: %q", ghQuery.Get("client_id"))
	}
	challenges["gh1"] = ghQuery.Get("code_challenge")
	if response := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+ghState.Value+"&code=gh1", "", ghState, ghState); response.Code != http.StatusBadRequest || providerCalls != 0 {
		t.Fatalf("duplicate state cookie: status=%d calls=%d", response.Code, providerCalls)
	}
	ghResponse := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+ghState.Value+"&code=gh1", "", ghState)
	if ghResponse.Code != http.StatusSeeOther {
		t.Fatalf("GitHub callback status=%d body=%q", ghResponse.Code, ghResponse.Body.String())
	}
	ghSession := oauthCookie(t, ghResponse, jw.CookieName)
	if ghSession.Value == "" || ghSession.Name != "__Host-minion_session" || !ghSession.Secure || !ghSession.HttpOnly || ghSession.Path != "/" || ghSession.Domain != "" {
		t.Fatalf("invalid user session cookie: %+v", ghSession)
	}
	if response := oauthRequest(handler, http.MethodGet, "/jaws/.ping", "", ghSession); response.Code != http.StatusForbidden {
		t.Fatalf("user session granted protected JaWS route: %d", response.Code)
	}
	for _, path := range []string{"/", "/admin/usage", "/admin/usage.json", "/admin/dashboard.css"} {
		if response := oauthRequest(handler, http.MethodGet, path, "", ghSession); response.Code != http.StatusForbidden {
			t.Fatalf("user session granted admin route %s: %d", path, response.Code)
		}
	}
	if response := oauthRequest(handler, http.MethodPost, "/admin/stop", "https://admin.example", ghSession); response.Code != http.StatusForbidden {
		t.Fatalf("user session granted stop route: %d", response.Code)
	}
	if response := oauthRequest(handler, http.MethodGet, "/admin/login", "", ghSession); response.Code != http.StatusOK || strings.Contains(response.Body.String(), "Signed in") {
		t.Fatalf("user session granted admin page: status=%d body=%q", response.Code, response.Body.String())
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/login", "", ghSession); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Signed in as same-name") ||
		!strings.Contains(response.Body.String(), "Current identity: github:github.com:42") ||
		!strings.Contains(response.Body.String(), "Link gitlab account on gitlab.example") {
		t.Fatalf("user session missing: status=%d body=%q", response.Code, response.Body.String())
	}
	adminRequest := httptest.NewRequest(http.MethodGet, "https://admin.example/admin/login", nil)
	adminRequest.TLS = &tls.ConnectionState{}
	adminRequest.RemoteAddr = "192.0.2.7:12345"
	adminSessionResponse := httptest.NewRecorder()
	adminSession := jw.NewSession(adminSessionResponse, adminRequest)
	if adminSession == nil {
		t.Fatal("could not create admin session")
	}
	adminSession.Set(adminSessionKey, "operator")
	adminCookie := oauthCookie(t, adminSessionResponse, jw.CookieName)
	if response := oauthRequest(handler, http.MethodGet, "/auth/login", "", adminCookie); response.Code != http.StatusOK || strings.Contains(response.Body.String(), "Signed in as") {
		t.Fatalf("admin-only session appeared as an OAuth user: status=%d body=%q", response.Code, response.Body.String())
	}
	if response := oauthRequest(handler, http.MethodPost, "/auth/logout", "https://admin.example", adminCookie); response.Code != http.StatusForbidden {
		t.Fatalf("user logout accepted an admin-only session: %d", response.Code)
	}
	if response := oauthRequest(handler, http.MethodGet, "/admin/login", "", adminCookie); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Signed in") {
		t.Fatalf("user logout revoked the admin session: status=%d body=%q", response.Code, response.Body.String())
	}
	if response := oauthRequest(handler, http.MethodPost, "/admin/logout", "https://admin.example", adminCookie); response.Code != http.StatusSeeOther {
		t.Fatalf("admin logout status=%d", response.Code)
	}
	if response := oauthRequest(handler, http.MethodGet, "/admin/login", "", adminCookie); strings.Contains(response.Body.String(), "Signed in") {
		t.Fatal("admin session remained live after admin logout")
	}
	beforeReplay := providerCalls
	if response := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+ghState.Value+"&code=gh1", "", ghState); response.Code != http.StatusBadRequest || providerCalls != beforeReplay {
		t.Fatalf("callback replay: status=%d calls=%d", response.Code, providerCalls-beforeReplay)
	}
	deniedState, _ := oauthStart(t, handler, http.MethodGet, "/auth/github", nil)
	if response := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+deniedState.Value+"&error=access_denied", "", deniedState); response.Code != http.StatusBadRequest {
		t.Fatalf("provider denial callback status=%d", response.Code)
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+deniedState.Value+"&code=gh1", "", deniedState); response.Code != http.StatusBadRequest || providerCalls != beforeReplay {
		t.Fatalf("provider denial state replay: status=%d calls=%d", response.Code, providerCalls-beforeReplay)
	}
	renewState, renewQuery := oauthStart(t, handler, http.MethodGet, "/auth/github", ghSession)
	challenges["gh2"] = renewQuery.Get("code_challenge")
	renewed := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+renewState.Value+"&code=gh2", "", ghSession, renewState)
	if renewed.Code != http.StatusSeeOther {
		t.Fatalf("session renewal status=%d", renewed.Code)
	}
	renewedSession := oauthCookie(t, renewed, jw.CookieName)
	if response := oauthRequest(handler, http.MethodGet, "/auth/login", "", ghSession); strings.Contains(response.Body.String(), "Signed in as") {
		t.Fatal("new OAuth sign-in did not revoke the previous session")
	}
	ghSession = renewedSession
	if response := oauthRequest(handler, http.MethodGet, "/auth/login", "", ghSession); response.Code != http.StatusOK ||
		strings.Contains(response.Body.String(), "<script>") || !strings.Contains(response.Body.String(), "&lt;script&gt;") {
		t.Fatalf("provider label was not escaped: %q", response.Body.String())
	}

	glState, glQuery := oauthStart(t, handler, http.MethodGet, "/auth/gitlab", nil)
	if glQuery.Get("client_id") != "gitlab-id" {
		t.Fatalf("GitLab authorization client ID: %q", glQuery.Get("client_id"))
	}
	challenges["gl1"] = glQuery.Get("code_challenge")
	glResponse := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+glState.Value+"&code=gl1", "", glState)
	if glResponse.Code != http.StatusSeeOther {
		t.Fatalf("GitLab callback status=%d body=%q", glResponse.Code, glResponse.Body.String())
	}
	glSession := oauthCookie(t, glResponse, jw.CookieName)
	if response := oauthRequest(handler, http.MethodGet, "/auth/login", "", glSession); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Current identity: gitlab:gitlab.example:42") ||
		!strings.Contains(response.Body.String(), "Link github account on github.com") {
		t.Fatalf("GitLab identity was unclear: status=%d body=%q", response.Code, response.Body.String())
	}
	_, _ = oauthStart(t, handler, http.MethodPost, "/auth/link/github", glSession)
	if response := oauthRequest(handler, http.MethodGet, "/auth/login", "", ghSession, glSession); response.Code != http.StatusForbidden {
		t.Fatalf("two live session cookies on the same client IP: status=%d", response.Code)
	}
	if response := oauthRequest(handler, http.MethodGet, "/admin/login", "", ghSession, glSession); response.Code != http.StatusForbidden {
		t.Fatalf("two live session cookies reached admin login: status=%d", response.Code)
	}
	ghKey := userIdentity{Provider: "github", Host: "github.com", ID: 42}.key()
	glKey := userIdentity{Provider: "gitlab", Host: "gitlab.example", ID: 42}.key()
	if oauth.links.account(ghKey) == oauth.links.account(glKey) {
		t.Fatal("equal names, email, and numeric IDs linked two providers without proof")
	}

	linkState, linkQuery := oauthStart(t, handler, http.MethodPost, "/auth/link/gitlab", ghSession)
	challenges["glLink"] = linkQuery.Get("code_challenge")
	linked := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+linkState.Value+"&code=glLink", "", ghSession, linkState)
	if linked.Code != http.StatusSeeOther || oauth.links.account(glKey) != oauth.links.account(ghKey) {
		t.Fatalf("dual-proof link: status=%d gh=%q gl=%q", linked.Code, oauth.links.account(ghKey), oauth.links.account(glKey))
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/login", "", glSession); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Signed in as same-name") ||
		oauth.links.account(glKey) != oauth.links.account(ghKey) {
		t.Fatalf("older GitLab session did not resolve merged account: status=%d body=%q", response.Code, response.Body.String())
	}
	if response := oauthRequest(handler, http.MethodPost, "/admin/logout", "https://admin.example", glSession); response.Code != http.StatusSeeOther {
		t.Fatalf("admin logout of user-only session status=%d", response.Code)
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/login", "", glSession); strings.Contains(response.Body.String(), "Signed in as") {
		t.Fatal("shared JaWS logout left the user session live")
	}
	reopened, err := openUserLinks(statePath + ".user-links.json")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.account(ghKey) != reopened.account(glKey) {
		t.Fatalf("link did not survive restart: error=%v gh=%q gl=%q", err, reopened.account(ghKey), reopened.account(glKey))
	}
	data, err := os.ReadFile(statePath + ".user-links.json")
	if err != nil || strings.Contains(string(data), "synthetic-") || strings.Contains(string(data), "-secret") {
		t.Fatalf("persisted provider credential: read_error=%v", err)
	}
	missingSessionState, _ := oauthStart(t, handler, http.MethodPost, "/auth/link/gitlab", ghSession)
	beforeMissingSession := providerCalls
	if response := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+missingSessionState.Value+"&code=glWithoutSession", "", missingSessionState); response.Code != http.StatusForbidden || providerCalls != beforeMissingSession {
		t.Fatalf("linking without a current first session: status=%d calls=%d", response.Code, providerCalls-beforeMissingSession)
	}

	lateState, lateQuery := oauthStart(t, handler, http.MethodPost, "/auth/link/gitlab", ghSession)
	challenges["gl2"] = lateQuery.Get("code_challenge")
	callbackDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		callbackDone <- oauthRequest(handler, http.MethodGet, "/auth/callback?state="+lateState.Value+"&code=gl2", "", ghSession, lateState)
	}()
	select {
	case <-profileStarted:
	case response := <-callbackDone:
		t.Fatalf("link callback stopped before provider profile: status=%d", response.Code)
	case <-time.After(5 * time.Second):
		close(resumeProfile)
		t.Fatal("link callback did not reach provider profile")
	}
	logout := oauthRequest(handler, http.MethodPost, "/auth/logout", "https://admin.example", ghSession)
	close(resumeProfile)
	if logout.Code != http.StatusSeeOther {
		t.Fatalf("user logout status=%d", logout.Code)
	}
	var response *httptest.ResponseRecorder
	select {
	case response = <-callbackDone:
	case <-time.After(5 * time.Second):
		t.Fatal("link callback did not finish after provider response")
	}
	if response.Code != http.StatusForbidden ||
		oauth.links.account((userIdentity{Provider: "gitlab", Host: "gitlab.example", ID: 84}).key()) == oauth.links.account(glKey) {
		t.Fatalf("revoked first proof linked account: status=%d", response.Code)
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/login", "", ghSession); response.Code != http.StatusOK || strings.Contains(response.Body.String(), "Signed in as") {
		t.Fatalf("revoked user session remains valid: status=%d", response.Code)
	}
}

func TestOAuthPendingStateBound(t *testing.T) {
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/callback"}}
	oauth, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, cfg, nil, oauth)
	if err != nil {
		t.Fatal(err)
	}
	for range maxPendingOAuthPerIP {
		if response := oauthRequest(handler, http.MethodGet, "/auth/github", ""); response.Code != http.StatusSeeOther {
			t.Fatalf("pending state admission: status=%d", response.Code)
		}
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/github", ""); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("excess pending state: status=%d, want 503", response.Code)
	}
	oauth.mu.Lock()
	for key, pending := range oauth.pending {
		pending.expires = pending.expires.Add(-2 * oauthStateLifetime)
		oauth.pending[key] = pending
		break
	}
	oauth.mu.Unlock()
	if response := oauthRequest(handler, http.MethodGet, "/auth/github", ""); response.Code != http.StatusSeeOther {
		t.Fatalf("expired pending state did not release capacity: status=%d", response.Code)
	}
}

func TestOAuthPrunesExpiredPendingWithoutRequest(t *testing.T) {
	now := time.Now()
	a := &oauthAccess{pending: map[string]pendingOAuth{
		"expired": {linkIdentity: "github:github.com:42", verifier: "secret", expires: now.Add(-time.Second)},
		"live":    {expires: now.Add(time.Minute)},
	}}
	a.pruneExpired(now)
	if _, exists := a.pending["expired"]; exists || len(a.pending) != 1 {
		t.Fatalf("expired link state remained after cleanup: entries=%d", len(a.pending))
	}
	if _, exists := a.pending["live"]; !exists {
		t.Fatal("live sign-in state was pruned")
	}
}

func TestOAuthExpiredCallbackDoesNotExchange(t *testing.T) {
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/callback"}}
	providerCalled := false
	client := &http.Client{Transport: oauthTransportFunc(func(*http.Request) (*http.Response, error) {
		providerCalled = true
		return oauthTestResponse(`{}`), nil
	})}
	oauth, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), client)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, cfg, nil, oauth)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := oauthStart(t, handler, http.MethodGet, "/auth/github", nil)
	oauth.mu.Lock()
	pending := oauth.pending[state.Value]
	pending.expires = time.Now().Add(-time.Second)
	oauth.pending[state.Value] = pending
	oauth.mu.Unlock()
	response := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+state.Value+"&code=expired", "", state)
	if response.Code != http.StatusBadRequest || providerCalled {
		t.Fatalf("expired callback: status=%d provider_called=%t", response.Code, providerCalled)
	}
}

func TestOAuthRepeatedStartRetiresBrowserState(t *testing.T) {
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/callback"}}
	a, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, cfg, nil, a)
	if err != nil {
		t.Fatal(err)
	}
	var previous, first *http.Cookie
	for range maxPendingOAuthPerIP + 2 {
		response := oauthRequest(handler, http.MethodGet, "/auth/github", "", previous)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("repeat start: status=%d", response.Code)
		}
		if previous == nil {
			first = oauthCookie(t, response, oauthStateCookie)
		}
		previous = oauthCookie(t, response, oauthStateCookie)
		a.mu.Lock()
		count := len(a.pending)
		a.mu.Unlock()
		if count != 1 {
			t.Fatalf("repeated starts retained %d states", count)
		}
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+first.Value+"&code=stale", "", first); response.Code != http.StatusBadRequest {
		t.Fatalf("retired browser state was accepted: %d", response.Code)
	}
}

func TestOAuthProviderExchangeAndPendingBounds(t *testing.T) {
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/callback"}}
	started := make(chan struct{}, maxOAuthExchanges+1)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	client := &http.Client{Transport: oauthTransportFunc(func(*http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-release
		return nil, errors.New("synthetic provider outage")
	})}
	a, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), client)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, cfg, nil, a)
	if err != nil {
		t.Fatal(err)
	}
	completed := make(chan int, maxOAuthExchanges)
	for range maxOAuthExchanges {
		state, _ := oauthStart(t, handler, http.MethodGet, "/auth/github", nil)
		go func() {
			response := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+state.Value+"&code=blocked", "", state)
			completed <- response.Code
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("provider exchange did not start")
		}
	}
	busyState, _ := oauthStart(t, handler, http.MethodGet, "/auth/github", nil)
	busyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+busyState.Value+"&code=busy", "", busyState)
		busyDone <- response
	}()
	select {
	case response := <-busyDone:
		if response.Code != http.StatusServiceUnavailable || len(response.Result().Cookies()) != 0 {
			t.Fatalf("fifth provider exchange status=%d cookies=%v, want retryable 503", response.Code, response.Result().Cookies())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fifth provider exchange blocked instead of rejecting admission")
	}
	for range maxPendingOAuthPerIP - maxOAuthExchanges - 1 {
		if response := oauthRequest(handler, http.MethodGet, "/auth/github", ""); response.Code != http.StatusSeeOther {
			t.Fatalf("pending admission with in-flight requests: status=%d", response.Code)
		}
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/github", ""); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("in-flight requests escaped pending budget: status=%d", response.Code)
	}
	close(release)
	released = true
	for range maxOAuthExchanges {
		if code := <-completed; code != http.StatusBadGateway {
			t.Fatalf("provider outage status=%d", code)
		}
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+busyState.Value+"&code=busy", "", busyState); response.Code != http.StatusBadGateway {
		t.Fatalf("busy callback could not retry after capacity returned: status=%d", response.Code)
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/github", ""); response.Code != http.StatusSeeOther {
		t.Fatalf("completed exchanges did not release capacity: status=%d", response.Code)
	}
}

func TestOAuthLoopbackStateCookie(t *testing.T) {
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret",
		RedirectURL: "http://127.0.0.1:8377/auth/callback"}}
	a, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8377/auth/github", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	handler, err := newAdminHandler(jw, cfg, nil, a)
	if err != nil {
		t.Fatal(err)
	}
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("loopback sign-in status=%d", w.Code)
	}
	cookie := oauthCookie(t, w, loopbackStateCookie)
	if cookie.Secure || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("loopback state cookie: %+v", cookie)
	}
}

func TestOAuthLinkHandoffForGitLabHosts(t *testing.T) {
	for _, base := range []string{
		"https://gitlab.example:8443", "https://127.0.0.1:8443", "https://[::1]:8443", "https://bücher.example",
	} {
		t.Run(base, func(t *testing.T) {
			jw, err := jaws.New()
			if err != nil {
				t.Fatal(err)
			}
			defer jw.Close()
			cfg := config.AdminConfig{GitLabOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret",
				RedirectURL: "https://admin.example/auth/callback", BaseURL: base}}
			a, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), nil)
			if err != nil {
				t.Fatal(err)
			}
			handler, err := newAdminHandler(jw, cfg, nil, a)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "https://admin.example/auth/link/gitlab", nil)
			r.TLS = &tls.ConnectionState{}
			r.RemoteAddr = "192.0.2.7:12345"
			r.Header.Set("Origin", "https://admin.example")
			session := jw.NewSession(httptest.NewRecorder(), r)
			if session == nil {
				t.Fatal("could not create user session")
			}
			session.Set(userSessionKey, "github:github.com:42")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			body := response.Body.String()
			_, after, found := strings.Cut(body, `<a href="`)
			if !found {
				t.Fatalf("handoff has no fallback link: %q", body)
			}
			escaped, _, found := strings.Cut(after, `"`)
			target := html.UnescapeString(escaped)
			parsed, err := url.Parse(target)
			if response.Code != http.StatusOK || response.Header().Get("Location") != "" ||
				response.Header().Get("Content-Security-Policy") != jw.ContentSecurityPolicy() ||
				!found || err != nil || parsed.Scheme != "https" || parsed.Host != strings.TrimPrefix(base, "https://") ||
				parsed.Path != "/oauth/authorize" || parsed.Query().Get("state") != oauthCookie(t, response, oauthStateCookie).Value ||
				!strings.Contains(body, `http-equiv="refresh" content="0;url=`+escaped+`"`) {
				t.Fatalf("GitLab handoff for %q: status=%d target=%q body=%q error=%v", base, response.Code, target, body, err)
			}
		})
	}
}

func TestOAuthProviderResponsesFailClosed(t *testing.T) {
	for _, test := range []struct {
		name        string
		profileBody string
		redirect    bool
		wantCalls   int
	}{
		{"token redirect", "", true, 1},
		{"invalid immutable ID", `{"id":0,"login":"same-name"}`, false, 2},
		{"oversized profile", `{"id":1,"login":"valid","padding":"` + strings.Repeat("x", maxOAuthResponse) + `"}`, false, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: oauthTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if test.redirect {
						return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://other.example/token"}},
							Body: io.NopCloser(strings.NewReader(""))}, nil
					}
					return oauthTestResponse(`{"access_token":"synthetic-token","token_type":"bearer"}`), nil
				}
				if r.URL.String() != "https://api.github.com/user" {
					t.Fatalf("redirected token or wrong profile destination: %s", r.URL)
				}
				return oauthTestResponse(test.profileBody), nil
			})}
			jw, err := jaws.New()
			if err != nil {
				t.Fatal(err)
			}
			defer jw.Close()
			cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret",
				RedirectURL: "https://admin.example/auth/callback"}}
			a, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), client)
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.fetchIdentity(context.Background(), a.providers["github"], "code", "verifier")
			if err == nil || calls != test.wantCalls || strings.Contains(err.Error(), "synthetic-token") {
				t.Fatalf("provider response: error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestOAuthRoutesWithoutAdministrator(t *testing.T) {
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	defer jw.Close()
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/callback"}}
	a, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, cfg, nil, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		want int
	}{
		{"/auth/login", http.StatusOK},
		{"/auth/callback", http.StatusBadRequest},
		{"/admin/login", http.StatusForbidden},
		{"/jaws/.ping", http.StatusForbidden},
		{"/download/report", http.StatusForbidden},
	} {
		if response := oauthRequest(handler, http.MethodGet, test.path, ""); response.Code != test.want {
			t.Errorf("GET %s: status=%d, want %d", test.path, response.Code, test.want)
		}
	}
}
