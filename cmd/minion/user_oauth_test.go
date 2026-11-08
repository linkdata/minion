package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
)

type oauthRoundTrip func(*http.Request) (*http.Response, error)

func (fn oauthRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

type oauthRotationNoticeLock struct {
	lock    sync.Locker
	entered chan struct{}
	once    sync.Once
}

func (l *oauthRotationNoticeLock) Lock() {
	l.once.Do(func() { close(l.entered) })
	l.lock.Lock()
}

func (l *oauthRotationNoticeLock) Unlock() { l.lock.Unlock() }

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
	if head := adminResponse(handler, http.MethodHead, "/auth/login", nil, nil, ""); head.Code != http.StatusOK ||
		head.Body.Len() != 0 || len(head.Result().Cookies()) != 0 || jw.SessionCount() != 0 {
		t.Fatalf("OAuth login HEAD: status=%d cookies=%v sessions=%d body=%q", head.Code, head.Result().Cookies(), jw.SessionCount(), head.Body.String())
	}
	page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 ||
		!strings.Contains(page.Body.String(), `name="jawsKey"`) ||
		strings.Contains(page.Body.String(), `<script defer src="/admin/login.js"></script>`) ||
		!strings.Contains(page.Body.String(), "Sign in with GitHub") {
		t.Fatalf("JaWS sign-in page: status=%d body=%q", page.Code, page.Body.String())
	}
	if asset := adminResponse(handler, http.MethodGet, bootstrapAssetPath(t, page.Body.String(), ".css"), nil, nil, ""); asset.Code != http.StatusOK {
		t.Fatalf("OAuth sign-in Bootstrap asset: status=%d", asset.Code)
	}
	home := adminResponse(handler, http.MethodGet, "/", nil, page.Result().Cookies()[0], "")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), `href="/auth/login">User sign in</a>`) ||
		strings.Contains(home.Body.String(), `href="/admin/login"`) ||
		!strings.Contains(home.Body.String(), `name="jawsKey"`) {
		t.Fatalf("OAuth-only home: status=%d body=%q", home.Code, home.Body.String())
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
	if wrong.Code != http.StatusSeeOther || wrong.Header().Get("Location") != "/auth/login?error=request" ||
		len(wrong.Result().Cookies()) != 0 || wrong.Body.Len() != 0 {
		t.Fatalf("failed OAuth redirect: status=%d location=%q cookies=%v body=%q",
			wrong.Code, wrong.Header().Get("Location"), wrong.Result().Cookies(), wrong.Body.String())
	}
	if home := adminResponse(handler, http.MethodGet, "/", nil, guest, ""); home.Code != http.StatusOK || len(home.Result().Cookies()) != 0 {
		t.Fatalf("failed OAuth callback changed guest session: status=%d cookies=%v", home.Code, home.Result().Cookies())
	}
	guestErrorPage := adminResponse(handler, http.MethodGet, wrong.Header().Get("Location"), nil, guest, "")
	if guestErrorPage.Code != http.StatusOK || !strings.Contains(guestErrorPage.Body.String(), `name="jawsKey"`) ||
		!strings.Contains(guestErrorPage.Body.String(), `role="alert"`) ||
		!strings.Contains(guestErrorPage.Body.String(), `<meta name="viewport"`) || strings.Contains(guestErrorPage.Body.String(), "wrong") {
		t.Fatalf("failed OAuth JaWS page: status=%d body=%q", guestErrorPage.Code, guestErrorPage.Body.String())
	}
	missingSession := adminResponse(handler, http.MethodGet, "/auth/github/callback?state=wrong", nil, nil, "")
	if missingSession.Code != http.StatusSeeOther || missingSession.Header().Get("Location") != "/auth/login?error=request" || missingSession.Body.Len() != 0 {
		t.Fatalf("OAuth callback without session: status=%d location=%q body=%q", missingSession.Code, missingSession.Header().Get("Location"), missingSession.Body.String())
	}
	errorPage := adminResponse(handler, http.MethodGet, missingSession.Header().Get("Location"), nil, nil, "")
	if errorPage.Code != http.StatusOK || !strings.Contains(errorPage.Body.String(), `name="jawsKey"`) ||
		!strings.Contains(errorPage.Body.String(), `role="alert"`) ||
		!strings.Contains(errorPage.Body.String(), `<meta name="viewport"`) {
		t.Fatalf("OAuth fallback JaWS page: status=%d body=%q", errorPage.Code, errorPage.Body.String())
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
	if denied := adminResponse(handler, http.MethodPost, "/auth/link/github", nil, signed, "https://admin.example"); denied.Code != http.StatusForbidden || denied.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(denied.Body.String(), `name="jawsKey"`) {
		t.Fatalf("same-provider JaWS error: status=%d content-type=%q body=%q", denied.Code, denied.Header().Get("Content-Type"), denied.Body.String())
	}
	if home := adminResponse(handler, http.MethodGet, "/", nil, signed, ""); home.Code != http.StatusOK ||
		!strings.Contains(home.Body.String(), `href="/auth/login">Your account</a>`) ||
		strings.Contains(home.Body.String(), `href="/admin/login"`) {
		t.Fatalf("signed-in OAuth home: status=%d body=%q", home.Code, home.Body.String())
	}
	for _, path := range []string{"/admin/usage", "/admin/stop"} {
		method := http.MethodGet
		if path == "/admin/stop" {
			method = http.MethodPost
		}
		if denied := adminResponse(handler, method, path, nil, signed, "https://admin.example"); denied.Code != http.StatusForbidden {
			t.Fatalf("OAuth user reached admin %s: %d", path, denied.Code)
		}
	}
	logout := adminResponse(handler, http.MethodPost, "/auth/logout", nil, signed, "https://admin.example")
	if logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != "/" || len(logout.Result().Cookies()) != 0 {
		t.Fatalf("OAuth logout: status=%d location=%q cookies=%v", logout.Code, logout.Header().Get("Location"), logout.Result().Cookies())
	}
	if account, _ := auth.currentUser(jw.GetSession(requestWithCookie(signed))); account != "" {
		t.Fatalf("closed session retained account %q", account)
	}
	if denied := adminResponse(handler, http.MethodGet, "/jaws/"+adminJaWSKey(t, signedPage.Body.String()), nil, signed, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("closed session retained JaWS page: %d", denied.Code)
	}
	if usageReload := adminResponse(handler, http.MethodGet, "/user/usage", nil, signed, ""); usageReload.Code != http.StatusSeeOther || !strings.HasPrefix(usageReload.Header().Get("Location"), "/auth/login") {
		t.Fatalf("closed user page reload: status=%d location=%q", usageReload.Code, usageReload.Header().Get("Location"))
	}
	reopened := adminResponse(handler, http.MethodGet, "/", nil, signed, "")
	if reopened.Code != http.StatusOK || len(reopened.Result().Cookies()) != 1 ||
		reopened.Result().Cookies()[0].Value == signed.Value ||
		!strings.Contains(reopened.Body.String(), `href="/auth/login">User sign in</a>`) ||
		strings.Contains(reopened.Body.String(), "Your account") {
		t.Fatalf("guest home after logout: status=%d cookies=%v body=%q", reopened.Code, reopened.Result().Cookies(), reopened.Body.String())
	}
	if loginPage := adminResponse(handler, http.MethodGet, "/auth/login", nil, reopened.Result().Cookies()[0], ""); loginPage.Code != http.StatusOK || !strings.Contains(loginPage.Body.String(), "Sign in with GitHub") {
		t.Fatalf("guest OAuth login after logout: status=%d body=%q", loginPage.Code, loginPage.Body.String())
	}
	if guestLogout := adminResponse(handler, http.MethodPost, "/auth/logout", nil, reopened.Result().Cookies()[0], "https://admin.example"); guestLogout.Code != http.StatusSeeOther || guestLogout.Header().Get("Location") != "/" || jw.GetSession(requestWithCookie(reopened.Result().Cookies()[0])) != nil {
		t.Fatalf("guest logout: status=%d location=%q", guestLogout.Code, guestLogout.Header().Get("Location"))
	}
	if _, err := os.Stat(auth.links.path); !os.IsNotExist(err) {
		t.Fatalf("unlinked login wrote durable links: %v", err)
	}
}

func TestOAuthLogoutClosesSessionAfterLinkStoreWriteFailure(t *testing.T) {
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/github/callback"}}
	handler, jw, auth := testOAuthAdminHandler(t, cfg, nil)
	request := httptest.NewRequest(http.MethodGet, "https://admin.example/auth/login", nil)
	response := httptest.NewRecorder()
	sess := jw.NewSession(response, request)
	if sess == nil || len(response.Result().Cookies()) != 1 {
		t.Fatal("could not create OAuth session")
	}
	provider := auth.providers[0].server
	sess.Set(provider.SessionKey, map[string]any{"iss": "https://github.com", "sub": "42"})
	sess.Set("oauth2authexpiry.github", time.Now().Add(time.Hour))
	if account, _ := auth.currentUser(sess); account == "" {
		t.Fatal("OAuth session was not authenticated before storage failure")
	}
	auth.links.mu.Lock()
	auth.links.bad = errors.New("save user links: injected write failure")
	auth.links.mu.Unlock()
	if account, _ := auth.currentUser(sess); account != "" {
		t.Fatalf("fault-injected link store resolved account %q", account)
	}
	var logoutEvents int
	provider.LogoutEvent = func(*jaws.Session, *http.Request) { logoutEvents++ }
	cookie := response.Result().Cookies()[0]
	logout := adminResponse(handler, http.MethodPost, "/auth/logout", nil, cookie, "https://admin.example")
	if logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != "/" || len(logout.Result().Cookies()) != 0 ||
		jw.GetSession(requestWithCookie(cookie)) != nil || sess.Get(provider.SessionKey) != nil || logoutEvents != 1 {
		t.Fatalf("storage-failure logout: status=%d location=%q cookies=%v session=%v claims=%v provider logout events=%d",
			logout.Code, logout.Header().Get("Location"), logout.Result().Cookies(), jw.GetSession(requestWithCookie(cookie)), sess.Get(provider.SessionKey), logoutEvents)
	}
	missing := adminResponse(handler, http.MethodPost, "/auth/logout", nil, nil, "https://admin.example")
	if missing.Code != http.StatusSeeOther || missing.Header().Get("Location") != "/" || len(missing.Result().Cookies()) != 0 {
		t.Fatalf("sessionless logout: status=%d location=%q cookies=%v", missing.Code, missing.Header().Get("Location"), missing.Result().Cookies())
	}
}

func requestWithCookie(cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://admin.example/auth/login", nil)
	r.AddCookie(cookie)
	return r
}

func TestOAuthBrowserErrorKeepsStatusAndRetryAfter(t *testing.T) {
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/github/callback"}}
	client := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			return oauthJSON(http.StatusOK, `{"access_token":"proof","token_type":"Bearer","expires_in":3600}`), nil
		case "https://api.github.com/user":
			return oauthJSON(http.StatusOK, `{"id":42}`), nil
		default:
			return nil, fmt.Errorf("unexpected provider request %s", r.URL)
		}
	})}
	handler, jw, auth := testOAuthAdminHandler(t, cfg, client)
	start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, nil, "")
	if start.Code != http.StatusFound || len(start.Result().Cookies()) != 1 {
		t.Fatalf("OAuth start: status=%d cookies=%v", start.Code, start.Result().Cookies())
	}
	location, err := url.Parse(start.Header().Get("Location"))
	if err != nil || location.Query().Get("state") == "" {
		t.Fatalf("OAuth start location=%q error=%v", start.Header().Get("Location"), err)
	}
	login := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=proof&state="+url.QueryEscape(location.Query().Get("state")), nil, start.Result().Cookies()[0], "")
	if login.Code != http.StatusFound || len(login.Result().Cookies()) != 1 {
		t.Fatalf("OAuth callback: status=%d cookies=%v", login.Code, login.Result().Cookies())
	}
	signed := login.Result().Cookies()[0]
	auth.links.mu.Lock()
	auth.links.bad = errors.New("link store unavailable")
	auth.links.mu.Unlock()
	if account, _ := auth.currentUser(jw.GetSession(requestWithCookie(signed))); account != "" {
		t.Fatalf("uncertain link store resolved account %q", account)
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		w := httptest.NewRecorder()
		w.Header().Set("Retry-After", "12")
		r := httptest.NewRequest(http.MethodGet, "https://admin.example/auth/usage", nil)
		r.AddCookie(signed)
		auth.browserError(w, r, status)
		if w.Code != status || w.Header().Get("Retry-After") != "12" ||
			w.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
			!strings.Contains(w.Body.String(), `name="jawsKey"`) ||
			!strings.Contains(w.Body.String(), `role="alert"`) ||
			!strings.Contains(w.Body.String(), `<meta name="viewport"`) {
			t.Fatalf("JaWS error %d: status=%d headers=%v body=%q", status, w.Code, w.Header(), w.Body.String())
		}
	}
}

func TestOAuthGuestErrorsLeaveLoginRequestSlot(t *testing.T) {
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/github/callback"}}
	handler, jw, _ := testOAuthAdminHandler(t, cfg, nil)
	page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 {
		t.Fatalf("guest page: status=%d cookies=%v", page.Code, page.Result().Cookies())
	}
	guest := page.Result().Cookies()[0]
	for i := range maxGuestPageRequests + 1 {
		failed := adminResponse(handler, http.MethodGet, "/auth/github/callback?state=wrong", nil, guest, "")
		if failed.Code != http.StatusSeeOther || failed.Header().Get("Location") != "/auth/login?error=request" || failed.Body.Len() != 0 {
			t.Fatalf("guest callback error %d: status=%d location=%q body=%q", i, failed.Code, failed.Header().Get("Location"), failed.Body.String())
		}
	}
	if sess := jw.GetSession(requestWithCookie(guest)); sess == nil || len(sess.Requests()) != 1 {
		t.Fatalf("guest errors consumed JaWS requests: session=%v", sess)
	}
	retry := adminResponse(handler, http.MethodGet, "/auth/login", nil, guest, "")
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `name="jawsKey"`) {
		t.Fatalf("guest login retry: status=%d body=%q", retry.Code, retry.Body.String())
	}
	fresh := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if fresh.Code != http.StatusOK || len(fresh.Result().Cookies()) != 1 {
		t.Fatalf("second guest page: status=%d cookies=%v", fresh.Code, fresh.Result().Cookies())
	}
	guest = fresh.Result().Cookies()[0]
	start := make(chan struct{})
	var workers sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 32)
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			responses <- adminResponse(handler, http.MethodGet, "/auth/github/callback?state=wrong", nil, guest, "")
		}()
	}
	close(start)
	workers.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/auth/login?error=request" {
			t.Fatalf("concurrent guest error: status=%d location=%q", response.Code, response.Header().Get("Location"))
		}
	}
	if sess := jw.GetSession(requestWithCookie(guest)); sess == nil || len(sess.Requests()) != 1 {
		t.Fatalf("concurrent errors used the login slot: session=%v", sess)
	}
	retry = adminResponse(handler, http.MethodGet, "/auth/login", nil, guest, "")
	if retry.Code != http.StatusOK {
		t.Fatalf("login after concurrent errors: status=%d", retry.Code)
	}
}

func TestOAuthErrorWriterPreservesHeaders(t *testing.T) {
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/github/callback"}}
	handler, _, auth := testOAuthAdminHandler(t, cfg, nil)
	page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 {
		t.Fatalf("guest page: status=%d cookies=%v", page.Code, page.Result().Cookies())
	}
	wrapped := auth.oauthErrors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Provider-Result", "retained")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("private provider error"))
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "https://admin.example/auth/github/callback", nil)
	r.AddCookie(page.Result().Cookies()[0])
	wrapped.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/auth/login?error=request" ||
		w.Header().Get("X-Provider-Result") != "retained" ||
		w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" {
		t.Fatalf("OAuth error response: status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
}

func TestOAuthStartsReserveAdminSessionCapacity(t *testing.T) {
	cfg := config.AdminConfig{
		Administrators: []config.AdminAccount{testAdminAccount()},
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/github/callback"},
	}
	handler, jw, _ := testOAuthAdminHandler(t, cfg, nil)
	admin := adminGuestCookie(t, handler)
	if home := adminResponse(handler, http.MethodGet, "/", nil, nil, ""); home.Code != http.StatusOK ||
		!strings.Contains(home.Body.String(), `href="/admin/login"`) ||
		!strings.Contains(home.Body.String(), `href="/auth/login"`) {
		t.Fatalf("combined sign-in home: status=%d body=%q", home.Code, home.Body.String())
	}
	for jw.SessionCount() < maxGuestPageSessions-1 {
		response := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, nil, "")
		if response.Code != http.StatusFound || len(response.Result().Cookies()) != 1 {
			t.Fatalf("OAuth start before limit: status=%d cookies=%v", response.Code, response.Result().Cookies())
		}
	}
	page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 || jw.SessionCount() != maxGuestPageSessions {
		t.Fatalf("guest page before final start: status=%d cookies=%v sessions=%d", page.Code, page.Result().Cookies(), jw.SessionCount())
	}
	guest := page.Result().Cookies()[0]

	start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, nil, "")
	if start.Code != http.StatusFound || len(start.Result().Cookies()) != 1 || jw.SessionCount() != maxGuestAdmissionSessions {
		t.Fatalf("last OAuth start: status=%d cookies=%v sessions=%d", start.Code, start.Result().Cookies(), jw.SessionCount())
	}
	for _, cookie := range []*http.Cookie{nil, guest} {
		response := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, cookie, "")
		if response.Code != http.StatusServiceUnavailable || len(response.Result().Cookies()) != 0 || response.Header().Get("Retry-After") != "60" ||
			(cookie == nil && response.Body.Len() != 0) ||
			(cookie != nil && response.Body.Len() != 0 && !strings.Contains(response.Body.String(), `name="jawsKey"`)) {
			t.Fatalf("OAuth start at reserve: status=%d cookies=%v body=%q retry=%q", response.Code, response.Result().Cookies(), response.Body.String(), response.Header().Get("Retry-After"))
		}
	}
	login := adminResponse(handler, http.MethodPost, "/admin/login", url.Values{
		"username": {"admin"}, "password": {"correct-password"},
	}, admin, "")
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("admin login after OAuth saturation: status=%d cookies=%v", login.Code, login.Result().Cookies())
	}
	if dashboard := adminResponse(handler, http.MethodGet, "/", nil, login.Result().Cookies()[0], ""); dashboard.Code != http.StatusOK {
		t.Fatalf("admin dashboard after OAuth saturation: status=%d", dashboard.Code)
	}
}

func TestConcurrentGuestPagesReserveAdminSessions(t *testing.T) {
	cfg := config.AdminConfig{
		Administrators: []config.AdminAccount{testAdminAccount()},
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/github/callback"},
	}
	handler, jw, _ := testOAuthAdminHandler(t, cfg, nil)
	admin := adminGuestCookie(t, handler)
	for jw.SessionCount() < maxGuestPageSessions-1 {
		w := httptest.NewRecorder()
		if jw.NewSession(w, httptest.NewRequest(http.MethodGet, "https://admin.example/auth/login", nil)) == nil {
			t.Fatal("could not fill guest sessions")
		}
	}
	paths := []string{"/", "/auth/login", "/auth/github/login"}
	responses := make([]*httptest.ResponseRecorder, 128)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range responses {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			responses[i] = adminResponse(handler, http.MethodGet, paths[i%len(paths)], nil, nil, "")
		}()
	}
	close(start)
	workers.Wait()
	if count := jw.SessionCount(); count != maxGuestAdmissionSessions {
		t.Fatalf("concurrent guest sessions=%d, want %d", count, maxGuestAdmissionSessions)
	}
	admitted := 0
	for i, response := range responses {
		path := paths[i%len(paths)]
		if path == "/" && response.Code == http.StatusSeeOther && response.Header().Get("Location") == "/admin/login" {
			continue
		}
		want := http.StatusOK
		if path == "/auth/github/login" {
			want = http.StatusFound
		}
		if response.Code == want {
			admitted++
		} else if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("concurrent guest %d (%s): status=%d", i, path, response.Code)
		}
	}
	if admitted != 2 {
		t.Fatalf("concurrent guest admissions=%d, want 2", admitted)
	}
	if home := adminResponse(handler, http.MethodGet, "/", nil, nil, ""); home.Code != http.StatusSeeOther ||
		home.Header().Get("Location") != "/admin/login" || len(home.Result().Cookies()) != 0 {
		t.Fatalf("home after guest burst: status=%d location=%q sessions=%d", home.Code, home.Header().Get("Location"), jw.SessionCount())
	}
	page := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 ||
		!strings.Contains(page.Body.String(), "/jaws/.tail/") || jw.SessionCount() != maxGuestAdmissionSessions+1 {
		t.Fatalf("fresh admin page after guest burst: status=%d sessions=%d cookies=%v", page.Code, jw.SessionCount(), page.Result().Cookies())
	}
	login := adminResponse(handler, http.MethodPost, "/admin/login", url.Values{
		"username": {"admin"}, "password": {"correct-password"},
	}, page.Result().Cookies()[0], "")
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("fresh admin cannot sign in after guest burst: status=%d sessions=%d", login.Code, jw.SessionCount())
	}
	if dashboard := adminResponse(handler, http.MethodGet, "/", nil, login.Result().Cookies()[0], ""); dashboard.Code != http.StatusOK {
		t.Fatalf("fresh admin dashboard after guest burst: status=%d", dashboard.Code)
	}
	if login := adminResponse(handler, http.MethodPost, "/admin/login", url.Values{
		"username": {"admin"}, "password": {"correct-password"},
	}, admin, ""); login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("existing admin guest cannot sign in after burst: status=%d sessions=%d", login.Code, jw.SessionCount())
	}
}

func TestOAuthLoginPageReservesAdminSessionCapacity(t *testing.T) {
	cfg := config.AdminConfig{
		Administrators: []config.AdminAccount{testAdminAccount()},
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/github/callback"},
	}
	handler, jw, _ := testOAuthAdminHandler(t, cfg, nil)
	var guest *http.Cookie
	for jw.SessionCount() < maxGuestPageSessions-1 {
		w := httptest.NewRecorder()
		if jw.NewSession(w, httptest.NewRequest(http.MethodGet, "https://admin.example/auth/login", nil)) == nil {
			t.Fatal("could not fill guest sessions")
		}
		if guest == nil {
			guest = w.Result().Cookies()[0]
		}
	}
	page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 || jw.SessionCount() != maxGuestPageSessions {
		t.Fatalf("last guest page: status=%d cookies=%v sessions=%d", page.Code, page.Result().Cookies(), jw.SessionCount())
	}
	last := page.Result().Cookies()[0]
	if start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, last, ""); start.Code != http.StatusFound {
		t.Fatalf("last OAuth login page cannot start OAuth: status=%d", start.Code)
	}
	if denied := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, ""); denied.Code != http.StatusServiceUnavailable || len(denied.Result().Cookies()) != 0 {
		t.Fatalf("guest page above reserve: status=%d cookies=%v", denied.Code, denied.Result().Cookies())
	}
	if page := adminResponse(handler, http.MethodGet, "/auth/login", nil, guest, ""); page.Code != http.StatusOK || len(page.Result().Cookies()) != 0 {
		t.Fatalf("existing guest page at reserve: status=%d cookies=%v", page.Code, page.Result().Cookies())
	}
	if failed := adminResponse(handler, http.MethodGet, "/auth/github/callback", nil, nil, ""); failed.Code != http.StatusServiceUnavailable || failed.Header().Get("Retry-After") != "60" || failed.Header().Get("Location") != "" || len(failed.Result().Cookies()) != 0 {
		t.Fatalf("OAuth error at guest entry limit: status=%d headers=%v cookies=%v", failed.Code, failed.Header(), failed.Result().Cookies())
	}
	if page := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, ""); page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 {
		t.Fatalf("new admin page at guest limit: status=%d cookies=%v", page.Code, page.Result().Cookies())
	}
	if page := adminResponse(handler, http.MethodGet, "/admin/login", nil, guest, ""); page.Code != http.StatusOK || len(page.Result().Cookies()) != 0 {
		t.Fatalf("existing guest admin page at guest limit: status=%d cookies=%v", page.Code, page.Result().Cookies())
	}
}

func TestOAuthLinkAndLogoutSerializePersistence(t *testing.T) {
	issuer, nonce := testGitLabIssuer(t)
	transport := issuer.Client().Transport
	var blockToken atomic.Bool
	tokenEntered := make(chan struct{})
	tokenRelease := make(chan struct{})
	releaseToken := sync.OnceFunc(func() { close(tokenRelease) })
	defer releaseToken()
	client := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			return oauthJSON(http.StatusOK, `{"access_token":"proof","token_type":"Bearer","expires_in":3600}`), nil
		case "https://api.github.com/user":
			return oauthJSON(http.StatusOK, `{"id":42}`), nil
		default:
			if r.URL.String() == issuer.URL+"/token" && blockToken.Load() {
				close(tokenEntered)
				<-tokenRelease
			}
			return transport.RoundTrip(r)
		}
	})}
	cfg := config.AdminConfig{
		Administrators: []config.AdminAccount{testAdminAccount()},
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/github/callback"},
		GitLabOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/gitlab/callback", BaseURL: issuer.URL},
	}
	handler, jw, auth := testOAuthAdminHandler(t, cfg, client)
	signIn := func() (*http.Cookie, *jaws.Session) {
		start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, nil, "")
		location, err := url.Parse(start.Header().Get("Location"))
		if err != nil || start.Code != http.StatusFound || len(start.Result().Cookies()) != 1 {
			t.Fatalf("GitHub start: status=%d cookies=%v error=%v", start.Code, start.Result().Cookies(), err)
		}
		callback := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=proof&state="+url.QueryEscape(location.Query().Get("state")), nil, start.Result().Cookies()[0], "")
		if callback.Code != http.StatusFound || len(callback.Result().Cookies()) != 1 {
			t.Fatalf("GitHub login: status=%d cookies=%v", callback.Code, callback.Result().Cookies())
		}
		cookie := callback.Result().Cookies()[0]
		sess := jw.GetSession(requestWithCookie(cookie))
		if sess == nil {
			t.Fatal("missing signed-in session")
		}
		return cookie, sess
	}
	cookie, sess := signIn()
	linkEvent := auth.providers[1].server.LinkEvent
	// jawsauth invokes LinkEvent while holding SessionRotationLock.
	callLinkEvent := func(sess *jaws.Session, cookie *http.Cookie, claims map[string]any) (int, error) {
		auth.linkMu.Lock()
		defer auth.linkMu.Unlock()
		return linkEvent(sess, requestWithCookie(cookie), claims)
	}
	proof := map[string]any{"iss": issuer.URL, "sub": "member:42"}
	key := (userIdentity{Provider: "gitlab", Host: strings.TrimPrefix(issuer.URL, "https://"), Subject: "member:42"}).key()

	auth.linkMu.Lock()
	linked := make(chan int, 1)
	linkStarted := make(chan struct{})
	go func() {
		close(linkStarted)
		status, _ := callLinkEvent(sess, cookie, proof)
		linked <- status
	}()
	<-linkStarted
	select {
	case status := <-linked:
		auth.linkMu.Unlock()
		t.Fatalf("link bypassed persistence lock: status=%d", status)
	case <-time.After(50 * time.Millisecond):
	}
	if account := auth.links.account(key); account != key {
		auth.linkMu.Unlock()
		t.Fatalf("link persisted before lock released: %q", account)
	}
	auth.linkMu.Unlock()
	select {
	case status := <-linked:
		if status != 0 || auth.links.account(key) != "github:github.com:42" {
			t.Fatalf("link after lock released: status=%d account=%q", status, auth.links.account(key))
		}
	case <-time.After(time.Second):
		t.Fatal("link did not finish after lock released")
	}

	staleKey := (userIdentity{Provider: "gitlab", Host: strings.TrimPrefix(issuer.URL, "https://"), Subject: "member:43"}).key()
	for _, path := range []string{"/auth/logout", "/admin/logout"} {
		t.Run(path, func(t *testing.T) {
			if path == "/admin/logout" {
				cookie, sess = signIn()
			}
			auth.linkMu.Lock()
			loggedOut := make(chan *httptest.ResponseRecorder, 1)
			logoutStarted := make(chan struct{})
			go func() {
				close(logoutStarted)
				loggedOut <- adminResponse(handler, http.MethodPost, path, nil, cookie, "https://admin.example")
			}()
			<-logoutStarted
			select {
			case response := <-loggedOut:
				auth.linkMu.Unlock()
				t.Fatalf("logout bypassed persistence lock: status=%d", response.Code)
			case <-time.After(50 * time.Millisecond):
			}
			if jw.GetSession(requestWithCookie(cookie)) == nil {
				auth.linkMu.Unlock()
				t.Fatal("logout closed session before lock released")
			}
			auth.linkMu.Unlock()
			select {
			case response := <-loggedOut:
				if response.Code != http.StatusSeeOther || jw.GetSession(requestWithCookie(cookie)) != nil {
					t.Fatalf("logout after link: status=%d sessionAlive=%t", response.Code, jw.GetSession(requestWithCookie(cookie)) != nil)
				}
			case <-time.After(time.Second):
				t.Fatal("logout did not finish after lock released")
			}
			if status, _ := callLinkEvent(sess, cookie, map[string]any{"iss": issuer.URL, "sub": "member:43"}); status != http.StatusForbidden || auth.links.account(staleKey) != staleKey {
				t.Fatalf("stale link after logout: status=%d account=%q", status, auth.links.account(staleKey))
			}
		})
	}
	cookie, sess = signIn()
	auth.linkMu.Lock()
	rotationStarted := make(chan struct{})
	rotatedResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		close(rotationStarted)
		rotatedResult <- adminResponse(handler, http.MethodPost, "/admin/login", url.Values{
			"username": {"admin"}, "password": {"correct-password"},
		}, cookie, "")
	}()
	<-rotationStarted
	select {
	case response := <-rotatedResult:
		auth.linkMu.Unlock()
		t.Fatalf("admin login bypassed persistence lock: status=%d", response.Code)
	case <-time.After(100 * time.Millisecond):
	}
	if jw.GetSession(requestWithCookie(cookie)) == nil {
		auth.linkMu.Unlock()
		t.Fatal("admin login closed OAuth session before lock released")
	}
	auth.linkMu.Unlock()
	var rotated *httptest.ResponseRecorder
	select {
	case rotated = <-rotatedResult:
	case <-time.After(time.Second):
		t.Fatal("admin login did not finish after lock released")
	}
	if rotated.Code != http.StatusSeeOther || len(rotated.Result().Cookies()) != 1 {
		t.Fatalf("admin login rotation: status=%d cookies=%v", rotated.Code, rotated.Result().Cookies())
	}
	if status, _ := callLinkEvent(sess, cookie, map[string]any{"iss": issuer.URL, "sub": "member:43"}); status != http.StatusForbidden || auth.links.account(staleKey) != staleKey {
		t.Fatalf("stale link after admin login: status=%d account=%q", status, auth.links.account(staleKey))
	}

	cookie, _ = signIn()
	link := adminResponse(handler, http.MethodPost, "/auth/link/gitlab", nil, cookie, "https://admin.example")
	linkURL, err := url.Parse(link.Header().Get("Location"))
	if err != nil || link.Code != http.StatusSeeOther || linkURL.Query().Get("state") == "" || linkURL.Query().Get("nonce") == "" {
		t.Fatalf("GitLab link start: status=%d location=%q error=%v", link.Code, link.Header().Get("Location"), err)
	}
	nonce.Store(linkURL.Query().Get("nonce"))
	blockToken.Store(true)
	lateCallback := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		lateCallback <- adminResponse(handler, http.MethodGet, "/auth/gitlab/callback?code=proof&state="+url.QueryEscape(linkURL.Query().Get("state")), nil, cookie, "")
	}()
	select {
	case <-tokenEntered:
	case <-time.After(time.Second):
		t.Fatal("GitLab callback did not reach provider")
	}
	if logout := adminResponse(handler, http.MethodPost, "/auth/logout", nil, cookie, "https://admin.example"); logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != "/" ||
		len(logout.Result().Cookies()) != 0 {
		t.Fatalf("logout before callback: status=%d location=%q cookies=%v", logout.Code, logout.Header().Get("Location"), logout.Result().Cookies())
	}
	if home := adminResponse(handler, http.MethodGet, "/", nil, cookie, ""); home.Code != http.StatusOK || len(home.Result().Cookies()) != 1 || home.Result().Cookies()[0].Value == cookie.Value {
		t.Fatalf("home after sign-out during callback: status=%d cookies=%v", home.Code, home.Result().Cookies())
	}
	releaseToken()
	select {
	case response := <-lateCallback:
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/auth/login?error=request" {
			t.Fatalf("callback after logout: status=%d location=%q", response.Code, response.Header().Get("Location"))
		}
	case <-time.After(time.Second):
		t.Fatal("callback did not finish after provider resumed")
	}
	lateKey := (userIdentity{Provider: "gitlab", Host: strings.TrimPrefix(issuer.URL, "https://"), Subject: "team:42/member"}).key()
	if saved, err := openUserLinks(auth.links.path); err != nil || saved.account(lateKey) != lateKey {
		t.Fatalf("late callback persisted link: saved=%v error=%v", saved, err)
	}
}

func TestOAuthLoginRotationCannotPersistStaleLink(t *testing.T) {
	issuer, nonce := testGitLabIssuer(t)
	transport := issuer.Client().Transport
	tokenEntered := make(chan struct{})
	tokenRelease := make(chan struct{})
	releaseToken := sync.OnceFunc(func() { close(tokenRelease) })
	defer releaseToken()
	client := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			return oauthJSON(http.StatusOK, `{"access_token":"proof","token_type":"Bearer","expires_in":3600}`), nil
		case "https://api.github.com/user":
			return oauthJSON(http.StatusOK, `{"id":42}`), nil
		default:
			if r.URL.String() == issuer.URL+"/token" {
				close(tokenEntered)
				<-tokenRelease
			}
			return transport.RoundTrip(r)
		}
	})}
	cfg := config.AdminConfig{
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/github/callback"},
		GitLabOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/gitlab/callback", BaseURL: issuer.URL},
	}
	handler, jw, auth := testOAuthAdminHandler(t, cfg, client)
	start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, nil, "")
	loginURL, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || len(start.Result().Cookies()) != 1 {
		t.Fatalf("initial GitHub start: status=%d error=%v", start.Code, err)
	}
	guest := start.Result().Cookies()[0]
	login := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=proof&state="+url.QueryEscape(loginURL.Query().Get("state")), nil, guest, "")
	if login.Code != http.StatusFound || len(login.Result().Cookies()) != 1 {
		t.Fatalf("initial GitHub callback: status=%d", login.Code)
	}
	signed := login.Result().Cookies()[0]
	if auth.providers[0].server.SessionRotationLock != &auth.linkMu {
		t.Fatal("GitHub callback does not share the link persistence lock")
	}
	link := adminResponse(handler, http.MethodPost, "/auth/link/gitlab", nil, signed, "https://admin.example")
	linkURL, err := url.Parse(link.Header().Get("Location"))
	if err != nil || link.Code != http.StatusSeeOther || linkURL.Query().Get("state") == "" {
		t.Fatalf("GitLab link start: status=%d error=%v", link.Code, err)
	}
	relogin := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, signed, "")
	reloginURL, err := url.Parse(relogin.Header().Get("Location"))
	if err != nil || relogin.Code != http.StatusFound || reloginURL.Query().Get("state") == "" {
		t.Fatalf("second GitHub start: status=%d error=%v", relogin.Code, err)
	}
	notice := &oauthRotationNoticeLock{lock: &auth.linkMu, entered: make(chan struct{})}
	auth.providers[0].server.SessionRotationLock = notice
	nonce.Store(linkURL.Query().Get("nonce"))
	linked := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		linked <- adminResponse(handler, http.MethodGet, "/auth/gitlab/callback?code=proof&state="+url.QueryEscape(linkURL.Query().Get("state")), nil, signed, "")
	}()
	select {
	case <-tokenEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("GitLab callback did not reach provider")
	}
	auth.linkMu.Lock()
	releaseRotation := sync.OnceFunc(func() { auth.linkMu.Unlock() })
	defer releaseRotation()
	rotation := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rotation <- adminResponse(handler, http.MethodGet, "/auth/github/callback?code=proof&state="+url.QueryEscape(reloginURL.Query().Get("state")), nil, signed, "")
	}()
	select {
	case <-notice.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("second GitHub callback did not reach session rotation")
	}
	if jw.GetSession(requestWithCookie(signed)) == nil {
		t.Fatal("GitHub callback rotated before acquiring the link lock")
	}
	releaseRotation()
	var rotated *httptest.ResponseRecorder
	select {
	case rotated = <-rotation:
	case <-time.After(5 * time.Second):
		t.Fatal("GitHub callback did not finish after link lock released")
	}
	if rotated.Code != http.StatusFound || len(rotated.Result().Cookies()) != 1 || jw.GetSession(requestWithCookie(signed)) != nil {
		t.Fatalf("second GitHub callback: status=%d cookies=%v", rotated.Code, rotated.Result().Cookies())
	}
	releaseToken()
	select {
	case response := <-linked:
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/auth/login?error=request" {
			t.Fatalf("stale GitLab link callback: status=%d location=%q", response.Code, response.Header().Get("Location"))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale GitLab callback did not finish")
	}
	key := (userIdentity{Provider: "gitlab", Host: strings.TrimPrefix(issuer.URL, "https://"), Subject: "team:42/member"}).key()
	if saved, err := openUserLinks(auth.links.path); err != nil || saved.account(key) != key {
		t.Fatalf("stale callback persisted link: saved=%v error=%v", saved, err)
	}
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
		{"expired flow", false, 11 * time.Minute, http.StatusSeeOther, 3600, 0},
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
				if tc.want != http.StatusFound && (len(callback.Result().Cookies()) != 0 || providerCalls.Load() != 0 ||
					callback.Header().Get("Location") != "/auth/login?error=request" || callback.Body.Len() != 0) {
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
	if denied := adminResponse(handler, http.MethodPost, "/auth/link/gitlab", nil, guest, "https://admin.example"); denied.Code != http.StatusSeeOther || denied.Header().Get("Location") != "/auth/login?error=request" || denied.Body.Len() != 0 {
		t.Fatalf("guest linked an account: status=%d", denied.Code)
	}
	start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, guest, "")
	githubURL, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || githubURL.Query().Get("state") == "" {
		t.Fatalf("start GitHub flow: status=%d location=%q err=%v", start.Code, start.Header().Get("Location"), err)
	}
	if wrong := adminResponse(handler, http.MethodGet, "/auth/gitlab/callback?code=github&state="+url.QueryEscape(githubURL.Query().Get("state")), nil, guest, ""); wrong.Code != http.StatusSeeOther || wrong.Header().Get("Location") != "/auth/login?error=request" || wrong.Body.Len() != 0 || len(wrong.Result().Cookies()) != 0 {
		t.Fatalf("wrong-provider callback: status=%d cookies=%v", wrong.Code, wrong.Result().Cookies())
	}
	login := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=github&state="+url.QueryEscape(githubURL.Query().Get("state")), nil, guest, "")
	if login.Code != http.StatusFound || len(login.Result().Cookies()) != 1 {
		t.Fatalf("GitHub callback: status=%d body=%q", login.Code, login.Body.String())
	}
	if replay := adminResponse(handler, http.MethodGet, "/auth/github/callback?code=github&state="+url.QueryEscape(githubURL.Query().Get("state")), nil, guest, ""); replay.Code != http.StatusSeeOther || replay.Header().Get("Location") != "/auth/login?error=request" || len(replay.Result().Cookies()) != 0 {
		t.Fatalf("replayed callback: status=%d cookies=%v", replay.Code, replay.Result().Cookies())
	}
	signed := login.Result().Cookies()[0]
	gitlabKey := (userIdentity{Provider: "gitlab", Host: strings.TrimPrefix(issuer.URL, "https://"), Subject: "team:42/member"}).key()
	if account := auth.links.account(gitlabKey); account == "github:github.com:42" {
		t.Fatal("matching provider labels linked accounts without dual proof")
	}
	if page := adminResponse(handler, http.MethodGet, "/auth/login", nil, signed, ""); !strings.Contains(page.Body.String(), "Link GitLab account") {
		t.Fatalf("unlinked account cannot link GitLab: %q", page.Body.String())
	}
	if denied := adminResponse(handler, http.MethodPost, "/auth/link/gitlab", nil, signed, "https://other.example"); denied.Code != http.StatusForbidden || denied.Body.Len() != 0 {
		t.Fatalf("cross-origin account link: status=%d body=%q", denied.Code, denied.Body.String())
	}
	link := adminResponse(handler, http.MethodPost, "/auth/link/gitlab", nil, signed, "https://admin.example")
	gitlabURL, err := url.Parse(link.Header().Get("Location"))
	if err != nil || link.Code != http.StatusSeeOther || gitlabURL.Host != strings.TrimPrefix(issuer.URL, "https://") ||
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
	if page := adminResponse(handler, http.MethodGet, "/auth/login", nil, signed, ""); strings.Contains(page.Body.String(), "Link GitLab account") {
		t.Fatalf("linked GitHub sign-in offered another GitLab identity: %q", page.Body.String())
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
	if page := adminResponse(handler, http.MethodGet, "/auth/login", nil, login.Result().Cookies()[0], ""); strings.Contains(page.Body.String(), "Link GitHub account") {
		t.Fatalf("linked GitLab sign-in offered another GitHub identity: %q", page.Body.String())
	}
}
