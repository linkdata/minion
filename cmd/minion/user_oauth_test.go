package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if response.Code != http.StatusSeeOther {
		t.Fatalf("start %s: status=%d body=%q", path, response.Code, response.Body.String())
	}
	stateCookie := oauthCookie(t, response, oauthStateCookie)
	if !stateCookie.Secure || !stateCookie.HttpOnly || stateCookie.SameSite != http.SameSiteLaxMode || stateCookie.Path != "/auth/callback" {
		t.Fatalf("state cookie attributes: %+v", stateCookie)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := location.Query()
	if query.Get("state") != stateCookie.Value || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" || query.Get("redirect_uri") != "https://admin.example/auth/callback" {
		t.Fatalf("OAuth authorization parameters: %v", query)
	}
	return stateCookie, query
}

func TestOAuthSignInLinkAndRevocation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config.AdminConfig{
		Administrators: []config.AdminAccount{testAdminAccount()},
		GitHubOAuth:    &config.OAuthClient{ClientID: "github-id", ClientSecret: "github-secret", RedirectURL: "https://admin.example/auth/callback"},
		GitLabOAuth:    &config.OAuthClient{ClientID: "gitlab-id", ClientSecret: "gitlab-secret", RedirectURL: "https://admin.example/auth/callback", BaseURL: "https://gitlab.example"},
	}
	challenges := make(map[string]string)
	providerCalls := 0
	client := &http.Client{Transport: oauthTransportFunc(func(r *http.Request) (*http.Response, error) {
		providerCalls++
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token", "https://gitlab.example/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			code := r.PostForm.Get("code")
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
			case "Bearer synthetic-gl1":
				return oauthTestResponse(`{"id":42,"username":"same-name","email":"same@example.test"}`), nil
			case "Bearer synthetic-gl2":
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
	challenges["gh1"] = ghQuery.Get("code_challenge")
	ghResponse := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+ghState.Value+"&code=gh1", "", ghState)
	if ghResponse.Code != http.StatusSeeOther {
		t.Fatalf("GitHub callback status=%d body=%q", ghResponse.Code, ghResponse.Body.String())
	}
	ghSession := oauthCookie(t, ghResponse, jw.CookieName)
	if ghSession.Value == "" || !ghSession.Secure || !ghSession.HttpOnly {
		t.Fatalf("invalid user session cookie: %+v", ghSession)
	}
	if response := oauthRequest(handler, http.MethodGet, "/jaws/.ping", "", ghSession); response.Code != http.StatusForbidden {
		t.Fatalf("user session granted protected JaWS route: %d", response.Code)
	}
	if response := oauthRequest(handler, http.MethodGet, "/admin/login", "", ghSession); response.Code != http.StatusOK || strings.Contains(response.Body.String(), "Signed in") {
		t.Fatalf("user session granted admin page: status=%d body=%q", response.Code, response.Body.String())
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/login", "", ghSession); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Signed in as same-name") {
		t.Fatalf("user session missing: status=%d body=%q", response.Code, response.Body.String())
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
	challenges["gl1"] = glQuery.Get("code_challenge")
	glResponse := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+glState.Value+"&code=gl1", "", glState)
	if glResponse.Code != http.StatusSeeOther {
		t.Fatalf("GitLab callback status=%d body=%q", glResponse.Code, glResponse.Body.String())
	}
	glSession := oauthCookie(t, glResponse, jw.CookieName)
	ghKey := userIdentity{Provider: "github", Host: "github.com", ID: 42}.key()
	glKey := userIdentity{Provider: "gitlab", Host: "gitlab.example", ID: 42}.key()
	if oauth.links.account(ghKey) == oauth.links.account(glKey) {
		t.Fatal("equal names, email, and numeric IDs linked two providers without proof")
	}

	linkState, linkQuery := oauthStart(t, handler, http.MethodPost, "/auth/link/gitlab", ghSession)
	challenges["gl1"] = linkQuery.Get("code_challenge")
	linked := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+linkState.Value+"&code=gl1", "", ghSession, linkState)
	if linked.Code != http.StatusSeeOther || oauth.links.account(glKey) != oauth.links.account(ghKey) {
		t.Fatalf("dual-proof link: status=%d gh=%q gl=%q", linked.Code, oauth.links.account(ghKey), oauth.links.account(glKey))
	}
	oldSessionRequest := httptest.NewRequest(http.MethodGet, "https://admin.example/auth/login", nil)
	oldSessionRequest.RemoteAddr = "192.0.2.7:12345"
	oldSessionRequest.AddCookie(glSession)
	if response := oauthRequest(handler, http.MethodGet, "/auth/login", "", glSession); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Signed in as same-name") ||
		oauth.links.account(oauth.userIdentity(oldSessionRequest)) != oauth.links.account(ghKey) {
		t.Fatalf("older GitLab session did not resolve merged account: status=%d body=%q", response.Code, response.Body.String())
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

	lateState, lateQuery := oauthStart(t, handler, http.MethodPost, "/auth/link/gitlab", ghSession)
	challenges["gl2"] = lateQuery.Get("code_challenge")
	logout := oauthRequest(handler, http.MethodPost, "/auth/logout", "https://admin.example", ghSession)
	if logout.Code != http.StatusSeeOther {
		t.Fatalf("user logout status=%d", logout.Code)
	}
	if response := oauthRequest(handler, http.MethodGet, "/auth/callback?state="+lateState.Value+"&code=gl2", "", ghSession, lateState); response.Code != http.StatusForbidden ||
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

func TestOAuthProviderResponsesFailClosed(t *testing.T) {
	for _, test := range []struct {
		name        string
		profileBody string
		redirect    bool
		wantCalls   int
	}{
		{"token redirect", "", true, 1},
		{"invalid immutable ID", `{"id":0,"login":"same-name"}`, false, 2},
		{"oversized profile", strings.Repeat("x", maxOAuthResponse+1), false, 2},
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
