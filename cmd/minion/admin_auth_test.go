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
	return config.AdminAccount{Username: "operator", PasswordHash: fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s",
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
	handler, err := newAdminHandler(jw, config.AdminConfig{Administrators: administrators}, nil)
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

func TestAdminLoginJaWSKeyIsolationAndRotation(t *testing.T) {
	handler, jw, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	if jw.MaxSessions != maxAdminSessions || jw.MaxSessionsPerIP != maxAdminSessionsPerIP {
		t.Fatalf("unbounded JaWS sessions: global=%d per_ip=%d", jw.MaxSessions, jw.MaxSessionsPerIP)
	}
	first := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if first.Code != http.StatusOK || len(first.Result().Cookies()) != 1 ||
		!strings.Contains(first.Body.String(), `name="password"`) ||
		!strings.Contains(first.Body.String(), `/jaws/.tail/`) ||
		first.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("guest JaWS login page: status=%d cookies=%v body=%q", first.Code, first.Result().Cookies(), first.Body.String())
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
	if denied := adminResponse(handler, http.MethodGet, "/", nil, firstCookie, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("guest reached dashboard: status=%d", denied.Code)
	}
	assetPath := func(ext string) string {
		t.Helper()
		for _, part := range strings.Split(first.Body.String(), `"`) {
			if strings.HasPrefix(part, "/jaws/.jaws.") && strings.HasSuffix(part, ext) {
				return part
			}
		}
		t.Fatalf("login page omitted JaWS %s asset", ext)
		return ""
	}
	for _, tc := range []struct {
		path string
		want int
	}{{"/admin/dashboard.css", http.StatusOK}, {assetPath(".js"), http.StatusOK},
		{assetPath(".css"), http.StatusOK}, {"/jaws/.ping", http.StatusNoContent}} {
		if asset := adminResponse(handler, http.MethodGet, tc.path, nil, nil, ""); asset.Code != tc.want {
			t.Fatalf("public asset %s: status=%d, want %d", tc.path, asset.Code, tc.want)
		}
	}
	duplicate := httptest.NewRequest(http.MethodGet, "https://admin.example/admin/login", nil)
	duplicate.AddCookie(firstCookie)
	duplicate.AddCookie(firstCookie)
	duplicateResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateResponse, duplicate)
	if duplicateResponse.Code != http.StatusForbidden {
		t.Fatalf("duplicate session cookies: status=%d", duplicateResponse.Code)
	}
	login := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"username": {"operator"}, "password": {"correct-password"}}, firstCookie, "https://admin.example")
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login after guest page: status=%d cookies=%v", login.Code, login.Result().Cookies())
	}
	adminCookie := login.Result().Cookies()[0]
	for _, cookie := range []*http.Cookie{firstCookie, adminCookie} {
		if denied := adminResponse(handler, http.MethodGet, "/jaws/"+firstKey, nil, cookie, ""); denied.Code != http.StatusForbidden {
			t.Fatalf("rotated guest key remained usable: status=%d", denied.Code)
		}
	}
	signed := adminResponse(handler, http.MethodGet, "/admin/login", nil, adminCookie, "")
	if signed.Code != http.StatusOK || !strings.Contains(signed.Body.String(), "Signed in") {
		t.Fatalf("signed-in JaWS login page: status=%d body=%q", signed.Code, signed.Body.String())
	}
	signedKey := adminJaWSKey(t, signed.Body.String())
	for _, path := range []string{"/jaws/" + signedKey, "/jaws/.tail/" + signedKey} {
		if denied := adminResponse(handler, http.MethodGet, path, nil, secondCookie, ""); denied.Code != http.StatusForbidden {
			t.Fatalf("guest reached admin page key %s: status=%d", path, denied.Code)
		}
	}
	if logout := adminResponse(handler, http.MethodPost, "/admin/logout", nil, adminCookie, "https://admin.example"); logout.Code != http.StatusSeeOther {
		t.Fatalf("logout: status=%d", logout.Code)
	}
	if denied := adminResponse(handler, http.MethodGet, "/jaws/"+signedKey, nil, adminCookie, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("revoked admin key remained usable: status=%d", denied.Code)
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
	connect := func(cookie *http.Cookie) (*websocket.Conn, *http.Response, error) {
		header := http.Header{}
		header.Set("Cookie", cookie.Name+"="+cookie.Value)
		header.Set("Origin", server.URL)
		return websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/jaws/"+firstKey,
			&websocket.DialOptions{HTTPHeader: header})
	}
	if conn, response, err := connect(secondCookie); err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		if conn != nil {
			conn.Close(websocket.StatusNormalClosure, "")
		}
		t.Fatalf("same-IP guest claimed another key: response=%v error=%v", response, err)
	} else {
		response.Body.Close()
	}
	conn, response, err := connect(firstCookie)
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
}

func TestAdminGuestSessionLimit(t *testing.T) {
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	jw.MaxSessions, jw.MaxSessionsPerIP = 2, 1
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	defer func() { jw.Close(); <-done }()
	first := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if first.Code != http.StatusOK || len(first.Result().Cookies()) != 1 {
		t.Fatalf("first guest session: status=%d cookies=%v", first.Code, first.Result().Cookies())
	}
	if denied := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, ""); denied.Code != http.StatusTooManyRequests || len(denied.Result().Cookies()) != 0 {
		t.Fatalf("per-IP session cap: status=%d cookies=%v", denied.Code, denied.Result().Cookies())
	}
	if existing := adminResponse(handler, http.MethodGet, "/admin/login", nil, first.Result().Cookies()[0], ""); existing.Code != http.StatusOK {
		t.Fatalf("session cap blocked existing guest: status=%d", existing.Code)
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
	handler, _, _ := testAdminHandler(t, cfg.Admin.Administrators)
	for _, test := range []struct {
		method, path string
		form         url.Values
	}{
		{method: http.MethodGet, path: "/admin/login"},
		{method: http.MethodGet, path: "/"},
		{method: http.MethodPost, path: "/admin/login", form: url.Values{"username": {"admin"}, "password": {"PASTE_GENERATED_HASH_HERE"}}},
	} {
		response := adminResponse(handler, test.method, test.path, test.form, nil, "")
		if response.Code != http.StatusForbidden || len(response.Result().Cookies()) != 0 {
			t.Fatalf("sample admin %s %s: status=%d cookies=%d, want 403 without session", test.method, test.path, response.Code, len(response.Result().Cookies()))
		}
	}
	mixed, _, _ := testAdminHandler(t, []config.AdminAccount{placeholder, testAdminAccount()})
	if response := adminResponse(mixed, http.MethodPost, "/admin/login",
		url.Values{"username": {"admin"}, "password": {"PASTE_GENERATED_HASH_HERE"}}, nil, ""); response.Code != http.StatusUnauthorized || len(response.Result().Cookies()) != 0 {
		t.Fatalf("placeholder among real accounts: status=%d cookies=%d", response.Code, len(response.Result().Cookies()))
	}
	if response := adminResponse(mixed, http.MethodPost, "/admin/login",
		url.Values{"username": {"operator"}, "password": {"correct-password"}}, nil, ""); response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 {
		t.Fatalf("real account alongside placeholder: status=%d cookies=%d", response.Code, len(response.Result().Cookies()))
	}
}

func TestAdminLoginAndRevocation(t *testing.T) {
	account := testAdminAccount()
	handler, jw, logs := testAdminHandler(t, []config.AdminAccount{account})
	if response := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, ""); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `name="password"`) || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("login page: status=%d noStore=%t hasPasswordField=%t", response.Code,
			response.Header().Get("Cache-Control") == "no-store", strings.Contains(response.Body.String(), `name="password"`))
	}
	if response := adminResponse(handler, http.MethodHead, "/admin/login", nil, nil, ""); response.Code != http.StatusOK {
		t.Fatalf("public HEAD login: status=%d, want 200", response.Code)
	}
	form := url.Values{"username": {"operator"}, "password": {"wrong-password"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusUnauthorized ||
		len(response.Result().Cookies()) != 0 || strings.Contains(response.Body.String(), "wrong-password") {
		t.Fatalf("invalid login: status=%d cookieCount=%d echoedPassword=%t", response.Code,
			len(response.Result().Cookies()), strings.Contains(response.Body.String(), "wrong-password"))
	}
	form.Set("password", "correct-password")
	response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, "")
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
	if response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 || response.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatalf("logout: status=%d cookieCount=%d", response.Code, len(response.Result().Cookies()))
	}
	if jw.MakeAuth(jaWSRequest).IsAdmin() {
		t.Fatal("revoked JaWS request retained admin template authorization")
	}
	if response := adminResponse(handler, http.MethodPost, "/jaws/invalid-key", nil, cookie, ""); response.Code != http.StatusForbidden {
		t.Errorf("revoked POST /jaws/invalid-key: status=%d, want 403", response.Code)
	}
	if response := adminResponse(handler, http.MethodGet, "/jaws/.ping", nil, cookie, ""); response.Code != http.StatusNoContent {
		t.Errorf("public JaWS ping after logout: status=%d, want 204", response.Code)
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin/login" {
		t.Fatalf("revoked logout: status=%d toLogin=%t", response.Code, response.Header().Get("Location") == "/admin/login")
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
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client := adminTLSClient(t, leaf)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	baseURL := "https://" + admin.listener.Addr().String()
	response, err := client.PostForm(baseURL+"/admin/login", url.Values{"username": {"operator"}, "password": {"correct-password"}})
	if err != nil {
		t.Fatal(err)
	}
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
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client := &http.Client{Transport: &http.Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	baseURL := "http://" + admin.listener.Addr().String()
	response, err := client.PostForm(baseURL+"/admin/login", url.Values{"username": {"operator"}, "password": {"correct-password"}})
	if err != nil {
		t.Fatal(err)
	}
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
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.PostForm("http://"+admin.listener.Addr().String()+"/admin/login",
		url.Values{"username": {"operator"}, "password": {"correct-password"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || len(response.Cookies()) != 1 {
		t.Fatalf("login: status=%d cookieCount=%d", response.StatusCode, len(response.Cookies()))
	}
	cookie := response.Cookies()[0]
	for _, tc := range []struct {
		name, method, path, headers string
		want                        int
	}{
		{name: "public GET", method: http.MethodGet, path: "/admin/login", want: http.StatusOK},
		{name: "cross-origin login", method: http.MethodPost, path: "/admin/login",
			headers: "Origin: http://other.example\r\n", want: http.StatusForbidden},
		{name: "unsupported login body", method: http.MethodPost, path: "/admin/login",
			headers: "Content-Type: text/plain\r\n", want: http.StatusBadRequest},
		{name: "authenticated JaWS rejection", method: http.MethodPost, path: "/jaws/.ping",
			headers: "Cookie: " + cookie.Name + "=" + cookie.Value + "\r\n", want: http.StatusForbidden},
		{name: "authenticated dashboard", method: http.MethodGet, path: "/",
			headers: "Cookie: " + cookie.Name + "=" + cookie.Value + "\r\n", want: http.StatusOK},
		{name: "authenticated stylesheet", method: http.MethodGet, path: "/admin/dashboard.css",
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
			request := tc.method + " " + tc.path + " HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n" + tc.headers + "\r\n"
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
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("server kept incomplete request open: %v", err)
			}
		})
	}
}

func TestAdminWithoutConfiguredAccountsDeniesEveryEntry(t *testing.T) {
	handler, jw, _ := testAdminHandler(t, nil)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/login"}, {http.MethodPost, "/admin/login"},
		{http.MethodHead, "/admin/login"},
		{http.MethodPost, "/admin/logout"}, {http.MethodGet, "/jaws/.ping"},
		{http.MethodGet, "/jaws/test-websocket"}, {http.MethodPost, "/jaws/invalid-key"},
	} {
		if response := adminResponse(handler, tc.method, tc.path, nil, nil, ""); response.Code != http.StatusForbidden ||
			len(response.Result().Cookies()) != 0 {
			t.Errorf("unconfigured %s %s: status=%d cookieCount=%d", tc.method, tc.path, response.Code, len(response.Result().Cookies()))
		}
	}
	request := httptest.NewRequest(http.MethodGet, "https://admin.example/jaws/.ping", nil)
	if jw.MakeAuth(jw.NewRequest(httptest.NewRecorder(), request)).IsAdmin() {
		t.Fatal("unconfigured JaWS template authorization is open")
	}
}

func TestAdminLoginBoundsAndCrossOrigin(t *testing.T) {
	account := testAdminAccount()
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{account})
	form := url.Values{"username": {"operator"}, "password": {"wrong-password"}}
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
	form = url.Values{"username": {"operator"}, "password": {strings.Repeat("x", 8192)}}
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("oversized password: status=%d, want 400", response.Code)
	}
}

func TestAdminHashCapacityReturnsBusy(t *testing.T) {
	access := &adminAccess{hashSlots: make(chan struct{}, 2)}
	access.hashSlots <- struct{}{}
	access.hashSlots <- struct{}{}
	request := httptest.NewRequest(http.MethodPost, "/admin/login",
		strings.NewReader(url.Values{"username": {"operator"}, "password": {"password"}}.Encode()))
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
		{"username": {"operator", "operator"}, "password": {"correct-password"}},
		{"username": {"operator"}, "password": {"correct-password", "correct-password"}},
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
			form := url.Values{"username": {"operator"}, "password": {tc.password}}
			if got := len(form.Encode()); got <= len(tc.password) || got >= 2<<10 {
				t.Fatalf("escaped valid password: password bytes=%d form bytes=%d", len(tc.password), got)
			}
			escapedHandler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccountWithPassword(tc.password)})
			if response := adminResponse(escapedHandler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 {
				t.Fatalf("escaped valid password: status=%d cookieCount=%d, want login", response.Code, len(response.Result().Cookies()))
			}
		})
	}
	for _, password := range []string{strings.Repeat("a", 65), strings.Repeat("😀", 65), string([]byte{0xff})} {
		if response := adminResponse(handler, http.MethodPost, "/admin/login",
			url.Values{"username": {"operator"}, "password": {password}}, nil, ""); response.Code != http.StatusBadRequest {
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
		access := &adminAccess{accounts: map[string]config.AdminAccount{"operator": {}}, attempts: make(map[string]loginWindow)}
		var allowed atomic.Int32
		var group sync.WaitGroup
		for range 20 {
			group.Go(func() {
				if access.reserveAttempt("operator") {
					allowed.Add(1)
				}
			})
		}
		group.Wait()
		if allowed.Load() != 5 || access.reserveAttempt("operator") {
			t.Fatalf("concurrent reserved attempts=%d, want 5", allowed.Load())
		}
		time.Sleep(time.Minute)
		if !access.reserveAttempt("operator") {
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
		handler, err := newAdminHandler(jw, config.AdminConfig{Administrators: []config.AdminAccount{account}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { jw.Serve(); close(done) }()
		defer func() { jw.Close(); <-done }()
		form := url.Values{"username": {"operator"}, "password": {"correct-password"}}
		response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, "")
		if response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 {
			t.Fatalf("login before expiry: status=%d cookieCount=%d", response.Code, len(response.Result().Cookies()))
		}
		cookie := response.Result().Cookies()[0]
		time.Sleep(time.Minute + time.Second)
		if response := adminResponse(handler, http.MethodGet, "/", nil, cookie, ""); response.Code != http.StatusForbidden {
			t.Fatalf("expired admin session: status=%d, want 403", response.Code)
		}
		if response := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin/login" {
			t.Fatalf("expired sign-out: status=%d toLogin=%t, want login redirect", response.Code, response.Header().Get("Location") == "/admin/login")
		}
	})
}
