package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
	"golang.org/x/crypto/argon2"
)

func testAdminAccount() config.AdminAccount {
	return testAdminAccountWithPassword("correct-password")
}

func testAdminAccountWithPassword(password string) config.AdminAccount {
	salt := bytes.Repeat([]byte{7}, 16)
	tag := argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32)
	return config.AdminAccount{Username: "admin", PasswordHash: fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(tag))}
}

func testAdminHandler(t *testing.T, administrators []config.AdminAccount) (http.Handler, *jaws.Jaws, *bytes.Buffer) {
	t.Helper()
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	logs := new(bytes.Buffer)
	jw.Logger = slog.New(slog.NewTextHandler(logs, nil))
	handler, err := newAdminHandler(jw, config.AdminConfig{Administrators: administrators}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })
	return handler, jw, logs
}

func adminResponse(handler http.Handler, method, path string, form url.Values, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, "https://admin.example"+path, body)
	req.TLS = &tls.ConnectionState{}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func adminJaWSKey(t *testing.T, page string) string {
	t.Helper()
	_, after, found := strings.Cut(page, `<meta name="jawsKey" content="`)
	if !found {
		t.Fatal("page omitted JaWS request key")
	}
	key, _, found := strings.Cut(after, `"`)
	if !found || key == "" {
		t.Fatal("page has invalid JaWS request key")
	}
	return key
}

func pageAssetPath(t *testing.T, page, prefix, suffix string) string {
	t.Helper()
	for _, part := range strings.Split(page, `"`) {
		if strings.HasPrefix(part, prefix) && strings.HasSuffix(part, suffix) {
			return part
		}
	}
	t.Fatalf("page omitted %s%s asset", prefix, suffix)
	return ""
}

func bootstrapAssetPath(t *testing.T, page, suffix string) string {
	t.Helper()
	return pageAssetPath(t, page, "/static/bootstrap", suffix)
}

func adminGuestCookie(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	response := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 {
		t.Fatalf("guest sign-in page: status=%d cookies=%v", response.Code, response.Result().Cookies())
	}
	return response.Result().Cookies()[0]
}

func adminNetworkLogin(t *testing.T, client *http.Client, baseURL string, form url.Values) (*http.Response, *http.Cookie) {
	t.Helper()
	page, err := client.Get(baseURL + "/admin/login")
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, page.Body)
	closeErr := page.Body.Close()
	if readErr != nil || closeErr != nil || page.StatusCode != http.StatusOK || len(page.Cookies()) != 1 {
		t.Fatalf("network sign-in page: status=%d cookies=%v read=%v close=%v", page.StatusCode, page.Cookies(), readErr, closeErr)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/admin/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(page.Cookies()[0])
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response, page.Cookies()[0]
}

func TestAdminLoginJaWSKeyIsolationAndRotation(t *testing.T) {
	handler, jw, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	if jw.MaxSessions != maxAdminSessions || jw.MaxSessionsPerIP != 0 {
		t.Fatalf("unbounded JaWS sessions: global=%d per_ip=%d", jw.MaxSessions, jw.MaxSessionsPerIP)
	}
	first := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if first.Code != http.StatusOK || len(first.Result().Cookies()) != 1 ||
		!strings.Contains(first.Body.String(), `name="password"`) ||
		!strings.Contains(first.Body.String(), `<input type="hidden" name="username" value="admin">`) ||
		strings.Contains(first.Body.String(), `autocomplete="username"`) ||
		!strings.Contains(first.Body.String(), `<script defer src="/admin/login.js"></script>`) ||
		!strings.Contains(first.Body.String(), `/jaws/.tail/`) ||
		first.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(first.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("guest JaWS login page: status=%d cookies=%v body=%q", first.Code, first.Result().Cookies(), first.Body.String())
	}
	script := adminResponse(handler, http.MethodGet, "/admin/login.js", nil, nil, "")
	if script.Code != http.StatusOK || script.Header().Get("Content-Type") != "text/javascript; charset=utf-8" ||
		!strings.Contains(script.Body.String(), "jawsUnloading()") ||
		!strings.Contains(script.Body.String(), "removeEventListener('message', jawsMessage)") {
		t.Fatalf("login submit script: status=%d type=%q body=%q", script.Code, script.Header().Get("Content-Type"), script.Body.String())
	}
	if head := adminResponse(handler, http.MethodHead, "/admin/login.js", nil, nil, ""); head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("login submit script HEAD: status=%d bodyBytes=%d", head.Code, head.Body.Len())
	}
	firstCookie, firstKey := first.Result().Cookies()[0], adminJaWSKey(t, first.Body.String())
	second := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if second.Code != http.StatusOK || len(second.Result().Cookies()) != 1 {
		t.Fatalf("second guest login page: status=%d cookies=%v", second.Code, second.Result().Cookies())
	}
	secondCookie, secondKey := second.Result().Cookies()[0], adminJaWSKey(t, second.Body.String())
	if firstCookie.Value == secondCookie.Value || firstKey == secondKey {
		t.Fatal("same-IP guests received the same session or request key")
	}
	preLoginGET := adminResponse(handler, http.MethodGet, "/admin/login", nil, firstCookie, "")
	if preLoginGET.Code != http.StatusOK || len(preLoginGET.Result().Cookies()) != 0 {
		t.Fatalf("existing guest GET changed cookie: status=%d cookies=%v", preLoginGET.Code, preLoginGET.Result().Cookies())
	}
	for _, path := range []string{"/jaws/" + firstKey, "/jaws/" + firstKey + "/noscript", "/jaws/.tail/" + firstKey} {
		for _, other := range []*http.Cookie{nil, secondCookie} {
			if denied := adminResponse(handler, http.MethodGet, path, nil, other, ""); denied.Code != http.StatusForbidden {
				t.Fatalf("foreign JaWS key %s: status=%d, want 403", path, denied.Code)
			}
		}
	}
	if tail := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+firstKey, nil, firstCookie, ""); tail.Code != http.StatusOK {
		t.Fatalf("own JaWS tail: status=%d, want 200", tail.Code)
	}
	if noscript := adminResponse(handler, http.MethodGet, "/jaws/"+secondKey+"/noscript", nil, secondCookie, ""); noscript.Code != http.StatusNoContent {
		t.Fatalf("own JaWS noscript: status=%d, want 204", noscript.Code)
	}
	home := adminResponse(handler, http.MethodGet, "/", nil, firstCookie, "")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), `href="/admin/login"`) ||
		strings.Contains(home.Body.String(), `href="/auth/login"`) || !strings.Contains(home.Body.String(), `name="jawsKey"`) ||
		len(home.Result().Cookies()) != 0 {
		t.Fatalf("guest home: status=%d cookies=%v body=%q", home.Code, home.Result().Cookies(), home.Body.String())
	}
	if tail := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+adminJaWSKey(t, home.Body.String()), nil, firstCookie, ""); tail.Code != http.StatusOK {
		t.Fatalf("guest home JaWS tail: status=%d", tail.Code)
	}
	for _, tc := range []struct {
		path string
		want int
	}{{bootstrapAssetPath(t, first.Body.String(), ".css"), http.StatusOK}, {bootstrapAssetPath(t, first.Body.String(), ".js"), http.StatusOK},
		{"/admin/login.js", http.StatusOK}, {pageAssetPath(t, first.Body.String(), "/jaws/.jaws.", ".js"), http.StatusOK},
		{pageAssetPath(t, first.Body.String(), "/jaws/.jaws.", ".css"), http.StatusOK}, {"/jaws/.ping", http.StatusNoContent}} {
		if asset := adminResponse(handler, http.MethodGet, tc.path, nil, nil, ""); asset.Code != tc.want {
			t.Fatalf("public asset %s: status=%d, want %d", tc.path, asset.Code, tc.want)
		}
	}
	login := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"username": {"admin"}, "password": {"correct-password"}}, firstCookie, "https://admin.example")
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login after guest page: status=%d cookies=%v", login.Code, login.Result().Cookies())
	}
	adminCookie := login.Result().Cookies()[0]
	if jw.GetSession(requestWithCookie(firstCookie)) != nil {
		t.Fatal("old JaWS session remained live after sign-in")
	}
	for _, cookie := range []*http.Cookie{firstCookie, adminCookie} {
		if denied := adminResponse(handler, http.MethodGet, "/jaws/"+firstKey, nil, cookie, ""); denied.Code != http.StatusForbidden {
			t.Fatalf("rotated guest key remained usable: status=%d", denied.Code)
		}
	}
	signed := adminResponse(handler, http.MethodGet, "/admin/login", nil, adminCookie, "")
	if signed.Code != http.StatusOK || !strings.Contains(signed.Body.String(), "Signed in") ||
		strings.Contains(signed.Body.String(), `<script defer src="/admin/login.js"></script>`) {
		t.Fatalf("signed-in JaWS login page: status=%d body=%q", signed.Code, signed.Body.String())
	}
	signedKey := adminJaWSKey(t, signed.Body.String())
	for _, path := range []string{"/jaws/" + signedKey, "/jaws/.tail/" + signedKey} {
		if denied := adminResponse(handler, http.MethodGet, path, nil, secondCookie, ""); denied.Code != http.StatusForbidden {
			t.Fatalf("guest reached admin page key %s: status=%d", path, denied.Code)
		}
	}
	logout := adminResponse(handler, http.MethodPost, "/admin/logout", nil, adminCookie, "https://admin.example")
	if logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != "/" || len(logout.Result().Cookies()) != 0 {
		t.Fatalf("logout: status=%d cookies=%v", logout.Code, logout.Result().Cookies())
	}
	newGuest := adminResponse(handler, http.MethodGet, "/", nil, adminCookie, "")
	if newGuest.Code != http.StatusOK || len(newGuest.Result().Cookies()) != 1 ||
		!strings.Contains(newGuest.Body.String(), `href="/admin/login"`) || strings.Contains(newGuest.Body.String(), "Live actions") {
		t.Fatalf("guest home after logout: status=%d cookies=%v body=%q", newGuest.Code, newGuest.Result().Cookies(), newGuest.Body.String())
	}
	if loginPage := adminResponse(handler, http.MethodGet, "/admin/login", nil, newGuest.Result().Cookies()[0], ""); loginPage.Code != http.StatusOK || !strings.Contains(loginPage.Body.String(), `name="password"`) {
		t.Fatalf("guest login after logout: status=%d body=%q", loginPage.Code, loginPage.Body.String())
	}
	if denied := adminResponse(handler, http.MethodGet, "/jaws/"+signedKey, nil, adminCookie, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("revoked admin key remained usable: status=%d", denied.Code)
	}
}

func TestAdminLoginSubmitClosesJaWSSocket(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable for the browser hook check")
	}
	const check = `
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const script = fs.readFileSync(process.argv[1], 'utf8');
const calls = [];
class WebSocket { removeEventListener(type, fn) { calls.push('socket:' + type + ':' + fn.name); } }
const jaws = new WebSocket();
let submit;
const document = { querySelector(selector) {
    assert.equal(selector, 'form[action="/admin/login"]');
    return { addEventListener(type, fn) { assert.equal(type, 'submit'); submit = fn; } };
} };
const window = { removeEventListener(type, fn) { calls.push('window:' + type + ':' + fn.name); } };
function jawsConnectWhenReady() {}
function jawsMessage() {}
function jawsUnloading() { calls.push('close'); }
vm.runInNewContext(script, { document, window, WebSocket, jaws, jawsMessage, jawsConnectWhenReady, jawsUnloading });
assert.equal(typeof submit, 'function');
submit();
assert.deepEqual(calls, [
    'window:DOMContentLoaded:jawsConnectWhenReady',
    'window:load:jawsConnectWhenReady',
    'socket:message:jawsMessage',
    'close',
]);
`
	command := exec.CommandContext(t.Context(), node, "-e", check, "admin_login.js")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("login submit script: %v: %s", err, output)
	}
}

func TestAdminGuestLoginWebSocket(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := server.Client()
	guestPage := func() (*http.Cookie, string) {
		t.Helper()
		response, err := client.Get(server.URL + "/admin/login")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK || len(response.Cookies()) != 1 {
			t.Fatalf("network guest page: status=%d cookies=%v read=%v", response.StatusCode, response.Cookies(), err)
		}
		return response.Cookies()[0], adminJaWSKey(t, string(body))
	}
	firstCookie, firstKey := guestPage()
	secondCookie, _ := guestPage()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	connect := func(cookie *http.Cookie, key string) (*websocket.Conn, *http.Response, error) {
		header := http.Header{}
		header.Set("Cookie", cookie.Name+"="+cookie.Value)
		header.Set("Origin", server.URL)
		return websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/jaws/"+key,
			&websocket.DialOptions{HTTPHeader: header})
	}
	if conn, response, err := connect(secondCookie, firstKey); err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		if conn != nil {
			conn.Close(websocket.StatusNormalClosure, "")
		}
		t.Fatalf("same-IP guest claimed another key: response=%v error=%v", response, err)
	} else {
		response.Body.Close()
	}
	conn, response, err := connect(firstCookie, firstKey)
	if err != nil || response == nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("guest WebSocket: response=%v error=%v", response, err)
	}
	readDone := make(chan struct{})
	defer func() { conn.CloseNow(); <-readDone }()
	go func() {
		defer close(readDone)
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("guest WebSocket did not stay live: %v", err)
	}
	getWithCookie := func() int {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/admin/login", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(firstCookie)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read guest page: read=%v close=%v", readErr, closeErr)
		}
		return response.StatusCode
	}
	for range maxGuestPageRequests - 1 {
		if status := getWithCookie(); status != http.StatusOK {
			t.Fatalf("guest page before request cap: status=%d", status)
		}
	}
	if status := getWithCookie(); status != http.StatusTooManyRequests {
		t.Fatalf("guest page past request cap: status=%d, want 429", status)
	}
	conn.CloseNow()
	<-readDone
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := getWithCookie()
		if status == http.StatusOK {
			break
		}
		if status != http.StatusTooManyRequests || time.Now().After(deadline) {
			t.Fatalf("guest page after socket close: status=%d", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	form := url.Values{"username": {"admin"}, "password": {"correct-password"}}
	postCtx, postCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer postCancel()
	request, err := http.NewRequestWithContext(postCtx, http.MethodPost, server.URL+"/admin/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(firstCookie)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || len(response.Cookies()) != 1 {
		t.Fatalf("sign-in after guest socket closes: status=%d cookies=%v", response.StatusCode, response.Cookies())
	}
	adminCookie := response.Cookies()[0]
	signedRequest, err := http.NewRequestWithContext(postCtx, http.MethodGet, server.URL+"/admin/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	signedRequest.AddCookie(adminCookie)
	signedResponse, err := client.Do(signedRequest)
	if err != nil {
		t.Fatal(err)
	}
	signedBody, readErr := io.ReadAll(signedResponse.Body)
	closeErr := signedResponse.Body.Close()
	if readErr != nil || closeErr != nil || signedResponse.StatusCode != http.StatusOK ||
		strings.Contains(string(signedBody), `<script defer src="/admin/login.js"></script>`) {
		t.Fatalf("signed-in network page: status=%d read=%v close=%v body=%q", signedResponse.StatusCode, readErr, closeErr, signedBody)
	}
	adminKey := adminJaWSKey(t, string(signedBody))
	adminConn, upgrade, err := connect(adminCookie, adminKey)
	if err != nil || upgrade == nil || upgrade.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("signed-in WebSocket: response=%v error=%v", upgrade, err)
	}
	defer adminConn.CloseNow()
	logoutRequest, err := http.NewRequestWithContext(postCtx, http.MethodPost, server.URL+"/admin/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	logoutRequest.AddCookie(adminCookie)
	logoutResponse, err := client.Do(logoutRequest)
	if err != nil {
		t.Fatal(err)
	}
	logoutResponse.Body.Close()
	if logoutResponse.StatusCode != http.StatusSeeOther || len(logoutResponse.Cookies()) != 0 {
		t.Fatalf("sign-out with live admin socket: status=%d cookies=%v", logoutResponse.StatusCode, logoutResponse.Cookies())
	}
	for {
		_, message, err := adminConn.Read(postCtx)
		if err != nil {
			t.Fatalf("live admin socket closed without Reload: %v", err)
		}
		if strings.Contains(string(message), "Reload\t") {
			break
		}
	}
	reload, err := http.NewRequestWithContext(postCtx, http.MethodGet, server.URL+"/admin/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	reload.AddCookie(adminCookie)
	reloaded, err := client.Do(reload)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Body.Close()
	body, err := io.ReadAll(reloaded.Body)
	if err != nil || reloaded.StatusCode != http.StatusOK || len(reloaded.Cookies()) != 1 || !strings.Contains(string(body), `name="password"`) {
		t.Fatalf("live socket reload guest page: status=%d cookies=%v read=%v body=%q", reloaded.StatusCode, reloaded.Cookies(), err, body)
	}
}

func TestAdminGuestSessionLimit(t *testing.T) {
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	jw.MaxSessions, jw.MaxSessionsPerIP = 1, 0
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	defer func() { jw.Close(); <-done }()
	if head := adminResponse(handler, http.MethodHead, "/admin/login", nil, nil, ""); head.Code != http.StatusOK ||
		len(head.Result().Cookies()) != 0 || len(jw.Sessions()) != 0 {
		t.Fatalf("HEAD created guest state: status=%d cookies=%v sessions=%d", head.Code, head.Result().Cookies(), len(jw.Sessions()))
	}
	first := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if first.Code != http.StatusOK || len(first.Result().Cookies()) != 1 {
		t.Fatalf("first guest session: status=%d cookies=%v", first.Code, first.Result().Cookies())
	}
	if denied := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, ""); denied.Code != http.StatusServiceUnavailable || len(denied.Result().Cookies()) != 0 {
		t.Fatalf("global session cap: status=%d cookies=%v", denied.Code, denied.Result().Cookies())
	}
	guestCookie := first.Result().Cookies()[0]
	existing := adminResponse(handler, http.MethodGet, "/admin/login", nil, guestCookie, "")
	if existing.Code != http.StatusOK || len(existing.Result().Cookies()) != 0 {
		t.Fatalf("session cap blocked existing guest or replaced its cookie: status=%d cookies=%v", existing.Code, existing.Result().Cookies())
	}
	login := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"username": {"admin"}, "password": {"correct-password"}}, guestCookie, "https://admin.example")
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("valid login at session cap: status=%d cookies=%v", login.Code, login.Result().Cookies())
	}
	adminCookie := login.Result().Cookies()[0]
	if adminCookie.Value == guestCookie.Value {
		t.Fatal("login did not rotate guest cookie")
	}
	if dashboard := adminResponse(handler, http.MethodGet, "/", nil, adminCookie, ""); dashboard.Code != http.StatusOK {
		t.Fatalf("new admin cookie cannot open dashboard: status=%d", dashboard.Code)
	}
}

func TestHomeReservesSessionsForSignIn(t *testing.T) {
	handler, jw, _ := testOAuthAdminHandler(t, config.AdminConfig{
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/github/callback"},
	}, nil)
	for jw.SessionCount() < maxGuestPageSessions-1 {
		if jw.NewSession(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "https://admin.example/", nil)) == nil {
			t.Fatal("could not fill guest session reserve")
		}
	}
	home := adminResponse(handler, http.MethodGet, "/", nil, nil, "")
	if home.Code != http.StatusOK || len(home.Result().Cookies()) != 1 || jw.SessionCount() != maxGuestPageSessions {
		t.Fatalf("home admission: status=%d sessions=%d cookies=%v", home.Code, jw.SessionCount(), home.Result().Cookies())
	}
	if start := adminResponse(handler, http.MethodGet, "/auth/github/login", nil, home.Result().Cookies()[0], ""); start.Code != http.StatusFound {
		t.Fatalf("last home page cannot start OAuth: status=%d", start.Code)
	}
	response := adminResponse(handler, http.MethodGet, "/", nil, nil, "")
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "60" || len(response.Result().Cookies()) != 0 || jw.SessionCount() != maxGuestPageSessions {
		t.Fatalf("home exhausted sign-in reserve: status=%d sessions=%d cookies=%v", response.Code, jw.SessionCount(), response.Result().Cookies())
	}
}

func TestSignedInPublicPagesAreNotGuestLimited(t *testing.T) {
	cfg := config.AdminConfig{
		Administrators: []config.AdminAccount{testAdminAccount()},
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/github/callback"},
	}
	handler, jw, _ := testOAuthAdminHandler(t, cfg, nil)
	key := (userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key()
	cookie := userUsageCookie(t, jw, key, false)
	for range maxGuestPageRequests {
		page := userUsageRequest(handler, http.MethodGet, "/auth/login", "", cookie)
		if page.Code != http.StatusOK {
			t.Fatalf("OAuth account page status=%d", page.Code)
		}
	}
	if page := userUsageRequest(handler, http.MethodGet, "/auth/login", "", cookie); page.Code != http.StatusOK {
		t.Fatalf("fifth OAuth account page status=%d", page.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "https://admin.example/", nil)
	r.RemoteAddr = "192.0.2.7:12345"
	r.AddCookie(cookie)
	sess := jw.GetSession(r)
	if sess == nil {
		t.Fatal("signed-in OAuth session missing")
	}
	if len(sess.Requests()) < maxGuestPageRequests {
		t.Fatalf("signed-in OAuth pages=%d, want at least %d", len(sess.Requests()), maxGuestPageRequests)
	}
	home := userUsageRequest(handler, http.MethodGet, "/", "", cookie)
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), `href="/admin/login"`) ||
		!strings.Contains(home.Body.String(), `href="/auth/login">Your account</a>`) {
		t.Fatalf("signed-in OAuth home: status=%d body=%q", home.Code, home.Body.String())
	}
	if login := userUsageRequest(handler, http.MethodGet, "/admin/login", "", cookie); login.Code != http.StatusOK {
		t.Fatalf("admin login for OAuth user: status=%d body=%q", login.Code, login.Body.String())
	}
	adminCookie := userUsageCookie(t, jw, "", true)
	for range maxGuestPageRequests {
		page := userUsageRequest(handler, http.MethodGet, "/admin/login", "", adminCookie)
		if page.Code != http.StatusOK {
			t.Fatalf("admin account page status=%d", page.Code)
		}
	}
	if page := userUsageRequest(handler, http.MethodGet, "/admin/login", "", adminCookie); page.Code != http.StatusOK {
		t.Fatalf("fifth admin account page status=%d", page.Code)
	}
	if page := userUsageRequest(handler, http.MethodGet, "/auth/login", "", adminCookie); page.Code != http.StatusOK {
		t.Fatalf("OAuth login page for admin: status=%d body=%q", page.Code, page.Body.String())
	}
}

func TestHomeWithoutOAuthReservesSessionsForAdminSignIn(t *testing.T) {
	handler, jw, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	for jw.SessionCount() < maxGuestPageSessions-1 {
		if jw.NewSession(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "https://admin.example/", nil)) == nil {
			t.Fatal("could not fill guest sessions")
		}
	}
	home := adminResponse(handler, http.MethodGet, "/", nil, nil, "")
	if home.Code != http.StatusOK || len(home.Result().Cookies()) != 1 || jw.SessionCount() != maxGuestPageSessions {
		t.Fatalf("admin-only home: status=%d sessions=%d cookies=%v", home.Code, jw.SessionCount(), home.Result().Cookies())
	}
	if existing := adminResponse(handler, http.MethodGet, "/", nil, home.Result().Cookies()[0], ""); existing.Code != http.StatusOK || len(existing.Result().Cookies()) != 0 || jw.SessionCount() != maxGuestPageSessions {
		t.Fatalf("existing home session: status=%d sessions=%d cookies=%v", existing.Code, jw.SessionCount(), existing.Result().Cookies())
	}
	denied := adminResponse(handler, http.MethodGet, "/", nil, nil, "")
	if denied.Code != http.StatusServiceUnavailable || denied.Header().Get("Retry-After") != "60" ||
		len(denied.Result().Cookies()) != 0 || jw.SessionCount() != maxGuestPageSessions {
		t.Fatalf("home exhausted admin sign-in reserve: status=%d sessions=%d cookies=%v", denied.Code, jw.SessionCount(), denied.Result().Cookies())
	}
	login := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 || jw.SessionCount() != maxGuestAdmissionSessions {
		t.Fatalf("admin sign-in at home reserve: status=%d sessions=%d cookies=%v", login.Code, jw.SessionCount(), login.Result().Cookies())
	}
}

func TestHomeSharesOAuthGuestAdmissionLock(t *testing.T) {
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/github/callback"}}
	handler, _, auth := testOAuthAdminHandler(t, cfg, nil)
	auth.admissionMu.Lock()
	locked := true
	defer func() {
		if locked {
			auth.admissionMu.Unlock()
		}
	}()
	started := make(chan struct{})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		close(started)
		done <- adminResponse(handler, http.MethodGet, "/", nil, nil, "")
	}()
	<-started
	select {
	case <-done:
		t.Fatal("home bypassed OAuth guest admission lock")
	case <-time.After(50 * time.Millisecond):
	}
	auth.admissionMu.Unlock()
	locked = false
	select {
	case response := <-done:
		if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 {
			t.Fatalf("home after admission unlock: status=%d cookies=%v", response.Code, response.Result().Cookies())
		}
	case <-time.After(time.Second):
		t.Fatal("home did not resume after admission unlock")
	}
}

func TestAdminLoginRequiresLiveSession(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	form := url.Values{"username": {"admin"}, "password": {"correct-password"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusConflict || len(response.Result().Cookies()) != 0 {
		t.Fatalf("cookie-free login: status=%d cookies=%v, want 409 without session", response.Code, response.Result().Cookies())
	}
	guestCookie := adminGuestCookie(t, handler)
	if response := adminResponse(handler, http.MethodPost, "/admin/logout", nil, guestCookie, "https://admin.example"); response.Code != http.StatusSeeOther {
		t.Fatalf("close guest session: status=%d", response.Code)
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, guestCookie, ""); response.Code != http.StatusConflict || len(response.Result().Cookies()) != 0 {
		t.Fatalf("revoked guest login: status=%d cookies=%v, want 409 without session", response.Code, response.Result().Cookies())
	}
}

func TestAdminReloginRetiresOldSession(t *testing.T) {
	handler, jw, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	form := url.Values{"username": {"admin"}, "password": {"correct-password"}}
	first := adminResponse(handler, http.MethodPost, "/admin/login", form, adminGuestCookie(t, handler), "")
	if first.Code != http.StatusSeeOther || len(first.Result().Cookies()) != 1 {
		t.Fatalf("first login: status=%d cookies=%v", first.Code, first.Result().Cookies())
	}
	oldCookie := first.Result().Cookies()[0]
	second := adminResponse(handler, http.MethodPost, "/admin/login", form, oldCookie, "")
	if second.Code != http.StatusSeeOther || len(second.Result().Cookies()) != 1 {
		t.Fatalf("relogin: status=%d cookies=%v", second.Code, second.Result().Cookies())
	}
	if jw.GetSession(requestWithCookie(oldCookie)) != nil {
		t.Fatal("old admin session remained live after sign-in")
	}
	if dashboard := adminResponse(handler, http.MethodGet, "/", nil, second.Result().Cookies()[0], ""); dashboard.Code != http.StatusOK {
		t.Fatalf("replacement admin cookie cannot open dashboard: status=%d", dashboard.Code)
	}
	currentCookie := second.Result().Cookies()[0]
	if logout := adminResponse(handler, http.MethodPost, "/admin/logout", nil, currentCookie, "https://admin.example"); logout.Code != http.StatusSeeOther {
		t.Fatalf("logout before relogin: status=%d", logout.Code)
	}
	if denied := adminResponse(handler, http.MethodPost, "/admin/login", form, currentCookie, ""); denied.Code != http.StatusConflict || len(denied.Result().Cookies()) != 0 {
		t.Fatalf("revoked admin session relogged in: status=%d cookies=%v", denied.Code, denied.Result().Cookies())
	}
}

func TestCheckedInAdminPlaceholderCannotSignIn(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin == nil || len(cfg.Admin.Administrators) != 1 {
		t.Fatal("checked-in admin placeholder account is missing")
	}
	placeholder := cfg.Admin.Administrators[0]
	if placeholder.Username != "admin" || placeholder.PasswordHash != config.AdminPasswordHashPlaceholder {
		t.Fatal("checked-in account must be the disabled admin placeholder")
	}
	handler, _, _ := testAdminHandler(t, cfg.Admin.Administrators)
	home := adminResponse(handler, http.MethodGet, "/", nil, nil, "")
	if home.Code != http.StatusOK || len(home.Result().Cookies()) != 1 ||
		!strings.Contains(home.Body.String(), "The admin password is not set") ||
		!strings.Contains(home.Body.String(), "minion -mode=hash") ||
		strings.Contains(home.Body.String(), `href="/admin/login"`) ||
		strings.Contains(home.Body.String(), `href="/auth/login"`) {
		t.Fatalf("sample home omitted password setup guidance or enabled sign-in: status=%d cookies=%v body=%q", home.Code, home.Result().Cookies(), home.Body.String())
	}
	if asset := adminResponse(handler, http.MethodGet, bootstrapAssetPath(t, home.Body.String(), ".css"), nil, nil, ""); asset.Code != http.StatusOK {
		t.Fatalf("sample home Bootstrap asset: status=%d", asset.Code)
	}
	if tail := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+adminJaWSKey(t, home.Body.String()), nil, home.Result().Cookies()[0], ""); tail.Code != http.StatusOK {
		t.Fatalf("sample home JaWS tail: status=%d", tail.Code)
	}
	loginPage := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if loginPage.Code != http.StatusOK || len(loginPage.Result().Cookies()) != 1 ||
		!strings.Contains(loginPage.Body.String(), "The admin password is not set") ||
		strings.Contains(loginPage.Body.String(), `name="password"`) ||
		strings.Contains(loginPage.Body.String(), `/admin/login.js`) {
		t.Fatalf("sample login page: status=%d cookies=%v body=%q", loginPage.Code, loginPage.Result().Cookies(), loginPage.Body.String())
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"username": {"admin"}, "password": {"PASTE_GENERATED_HASH_HERE"}}, loginPage.Result().Cookies()[0], ""); response.Code != http.StatusForbidden || len(response.Result().Cookies()) != 0 {
		t.Fatalf("sample admin password sign-in: status=%d cookies=%d, want 403", response.Code, len(response.Result().Cookies()))
	}
}

func TestUnsetAdminPasswordShowsSetupAndDeniesLogin(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{{Username: "admin"}})
	for _, path := range []string{"/", "/admin/login"} {
		page := adminResponse(handler, http.MethodGet, path, nil, nil, "")
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "The admin password is not set") ||
			!strings.Contains(page.Body.String(), "minion -mode=hash") ||
			strings.Contains(page.Body.String(), `name="password"`) ||
			strings.Contains(page.Body.String(), `href="/admin/login"`) {
			t.Fatalf("unset password page %s: status=%d body=%q", path, page.Code, page.Body.String())
		}
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"username": {"admin"}, "password": {"guess"}}, adminGuestCookie(t, handler), ""); response.Code != http.StatusForbidden || len(response.Result().Cookies()) != 0 {
		t.Fatalf("unset password sign-in: status=%d cookies=%d, want 403", response.Code, len(response.Result().Cookies()))
	}
}

func TestAdminLoginAndRevocation(t *testing.T) {
	account := testAdminAccount()
	handler, jw, logs := testAdminHandler(t, []config.AdminAccount{account})
	guestPage := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if guestPage.Code != http.StatusOK || len(guestPage.Result().Cookies()) != 1 ||
		!strings.Contains(guestPage.Body.String(), `name="password"`) || guestPage.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("login page: status=%d cookies=%v noStore=%t hasPasswordField=%t", guestPage.Code,
			guestPage.Result().Cookies(), guestPage.Header().Get("Cache-Control") == "no-store", strings.Contains(guestPage.Body.String(), `name="password"`))
	}
	guestCookie := guestPage.Result().Cookies()[0]
	if response := adminResponse(handler, http.MethodHead, "/admin/login", nil, nil, ""); response.Code != http.StatusOK {
		t.Fatalf("public HEAD login: status=%d, want 200", response.Code)
	}
	form := url.Values{"username": {"admin"}, "password": {"wrong-password"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusUnauthorized ||
		len(response.Result().Cookies()) != 0 || strings.Contains(response.Body.String(), "wrong-password") {
		t.Fatalf("invalid login: status=%d cookieCount=%d echoedPassword=%t", response.Code,
			len(response.Result().Cookies()), strings.Contains(response.Body.String(), "wrong-password"))
	}
	form.Set("password", "correct-password")
	response := adminResponse(handler, http.MethodPost, "/admin/login", form, guestCookie, "")
	if response.Code != http.StatusSeeOther || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Location") != "/" {
		t.Fatalf("valid login: status=%d noStore=%t location=%q", response.Code,
			response.Header().Get("Cache-Control") == "no-store", response.Header().Get("Location"))
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("admin session cookie count=%d, want 1", len(cookies))
	}
	if cookies[0].Name != jw.CookieName || !cookies[0].Secure || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Path != "/" {
		t.Fatalf("admin session cookie attributes: nameMatches=%t secure=%t httpOnly=%t sameSiteLax=%t pathRoot=%t",
			cookies[0].Name == jw.CookieName, cookies[0].Secure, cookies[0].HttpOnly,
			cookies[0].SameSite == http.SameSiteLaxMode, cookies[0].Path == "/")
	}
	cookie := cookies[0]
	if response := adminResponse(handler, http.MethodGet, "/admin/login", nil, cookie, ""); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Signed in") {
		t.Fatalf("authenticated page: status=%d signedIn=%t", response.Code, strings.Contains(response.Body.String(), "Signed in"))
	}
	request := httptest.NewRequest(http.MethodGet, "https://admin.example/jaws/.ping", nil)
	request.TLS = &tls.ConnectionState{}
	request.AddCookie(cookie)
	jaWSRequest := jw.NewRequest(httptest.NewRecorder(), request)
	if !jw.MakeAuth(jaWSRequest).IsAdmin() {
		t.Fatal("JaWS template authorization did not use the admin session")
	}
	session := jaWSRequest.Session()
	if session == nil || session.Get(adminSessionKey) != "admin" {
		t.Fatal("admin session did not contain the signed-in identity")
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://other.example"); response.Code != http.StatusForbidden || response.Header().Get("Strict-Transport-Security") != "" {
		t.Fatalf("cross-origin logout: status=%d hasHSTS=%t", response.Code, response.Header().Get("Strict-Transport-Security") != "")
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "http://admin.example"); response.Code != http.StatusForbidden {
		t.Fatalf("HTTP origin on TLS logout: status=%d, want 403", response.Code)
	}
	if response := adminResponse(handler, http.MethodGet, "/admin/login", nil, cookie, ""); !strings.Contains(response.Body.String(), "Signed in") {
		t.Fatal("cross-origin logout revoked the active session")
	}
	response = adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example")
	if response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 0 {
		t.Fatalf("logout: status=%d cookieCount=%d", response.Code, len(response.Result().Cookies()))
	}
	if jw.MakeAuth(jaWSRequest).IsAdmin() {
		t.Fatal("revoked JaWS request retained admin template authorization")
	}
	if session.Get(adminSessionKey) != nil {
		t.Fatal("logout retained the admin identity in the closed session")
	}
	if response := adminResponse(handler, http.MethodPost, "/jaws/invalid-key", nil, cookie, ""); response.Code != http.StatusForbidden {
		t.Errorf("revoked POST /jaws/invalid-key: status=%d, want 403", response.Code)
	}
	if response := adminResponse(handler, http.MethodGet, "/jaws/.ping", nil, cookie, ""); response.Code != http.StatusNoContent {
		t.Errorf("public JaWS ping after logout: status=%d, want 204", response.Code)
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatalf("revoked logout: status=%d location=%q", response.Code, response.Header().Get("Location"))
	}
	wsRequest := httptest.NewRequest(http.MethodGet, "https://admin.example/jaws/test-websocket", nil)
	wsRequest.TLS = &tls.ConnectionState{}
	wsRequest.AddCookie(cookie)
	wsRequest.Header.Set("Connection", "Upgrade")
	wsRequest.Header.Set("Upgrade", "websocket")
	wsResponse := httptest.NewRecorder()
	handler.ServeHTTP(wsResponse, wsRequest)
	if wsResponse.Code != http.StatusForbidden {
		t.Fatalf("revoked WebSocket upgrade: status=%d, want 403", wsResponse.Code)
	}
	if response := adminResponse(handler, http.MethodGet, "/admin/login", nil, cookie, ""); strings.Contains(response.Body.String(), "Signed in") {
		t.Fatal("revoked cookie still opens signed-in page")
	}
	if strings.Contains(logs.String(), "correct-password") || strings.Contains(logs.String(), "wrong-password") ||
		strings.Contains(logs.String(), account.PasswordHash) {
		t.Fatal("credentials appeared in JaWS logs")
	}
}

func TestAdminServerLoginOverConfiguredTLS(t *testing.T) {
	certFile, keyFile, leaf := remoteTestTLSFiles(t)
	admin, err := startAdminServer(t.Context(), config.AdminConfig{
		Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile,
		Administrators: []config.AdminAccount{testAdminAccount()},
	}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client := adminTLSClient(t, leaf)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	baseURL := "https://" + admin.listener.Addr().String()
	response, _ := adminNetworkLogin(t, client, baseURL, url.Values{"username": {"admin"}, "password": {"correct-password"}})
	defer response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || len(response.Cookies()) != 1 || !response.Cookies()[0].Secure {
		t.Fatalf("TLS login: status=%d cookieCount=%d", response.StatusCode, len(response.Cookies()))
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/admin/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(response.Cookies()[0])
	page, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	pageBody, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatal(err)
	}
	if page.StatusCode != http.StatusOK || !strings.Contains(string(pageBody), "Signed in") {
		t.Fatalf("authenticated TLS page: status=%d signedIn=%t", page.StatusCode, strings.Contains(string(pageBody), "Signed in"))
	}
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/jaws/.ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(response.Cookies()[0])
	allowed, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer allowed.Body.Close()
	if allowed.StatusCode != http.StatusNoContent {
		t.Fatalf("authenticated JaWS ping: status=%d, want 204", allowed.StatusCode)
	}
}

func TestAdminServerLoginOnLoopbackHTTP(t *testing.T) {
	admin, err := startAdminServer(t.Context(), config.AdminConfig{
		Listen: "127.0.0.1:0", Administrators: []config.AdminAccount{testAdminAccount()},
	}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client := &http.Client{Transport: &http.Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	baseURL := "http://" + admin.listener.Addr().String()
	_, port, err := net.SplitHostPort(admin.listener.Addr().String())
	if err != nil || admin.entryURL != "http://127.0.0.1:"+port {
		t.Fatalf("admin home URL = %q, port=%q: %v", admin.entryURL, port, err)
	}
	entry, err := client.Get(baseURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer entry.Body.Close()
	entryBody, err := io.ReadAll(entry.Body)
	if err != nil {
		t.Fatal(err)
	}
	if entry.StatusCode != http.StatusOK || !strings.Contains(string(entryBody), `href="/admin/login"`) ||
		!strings.Contains(string(entryBody), `name="jawsKey"`) {
		t.Fatalf("loopback home: status=%d body=%q", entry.StatusCode, entryBody)
	}
	response, _ := adminNetworkLogin(t, client, baseURL, url.Values{"username": {"admin"}, "password": {"correct-password"}})
	defer response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || len(response.Cookies()) != 1 || response.Cookies()[0].Secure {
		t.Fatalf("loopback login: status=%d cookieCount=%d", response.StatusCode, len(response.Cookies()))
	}
	crossScheme, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/admin/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	crossScheme.AddCookie(response.Cookies()[0])
	crossScheme.Header.Set("Origin", "https://"+admin.listener.Addr().String())
	rejected, err := client.Do(crossScheme)
	if err != nil {
		t.Fatal(err)
	}
	defer rejected.Body.Close()
	if rejected.StatusCode != http.StatusForbidden {
		t.Fatalf("HTTPS origin on loopback HTTP logout: status=%d, want 403", rejected.StatusCode)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/admin/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(response.Cookies()[0])
	page, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	pageBody, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatal(err)
	}
	if page.StatusCode != http.StatusOK || !strings.Contains(string(pageBody), "Signed in") {
		t.Fatalf("authenticated loopback page: status=%d signedIn=%t", page.StatusCode, strings.Contains(string(pageBody), "Signed in"))
	}
}

func TestAdminUnreadBodiesCloseWithoutWaiting(t *testing.T) {
	admin, err := startAdminServer(t.Context(), config.AdminConfig{
		Listen: "127.0.0.1:0", Administrators: []config.AdminAccount{testAdminAccount()},
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret", RedirectURL: "http://127.0.0.1/auth/github/callback"},
	}, nil, filepath.Join(t.TempDir(), "state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, _ := adminNetworkLogin(t, client, "http://"+admin.listener.Addr().String(),
		url.Values{"username": {"admin"}, "password": {"correct-password"}})
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || len(response.Cookies()) != 1 {
		t.Fatalf("login: status=%d cookieCount=%d", response.StatusCode, len(response.Cookies()))
	}
	page := adminResponse(admin.server.Handler, http.MethodGet, "/admin/login", nil, nil, "")
	if page.Code != http.StatusOK {
		t.Fatalf("Bootstrap entry page: status=%d", page.Code)
	}
	cssPath := bootstrapAssetPath(t, page.Body.String(), ".css")
	cookie := response.Cookies()[0]
	oauthPage, err := client.Get("http://" + admin.listener.Addr().String() + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	defer oauthPage.Body.Close()
	if oauthPage.StatusCode != http.StatusOK || len(oauthPage.Cookies()) != 1 {
		t.Fatalf("OAuth login page: status=%d cookies=%v", oauthPage.StatusCode, oauthPage.Cookies())
	}
	oauthCookie := oauthPage.Cookies()[0]
	for _, tc := range []struct {
		name, method, path, headers string
		want                        int
		location                    string
	}{
		{name: "public GET", method: http.MethodGet, path: "/admin/login", want: http.StatusOK},
		{name: "OAuth callback", method: http.MethodGet, path: "/auth/github/callback?state=wrong",
			headers: "Cookie: " + oauthCookie.Name + "=" + oauthCookie.Value + "\r\n", want: http.StatusSeeOther,
			location: "/auth/login?error=request"},
		{name: "cross-origin login", method: http.MethodPost, path: "/admin/login",
			headers: "Origin: http://other.example\r\n", want: http.StatusForbidden},
		{name: "unsupported login body", method: http.MethodPost, path: "/admin/login",
			headers: "Content-Type: text/plain\r\n", want: http.StatusBadRequest},
		{name: "authenticated JaWS rejection", method: http.MethodPost, path: "/jaws/.ping",
			headers: "Cookie: " + cookie.Name + "=" + cookie.Value + "\r\n", want: http.StatusForbidden},
		{name: "authenticated dashboard", method: http.MethodGet, path: "/",
			headers: "Cookie: " + cookie.Name + "=" + cookie.Value + "\r\n", want: http.StatusOK},
		{name: "authenticated stylesheet", method: http.MethodGet, path: cssPath,
			headers: "Cookie: " + cookie.Name + "=" + cookie.Value + "\r\n", want: http.StatusOK},
		{name: "authenticated usage page", method: http.MethodGet, path: "/admin/usage",
			headers: "Cookie: " + cookie.Name + "=" + cookie.Value + "\r\n", want: http.StatusServiceUnavailable},
		{name: "authenticated usage export", method: http.MethodGet, path: "/admin/usage.json",
			headers: "Cookie: " + cookie.Name + "=" + cookie.Value + "\r\n", want: http.StatusServiceUnavailable},
		{name: "cross-origin logout", method: http.MethodPost, path: "/admin/logout",
			headers: "Origin: http://other.example\r\nCookie: " + cookie.Name + "=" + cookie.Value + "\r\n", want: http.StatusForbidden},
		{name: "logout", method: http.MethodPost, path: "/admin/logout",
			headers: "Cookie: " + cookie.Name + "=" + cookie.Value + "\r\n", want: http.StatusSeeOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", admin.listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			request := tc.method + " " + tc.path + " HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 100\r\n" + tc.headers + "\r\n"
			if _, err := io.WriteString(conn, request); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: tc.method})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want || !resp.Close {
				t.Fatalf("incomplete body: status=%d close=%t, want %d and close", resp.StatusCode, resp.Close, tc.want)
			}
			if tc.location != "" && resp.Header.Get("Location") != tc.location {
				t.Fatalf("redirect location=%q, want %q", resp.Header.Get("Location"), tc.location)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("server kept incomplete request open: %v", err)
			}
		})
	}
}

func TestAdminWithoutConfiguredAccountsServesPublicHome(t *testing.T) {
	handler, jw, _ := testAdminHandler(t, nil)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/admin/login"},
		{http.MethodPost, "/admin/logout"},
		{http.MethodGet, "/jaws/test-websocket"}, {http.MethodPost, "/jaws/invalid-key"},
	} {
		if response := adminResponse(handler, tc.method, tc.path, nil, nil, ""); response.Code != http.StatusForbidden ||
			len(response.Result().Cookies()) != 0 {
			t.Errorf("unconfigured %s %s: status=%d cookieCount=%d", tc.method, tc.path, response.Code, len(response.Result().Cookies()))
		}
	}
	if home := adminResponse(handler, http.MethodGet, "/", nil, nil, ""); home.Code != http.StatusOK ||
		!strings.Contains(home.Body.String(), "The admin password is not set") {
		t.Fatalf("unconfigured home: status=%d body=%q", home.Code, home.Body.String())
	}
	if login := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, ""); login.Code != http.StatusOK ||
		!strings.Contains(login.Body.String(), "The admin password is not set") {
		t.Fatalf("unconfigured login: status=%d body=%q", login.Code, login.Body.String())
	}
	if head := adminResponse(handler, http.MethodHead, "/admin/login", nil, nil, ""); head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("unconfigured login HEAD: status=%d body=%q", head.Code, head.Body.String())
	}
	if ping := adminResponse(handler, http.MethodGet, "/jaws/.ping", nil, nil, ""); ping.Code != http.StatusNoContent {
		t.Fatalf("public JaWS ping: status=%d", ping.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "https://admin.example/jaws/.ping", nil)
	if jw.MakeAuth(jw.NewRequest(httptest.NewRecorder(), request)).IsAdmin() {
		t.Fatal("unconfigured JaWS template authorization is open")
	}
}

func TestAdminLoginBoundsAndCrossOrigin(t *testing.T) {
	account := testAdminAccount()
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{account})
	form := url.Values{"username": {"admin"}, "password": {"wrong-password"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, "https://other.example"); response.Code != http.StatusForbidden || response.Header().Get("Strict-Transport-Security") != "" {
		t.Fatalf("cross-origin login: status=%d hasHSTS=%t", response.Code, response.Header().Get("Strict-Transport-Security") != "")
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, "http://admin.example"); response.Code != http.StatusForbidden {
		t.Fatalf("HTTP origin on TLS login: status=%d, want 403", response.Code)
	}
	for range 5 {
		if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusUnauthorized {
			t.Fatalf("invalid login before limit: status=%d", response.Code)
		}
	}
	form.Set("password", "correct-password")
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusTooManyRequests {
		t.Fatalf("login after five failures: status=%d, want 429", response.Code)
	}
	form = url.Values{"username": {"admin"}, "password": {strings.Repeat("x", 8192)}}
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("oversized password: status=%d, want 400", response.Code)
	}
}

func TestAdminHashCapacityReturnsBusy(t *testing.T) {
	access := &adminAccess{hashSlots: make(chan struct{}, 2)}
	access.hashSlots <- struct{}{}
	access.hashSlots <- struct{}{}
	request := httptest.NewRequest(http.MethodPost, "/admin/login",
		strings.NewReader(url.Values{"username": {"admin"}, "password": {"password"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	access.login(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("third concurrent hash: status=%d, want 503", response.Code)
	}
}

func TestAdminLoginBodyBoundaryAndDuplicateFields(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	for _, form := range []url.Values{
		{"username": {"admin", "admin"}, "password": {"correct-password"}},
		{"username": {"admin"}, "password": {"correct-password", "correct-password"}},
	} {
		if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusBadRequest || len(response.Result().Cookies()) != 0 {
			t.Fatalf("duplicate credential field: status=%d cookieCount=%d", response.Code, len(response.Result().Cookies()))
		}
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"username": {strings.Repeat("x", 101)}, "password": {"wrong"}}, nil, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("username beyond config limit: status=%d, want 400", response.Code)
	}
	for _, tc := range []struct{ name, password string }{
		{name: "reserved", password: strings.Repeat("&", 64)},
		{name: "non-ASCII", password: strings.Repeat("😀", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"username": {"admin"}, "password": {tc.password}}
			if got := len(form.Encode()); got <= len(tc.password) || got >= 2<<10 {
				t.Fatalf("escaped valid password: password bytes=%d form bytes=%d", len(tc.password), got)
			}
			escapedHandler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccountWithPassword(tc.password)})
			if response := adminResponse(escapedHandler, http.MethodPost, "/admin/login", form, adminGuestCookie(t, escapedHandler), ""); response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 {
				t.Fatalf("escaped valid password: status=%d cookieCount=%d, want login", response.Code, len(response.Result().Cookies()))
			}
		})
	}
	for _, password := range []string{strings.Repeat("a", 65), strings.Repeat("😀", 65), string([]byte{0xff})} {
		if response := adminResponse(handler, http.MethodPost, "/admin/login",
			url.Values{"username": {"admin"}, "password": {password}}, nil, ""); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid password: bytes=%d status=%d, want 400", len(password), response.Code)
		}
	}
	form := url.Values{"username": {"unknown"}, "password": {"wrong"}, "padding": {""}}
	form.Set("padding", strings.Repeat("x", (2<<10)-len(form.Encode())))
	if got := len(form.Encode()); got != 2<<10 {
		t.Fatalf("fixture body length=%d, want 2048", got)
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("2048-byte login: status=%d, want 401", response.Code)
	}
	form.Set("padding", form.Get("padding")+"x")
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("2049-byte login: status=%d, want 400", response.Code)
	}
}

func TestAdminAttemptReservationIsConcurrentAndExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		access := &adminAccess{accounts: map[string]config.AdminAccount{"admin": {}}, attempts: make(map[string]loginWindow)}
		var allowed atomic.Int32
		var group sync.WaitGroup
		for range 20 {
			group.Go(func() {
				if access.reserveAttempt("admin") {
					allowed.Add(1)
				}
			})
		}
		group.Wait()
		if allowed.Load() != 5 || access.reserveAttempt("admin") {
			t.Fatalf("concurrent reserved attempts=%d, want 5", allowed.Load())
		}
		time.Sleep(time.Minute)
		if !access.reserveAttempt("admin") {
			t.Fatal("expired login window did not reopen")
		}
	})
}

func TestAdminUnknownNamesShareAttemptLimit(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	for i := range 6 {
		form := url.Values{"username": {fmt.Sprintf("unknown-%d", i)}, "password": {"wrong"}}
		response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, "")
		want := http.StatusUnauthorized
		if i == 5 {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("unknown name %d: status=%d, want %d", i, response.Code, want)
		}
	}
}

func TestAdminUnattachedSessionExpires(t *testing.T) {
	account := testAdminAccount()
	synctest.Test(t, func(t *testing.T) {
		jw, err := jaws.New()
		if err != nil {
			t.Fatal(err)
		}
		handler, err := newAdminHandler(jw, config.AdminConfig{Administrators: []config.AdminAccount{account}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { jw.Serve(); close(done) }()
		defer func() { jw.Close(); <-done }()
		form := url.Values{"username": {"admin"}, "password": {"correct-password"}}
		expiredGuest := adminGuestCookie(t, handler)
		time.Sleep(time.Minute + time.Second)
		if response := adminResponse(handler, http.MethodPost, "/admin/login", form, expiredGuest, ""); response.Code != http.StatusConflict || len(response.Result().Cookies()) != 0 {
			t.Fatalf("expired guest login: status=%d cookies=%v", response.Code, response.Result().Cookies())
		}
		response := adminResponse(handler, http.MethodPost, "/admin/login", form, adminGuestCookie(t, handler), "")
		if response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 {
			t.Fatalf("login before expiry: status=%d cookieCount=%d", response.Code, len(response.Result().Cookies()))
		}
		cookie := response.Result().Cookies()[0]
		time.Sleep(time.Minute + time.Second)
		if response := adminResponse(handler, http.MethodGet, "/", nil, cookie, ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `href="/admin/login"`) {
			t.Fatalf("expired admin session home: status=%d body=%q", response.Code, response.Body.String())
		}
		if response := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
			t.Fatalf("expired sign-out: status=%d location=%q", response.Code, response.Header().Get("Location"))
		}
	})
}
