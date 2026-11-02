package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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
	handler := newAdminHandler(jw, config.AdminConfig{Administrators: administrators})
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

func TestAdminLoginAndRevocation(t *testing.T) {
	account := testAdminAccount()
	handler, jw, logs := testAdminHandler(t, []config.AdminAccount{account})
	if response := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, ""); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `name="password"`) || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("login page: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	form := url.Values{"username": {"operator"}, "password": {"wrong-password"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusUnauthorized ||
		len(response.Result().Cookies()) != 0 || strings.Contains(response.Body.String(), "wrong-password") {
		t.Fatalf("invalid login: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	form.Set("password", "correct-password")
	response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, "")
	if response.Code != http.StatusSeeOther || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("valid login: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != jw.CookieName || !cookies[0].Secure || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Path != "/" {
		t.Fatalf("admin session cookie = %+v", cookies)
	}
	cookie := cookies[0]
	if response := adminResponse(handler, http.MethodGet, "/admin/login", nil, cookie, ""); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Signed in") {
		t.Fatalf("authenticated page: status=%d body=%q", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "https://admin.example/jaws/.ping", nil)
	request.TLS = &tls.ConnectionState{}
	request.AddCookie(cookie)
	jaWSRequest := jw.NewRequest(httptest.NewRecorder(), request)
	if !jw.MakeAuth(jaWSRequest).IsAdmin() {
		t.Fatal("JaWS template authorization did not use the admin session")
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://other.example"); response.Code != http.StatusForbidden || response.Header().Get("Strict-Transport-Security") != "" {
		t.Fatalf("cross-origin logout: status=%d headers=%v", response.Code, response.Header())
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "http://admin.example"); response.Code != http.StatusForbidden {
		t.Fatalf("HTTP origin on TLS logout: status=%d, want 403", response.Code)
	}
	if response := adminResponse(handler, http.MethodGet, "/admin/login", nil, cookie, ""); !strings.Contains(response.Body.String(), "Signed in") {
		t.Fatal("cross-origin logout revoked the active session")
	}
	response = adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example")
	if response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 || response.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatalf("logout: status=%d headers=%v", response.Code, response.Header())
	}
	if jw.MakeAuth(jaWSRequest).IsAdmin() {
		t.Fatal("revoked JaWS request retained admin template authorization")
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/jaws/.ping"},
		{http.MethodPost, "/jaws/invalid-key"},
	} {
		if response := adminResponse(handler, tc.method, tc.path, nil, cookie, ""); response.Code != http.StatusForbidden {
			t.Errorf("revoked %s %s: status=%d, want 403", tc.method, tc.path, response.Code)
		}
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin/login" {
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
		t.Fatalf("credentials appeared in logs: %q", logs.String())
	}
}

func TestAdminServerLoginOverConfiguredTLS(t *testing.T) {
	certFile, keyFile, leaf := remoteTestTLSFiles(t)
	admin, err := startAdminServer(t.Context(), config.AdminConfig{
		Listen: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile,
		Administrators: []config.AdminAccount{testAdminAccount()},
	}, nil)
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
		t.Fatalf("TLS login: status=%d cookies=%v", response.StatusCode, response.Cookies())
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
		t.Fatalf("authenticated TLS page: status=%d body=%q", page.StatusCode, pageBody)
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
	}, nil)
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
		t.Fatalf("loopback login: status=%d cookies=%v", response.StatusCode, response.Cookies())
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
		t.Fatalf("authenticated loopback page: status=%d body=%q", page.StatusCode, pageBody)
	}
}

func TestAdminUnreadBodiesCloseWithoutWaiting(t *testing.T) {
	admin, err := startAdminServer(t.Context(), config.AdminConfig{
		Listen: "127.0.0.1:0", Administrators: []config.AdminAccount{testAdminAccount()},
	}, nil)
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
		t.Fatalf("login: status=%d cookies=%v", response.StatusCode, response.Cookies())
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
			headers: "Cookie: " + cookie.Name + "=" + cookie.Value + "\r\n", want: http.StatusMethodNotAllowed},
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
		{http.MethodPost, "/admin/logout"}, {http.MethodGet, "/jaws/.ping"},
		{http.MethodGet, "/jaws/test-websocket"}, {http.MethodPost, "/jaws/invalid-key"},
	} {
		if response := adminResponse(handler, tc.method, tc.path, nil, nil, ""); response.Code != http.StatusForbidden ||
			len(response.Result().Cookies()) != 0 {
			t.Errorf("unconfigured %s %s: status=%d cookies=%v", tc.method, tc.path, response.Code, response.Result().Cookies())
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
		t.Fatalf("cross-origin login: status=%d headers=%v", response.Code, response.Header())
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
			t.Fatalf("duplicate credential field: status=%d cookies=%v", response.Code, response.Result().Cookies())
		}
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"username": {strings.Repeat("x", 101)}, "password": {"wrong"}}, nil, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("username beyond config limit: status=%d, want 400", response.Code)
	}
	for _, tc := range []struct{ name, password string }{
		{name: "reserved", password: strings.Repeat("&", 4096)},
		{name: "non-ASCII", password: strings.Repeat("é", 2048)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"username": {"operator"}, "password": {tc.password}}
			if got := len(form.Encode()); len(tc.password) != 4096 || got <= 8<<10 || got >= 16<<10 {
				t.Fatalf("escaped valid password: bytes=%d form bytes=%d, want 4096 and 8193..16383", len(tc.password), got)
			}
			escapedHandler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccountWithPassword(tc.password)})
			if response := adminResponse(escapedHandler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 {
				t.Fatalf("escaped valid password: status=%d cookies=%v, want login", response.Code, response.Result().Cookies())
			}
		})
	}
	form := url.Values{"username": {"unknown"}, "password": {"wrong"}, "padding": {""}}
	form.Set("padding", strings.Repeat("x", (16<<10)-len(form.Encode())))
	if got := len(form.Encode()); got != 16<<10 {
		t.Fatalf("fixture body length=%d, want 16384", got)
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("16384-byte login: status=%d, want 401", response.Code)
	}
	form.Set("padding", form.Get("padding")+"x")
	if response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("16385-byte login: status=%d, want 400", response.Code)
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
		handler := newAdminHandler(jw, config.AdminConfig{Administrators: []config.AdminAccount{account}})
		done := make(chan struct{})
		go func() { jw.Serve(); close(done) }()
		defer func() { jw.Close(); <-done }()
		form := url.Values{"username": {"operator"}, "password": {"correct-password"}}
		response := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, "")
		if response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 {
			t.Fatalf("login before expiry: status=%d cookies=%v", response.Code, response.Result().Cookies())
		}
		cookie := response.Result().Cookies()[0]
		time.Sleep(time.Minute + time.Second)
		if response := adminResponse(handler, http.MethodGet, "/jaws/.ping", nil, cookie, ""); response.Code != http.StatusForbidden {
			t.Fatalf("expired admin session: status=%d, want 403", response.Code)
		}
		if response := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin/login" {
			t.Fatalf("expired sign-out: status=%d location=%q, want login redirect", response.Code, response.Header().Get("Location"))
		}
	})
}
