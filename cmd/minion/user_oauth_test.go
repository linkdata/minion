package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
)

type oauthRoundTrip func(*http.Request) (*http.Response, error)

func (fn oauthRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func oauthJSON(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestGitHubProfileSubjectUsesExactNumericID(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{json.Number("9007199254740993"), "9007199254740993"},
		{json.Number("9223372036854775807"), "9223372036854775807"},
		{json.Number("9223372036854775808"), ""},
		{json.Number("0"), ""},
		{json.Number("01"), ""},
		{json.Number("1e3"), ""},
		{json.Number("18446744073709551616"), ""},
		{"42", ""},
		{float64(42), ""},
	} {
		got, err := githubProfileSubject(map[string]any{"id": tc.value})
		if got != tc.want || (err == nil) != (tc.want != "") {
			t.Fatalf("GitHub ID %v: subject=%q error=%v", tc.value, got, err)
		}
	}
}

func TestOAuthProviderBindsIssuerAndOpaqueSubject(t *testing.T) {
	p := &oauthProvider{name: "gitlab", issuer: "https://gitlab.example", host: "gitlab.example"}
	for _, tc := range []struct {
		claims map[string]any
		want   bool
	}{
		{map[string]any{"iss": "https://gitlab.example", "sub": "team/user:42"}, true},
		{map[string]any{"iss": "https://other.example", "sub": "team/user:42"}, false},
		{map[string]any{"iss": "https://gitlab.example", "sub": ""}, false},
		{map[string]any{"iss": "https://gitlab.example", "sub": "team\nuser"}, false},
	} {
		id, ok := p.identity(tc.claims)
		if ok != tc.want || ok && id.key() != "gitlab:gitlab.example:team%2Fuser%3A42" {
			t.Fatalf("claims %v: identity=%v accepted=%t", tc.claims, id, ok)
		}
	}
}

func testOAuthAdminHandler(t *testing.T, cfg config.AdminConfig, client *http.Client) (http.Handler, *jaws.Jaws, *oauthAccess) {
	t.Helper()
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	jw.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	auth, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), client)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, cfg, nil, auth)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })
	return handler, jw, auth
}

func TestGitHubOAuthSignInWithoutLocalAdmin(t *testing.T) {
	const githubID = "9007199254740993"
	client := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			return oauthJSON(http.StatusOK, `{"access_token":"secret","token_type":"Bearer","expires_in":3600}`), nil
		case "https://api.github.com/user":
			if r.Header.Get("Authorization") != "Bearer secret" {
				t.Errorf("missing bearer token: %q", r.Header.Get("Authorization"))
			}
			return oauthJSON(http.StatusOK, `{"id":`+githubID+`}`), nil
		default:
			t.Errorf("unexpected OAuth request: %s", r.URL)
			return oauthJSON(http.StatusNotFound, `{}`), nil
		}
	})}
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/github/callback"}}
	handler, jw, auth := testOAuthAdminHandler(t, cfg, client)
	if auth == nil || len(auth.providers) != 1 {
		t.Fatal("GitHub sign-in not configured")
	}
	page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 ||
		!strings.Contains(page.Body.String(), `name="jawsKey"`) ||
		!strings.Contains(page.Body.String(), `<script defer src="/admin/login.js"></script>`) ||
		!strings.Contains(page.Body.String(), "Sign in with GitHub") {
		t.Fatalf("JaWS sign-in page: status=%d body=%q", page.Code, page.Body.String())
	}
	if script := adminResponse(handler, http.MethodGet, "/admin/login.js", nil, nil, ""); script.Code != http.StatusOK ||
		!strings.Contains(script.Body.String(), `form[action="/auth/logout"]`) {
		t.Fatalf("OAuth logout JaWS script: status=%d body=%q", script.Code, script.Body.String())
	}
	guest := page.Result().Cookies()[0]
	start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, guest, "")
	if start.Code != http.StatusFound || start.Header().Get("Strict-Transport-Security") != "" {
		t.Fatalf("start GitHub sign-in: status=%d HSTS=%q body=%q", start.Code, start.Header().Get("Strict-Transport-Security"), start.Body.String())
	}
	location, err := url.Parse(start.Header().Get("Location"))
	if err != nil || location.Host != "github.com" || location.Query().Get("state") == "" ||
		location.Query().Get("code_challenge") == "" {
		t.Fatalf("GitHub authorization URL: %q, %v", start.Header().Get("Location"), err)
	}
	wrong := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=proof&state=wrong", nil, guest, "")
	if wrong.Code != http.StatusBadRequest || wrong.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
		strings.Contains(wrong.Body.String(), "<html") {
		t.Fatalf("failed OAuth callback rendered HTML: status=%d content-type=%q body=%q",
			wrong.Code, wrong.Header().Get("Content-Type"), wrong.Body.String())
	}
	callback := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=proof&state="+url.QueryEscape(location.Query().Get("state")), nil, guest, "")
	if callback.Code != http.StatusFound || len(callback.Result().Cookies()) != 1 || callback.Header().Get("Strict-Transport-Security") != "" {
		t.Fatalf("GitHub callback: status=%d cookies=%v HSTS=%q body=%q", callback.Code, callback.Result().Cookies(), callback.Header().Get("Strict-Transport-Security"), callback.Body.String())
	}
	if callback.Header().Get("Location") != "/auth/login" {
		t.Fatalf("GitHub callback returned to %q", callback.Header().Get("Location"))
	}
	signed := callback.Result().Cookies()[0]
	if signed.Value == guest.Value {
		t.Fatal("OAuth sign-in reused guest session")
	}
	if account, _ := auth.currentUser(jw.GetSession(requestWithCookie(signed))); account != "github:github.com:"+githubID {
		t.Fatalf("authenticated account = %q", account)
	}
	signedPage := adminResponse(handler, http.MethodGet, "/auth/login", nil, signed, "")
	if signedPage.Code != http.StatusOK || !strings.Contains(signedPage.Body.String(), "Signed in with github") ||
		strings.Contains(signedPage.Body.String(), "Link GitHub account") {
		t.Fatalf("signed-in JaWS page: status=%d body=%q", signedPage.Code, signedPage.Body.String())
	}
	if denied := adminResponse(handler, http.MethodPost, "/auth/link/github", nil, signed, "https://admin.example"); denied.Code != http.StatusForbidden {
		t.Fatalf("same-provider link: status=%d", denied.Code)
	}
	for _, path := range []string{"/", "/admin/usage", "/admin/stop"} {
		method := http.MethodGet
		if path == "/admin/stop" {
			method = http.MethodPost
		}
		if denied := adminResponse(handler, method, path, nil, signed, "https://admin.example"); denied.Code != http.StatusForbidden {
			t.Fatalf("OAuth user reached admin %s: %d", path, denied.Code)
		}
	}
	logout := adminResponse(handler, http.MethodPost, "/auth/logout", nil, signed, "https://admin.example")
	if logout.Code != http.StatusSeeOther || len(logout.Result().Cookies()) != 0 {
		t.Fatalf("OAuth logout: status=%d cookies=%v", logout.Code, logout.Result().Cookies())
	}
	if account, _ := auth.currentUser(jw.GetSession(requestWithCookie(signed))); account != "" {
		t.Fatalf("closed session retained account %q", account)
	}
	if denied := adminResponse(handler, http.MethodGet, "/jaws/"+adminJaWSKey(t, signedPage.Body.String()), nil, signed, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("closed session retained JaWS page: %d", denied.Code)
	}
	reopened := adminResponse(handler, http.MethodGet, "/auth/login", nil, signed, "")
	if reopened.Code != http.StatusOK || len(reopened.Result().Cookies()) != 1 ||
		reopened.Result().Cookies()[0].Value == signed.Value ||
		!strings.Contains(reopened.Body.String(), "Sign in with GitHub") {
		t.Fatalf("guest page after logout: status=%d cookies=%v body=%q", reopened.Code, reopened.Result().Cookies(), reopened.Body.String())
	}
	if _, err := os.Stat(auth.links.path); !os.IsNotExist(err) {
		t.Fatalf("unlinked login wrote durable links: %v", err)
	}
}

func requestWithCookie(cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://admin.example/auth/login", nil)
	r.AddCookie(cookie)
	return r
}

func TestGitHubOAuthFlowAndAuthExpiry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		firstPage bool
		delay     time.Duration
		want      int
		tokenTTL  int
		after     time.Duration
	}{
		{"direct start", false, 61 * time.Second, http.StatusFound, 3600, 0},
		{"JaWS page first", true, 61 * time.Second, http.StatusFound, 3600, 0},
		{"expired flow", false, 11 * time.Minute, http.StatusBadRequest, 3600, 0},
		{"expired auth", false, 0, http.StatusFound, 30, 31 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				jw, err := jaws.New()
				if err != nil {
					t.Fatal(err)
				}
				var providerCalls atomic.Int32
				client := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
					providerCalls.Add(1)
					switch r.URL.String() {
					case "https://github.com/login/oauth/access_token":
						return oauthJSON(http.StatusOK, fmt.Sprintf(`{"access_token":"proof","token_type":"Bearer","expires_in":%d}`, tc.tokenTTL)), nil
					case "https://api.github.com/user":
						return oauthJSON(http.StatusOK, `{"id":42}`), nil
					default:
						return nil, fmt.Errorf("unexpected provider request %s", r.URL)
					}
				})}
				cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
					RedirectURL: "https://admin.example/auth/github/callback"}}
				auth, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), client)
				if err != nil {
					t.Fatal(err)
				}
				handler, err := newAdminHandler(jw, cfg, nil, auth)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				go func() { jw.Serve(); close(done) }()
				defer func() { jw.Close(); <-done }()
				var guest *http.Cookie
				if tc.firstPage {
					page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
					if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 {
						t.Fatalf("guest JaWS page: status=%d cookies=%v", page.Code, page.Result().Cookies())
					}
					guest = page.Result().Cookies()[0]
				}
				start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, guest, "")
				if start.Code != http.StatusFound {
					t.Fatalf("start OAuth: %d", start.Code)
				}
				if guest == nil {
					if len(start.Result().Cookies()) != 1 {
						t.Fatalf("direct start cookies=%v", start.Result().Cookies())
					}
					guest = start.Result().Cookies()[0]
				}
				location, err := url.Parse(start.Header().Get("Location"))
				if err != nil || location.Query().Get("state") == "" {
					t.Fatalf("start location=%q error=%v", start.Header().Get("Location"), err)
				}
				time.Sleep(tc.delay)
				callback := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=proof&state="+url.QueryEscape(location.Query().Get("state")), nil, guest, "")
				if callback.Code != tc.want {
					t.Fatalf("callback after %s: status=%d cookies=%v body=%q", tc.delay, callback.Code, callback.Result().Cookies(), callback.Body.String())
				}
				if tc.want == http.StatusFound && (len(callback.Result().Cookies()) != 1 || providerCalls.Load() != 2) {
					t.Fatalf("valid callback after %s: cookies=%v provider calls=%d", tc.delay, callback.Result().Cookies(), providerCalls.Load())
				}
				if tc.want != http.StatusFound && (len(callback.Result().Cookies()) != 0 || providerCalls.Load() != 0) {
					t.Fatalf("expired callback exchanged code: cookies=%v provider calls=%d", callback.Result().Cookies(), providerCalls.Load())
				}
				if tc.after != 0 {
					signed := callback.Result().Cookies()[0]
					time.Sleep(tc.after)
					sess := jw.GetSession(requestWithCookie(signed))
					if sess == nil {
						t.Fatal("JaWS session ended before provider proof expiry check")
					}
					if account, _ := auth.currentUser(sess); account != "" {
						t.Fatalf("expired provider proof retained account %q", account)
					}
				}
			})
		})
	}
}

func testGitLabIssuer(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var nonce atomic.Value
	nonce.Store("")
	var issuer *httptest.Server
	issuer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize",
				"token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/keys",
			})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB",
			}}})
		case "/token":
			if err := r.ParseForm(); err != nil || r.Form.Get("code_verifier") == "" {
				t.Errorf("invalid token request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
			claims, _ := json.Marshal(map[string]any{"iss": issuer.URL, "aud": "app", "sub": "team:42/member",
				"preferred_username": "same", "email": "same@example.com",
				"nonce": nonce.Load().(string), "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()})
			payload := base64.RawURLEncoding.EncodeToString(claims)
			input := header + "." + payload
			digest := sha256.Sum256([]byte(input))
			signed, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			if err != nil {
				t.Errorf("sign test token: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "gitlab-token", "token_type": "Bearer",
				"expires_in": 3600, "id_token": input + "." + base64.RawURLEncoding.EncodeToString(signed)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(issuer.Close)
	return issuer, &nonce
}

func TestGitLabOIDCSignInAndLinkGitHub(t *testing.T) {
	issuer, nonce := testGitLabIssuer(t)
	tlsTransport := issuer.Client().Transport
	client := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			return oauthJSON(http.StatusOK, `{"access_token":"github-token","token_type":"Bearer","expires_in":3600}`), nil
		case "https://api.github.com/user":
			return oauthJSON(http.StatusOK, `{"id":42,"login":"same","email":"same@example.com"}`), nil
		default:
			if r.URL.Host != strings.TrimPrefix(issuer.URL, "https://") {
				return nil, fmt.Errorf("unexpected provider request %s", r.URL)
			}
			return tlsTransport.RoundTrip(r)
		}
	})}
	cfg := config.AdminConfig{
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/github/callback"},
		GitLabOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/gitlab/callback", BaseURL: issuer.URL},
	}
	handler, jw, auth := testOAuthAdminHandler(t, cfg, client)
	page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 ||
		!strings.Contains(page.Body.String(), "Sign in with GitLab") {
		t.Fatalf("dual-provider JaWS page: %d %q", page.Code, page.Body.String())
	}
	guest := page.Result().Cookies()[0]
	if denied := adminResponse(handler, http.MethodPost, "/auth/link/gitlab", nil, guest, "https://admin.example"); denied.Code != http.StatusForbidden {
		t.Fatalf("guest linked an account: status=%d", denied.Code)
	}
	start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, guest, "")
	githubURL, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || githubURL.Query().Get("state") == "" {
		t.Fatalf("start GitHub flow: status=%d location=%q err=%v", start.Code, start.Header().Get("Location"), err)
	}
	if wrong := adminResponse(handler, http.MethodGet, "/auth/gitlab/callback?code=github&state="+url.QueryEscape(githubURL.Query().Get("state")), nil, guest, ""); wrong.Code != http.StatusBadRequest || len(wrong.Result().Cookies()) != 0 {
		t.Fatalf("wrong-provider callback: status=%d cookies=%v", wrong.Code, wrong.Result().Cookies())
	}
	login := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=github&state="+url.QueryEscape(githubURL.Query().Get("state")), nil, guest, "")
	if login.Code != http.StatusFound || len(login.Result().Cookies()) != 1 {
		t.Fatalf("GitHub callback: status=%d body=%q", login.Code, login.Body.String())
	}
	if replay := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=github&state="+url.QueryEscape(githubURL.Query().Get("state")), nil, guest, ""); replay.Code != http.StatusBadRequest || len(replay.Result().Cookies()) != 0 {
		t.Fatalf("replayed callback: status=%d cookies=%v", replay.Code, replay.Result().Cookies())
	}
	signed := login.Result().Cookies()[0]
	gitlabKey := (userIdentity{Provider: "gitlab", Host: strings.TrimPrefix(issuer.URL, "https://"), Subject: "team:42/member"}).key()
	if account := auth.links.account(gitlabKey); account == "github:github.com:42" {
		t.Fatal("matching provider labels linked accounts without dual proof")
	}
	if denied := adminResponse(handler, http.MethodPost, "/auth/link/gitlab", nil, signed, "https://other.example"); denied.Code != http.StatusForbidden {
		t.Fatalf("cross-origin account link: status=%d", denied.Code)
	}
	link := adminResponse(handler, http.MethodPost, "/auth/link/gitlab", nil, signed, "https://admin.example")
	gitlabURL, err := url.Parse(link.Header().Get("Location"))
	if err != nil || link.Code != http.StatusFound || gitlabURL.Host != strings.TrimPrefix(issuer.URL, "https://") ||
		gitlabURL.Query().Get("state") == "" || gitlabURL.Query().Get("nonce") == "" {
		t.Fatalf("start GitLab link: status=%d location=%q err=%v", link.Code, link.Header().Get("Location"), err)
	}
	nonce.Store(gitlabURL.Query().Get("nonce"))
	linked := adminResponse(handler, http.MethodGet, "/auth/gitlab/callback?code=gitlab&state="+url.QueryEscape(gitlabURL.Query().Get("state")), nil, signed, "")
	if linked.Code != http.StatusFound || len(linked.Result().Cookies()) != 0 || linked.Header().Get("Location") != "/auth/login" {
		t.Fatalf("GitLab link callback: status=%d cookies=%v location=%q body=%q", linked.Code, linked.Result().Cookies(), linked.Header().Get("Location"), linked.Body.String())
	}
	if account := auth.links.account(gitlabKey); account != "github:github.com:42" {
		t.Fatalf("linked account = %q", account)
	}
	if saved, err := openUserLinks(auth.links.path); err != nil {
		t.Fatalf("read durable link: %v", err)
	} else if account := saved.account(gitlabKey); account != "github:github.com:42" {
		t.Fatalf("durable link: account=%q", account)
	}
	if account, _ := auth.currentUser(jw.GetSession(requestWithCookie(signed))); account != "github:github.com:42" {
		t.Fatalf("current account after link = %q", account)
	}

	// A later GitLab sign-in must resolve through the persisted account link.
	start = adminResponse(handler, http.MethodGet, "/auth/gitlab/login", nil, signed, "")
	gitlabURL, err = url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || gitlabURL.Query().Get("state") == "" {
		t.Fatalf("start GitLab sign-in: status=%d location=%q err=%v", start.Code, start.Header().Get("Location"), err)
	}
	nonce.Store(gitlabURL.Query().Get("nonce"))
	login = adminResponse(handler, http.MethodGet, "/auth/gitlab/callback?code=gitlab&state="+url.QueryEscape(gitlabURL.Query().Get("state")), nil, signed, "")
	if login.Code != http.StatusFound || len(login.Result().Cookies()) != 1 {
		t.Fatalf("GitLab sign-in callback: status=%d body=%q", login.Code, login.Body.String())
	}
	if account, _ := auth.currentUser(jw.GetSession(requestWithCookie(login.Result().Cookies()[0]))); account != "github:github.com:42" {
		t.Fatalf("GitLab account after sign-in = %q", account)
	}
}
