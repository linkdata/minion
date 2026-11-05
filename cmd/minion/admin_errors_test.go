package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestAdminBrowserErrorsUseJaWS(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	guest := adminGuestCookie(t, handler)

	for _, test := range []struct {
		name, method, path string
		form               url.Values
		cookie             *http.Cookie
		origin             string
		status             int
	}{
		{"bad login", http.MethodPost, "/admin/login", url.Values{"username": {"operator"}, "password": {"wrong-password"}}, guest, "", http.StatusUnauthorized},
		{"guest usage", http.MethodGet, "/admin/usage", nil, guest, "", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := adminResponse(handler, test.method, test.path, test.form, test.cookie, test.origin)
			if response.Code != test.status || response.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
				!strings.Contains(response.Body.String(), "Request could not be completed") ||
				!strings.Contains(response.Body.String(), `<meta name="viewport" content="width=device-width, initial-scale=1">`) ||
				!strings.Contains(response.Body.String(), `<title>Minion admin request error</title>`) ||
				!strings.Contains(response.Body.String(), "/jaws/.tail/") ||
				strings.Contains(response.Body.String(), "wrong-password") {
				t.Fatalf("browser failure: status=%d type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
			}
			if test.name == "bad login" {
				key := adminJaWSKey(t, response.Body.String())
				if tail := adminResponse(handler, http.MethodGet, "/jaws/.tail/"+key, nil, guest, ""); tail.Code != http.StatusOK {
					t.Fatalf("JaWS error page connection: status=%d", tail.Code)
				}
			}
		})
	}

	login := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"username": {"operator"}, "password": {"correct-password"}}, guest, "")
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("admin login: status=%d cookies=%v", login.Code, login.Result().Cookies())
	}
	admin := login.Result().Cookies()[0]
	for _, test := range []struct {
		name, method, path string
		form               url.Values
		origin             string
		status             int
	}{
		{"invalid dashboard filter", http.MethodGet, "/?action=unknown", nil, "", http.StatusBadRequest},
		{"invalid detail ID", http.MethodGet, "/admin/actions/@", nil, "", http.StatusBadRequest},
		{"invalid stop form", http.MethodPost, "/admin/stop", url.Values{"job_id": {"bad"}}, "https://admin.example", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := adminResponse(handler, test.method, test.path, test.form, admin, test.origin)
			if response.Code != test.status || response.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
				!strings.Contains(response.Body.String(), "Request could not be completed") ||
				!strings.Contains(response.Body.String(), "/jaws/.tail/") {
				t.Fatalf("browser failure: status=%d type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
			}
		})
	}

	if dashboard := adminResponse(handler, http.MethodGet, "/", nil, admin, ""); dashboard.Code != http.StatusOK ||
		!strings.Contains(dashboard.Body.String(), "Live actions") {
		t.Fatalf("successful dashboard: status=%d body=%q", dashboard.Code, dashboard.Body.String())
	}
	if logout := adminResponse(handler, http.MethodPost, "/admin/logout", nil, admin, "https://admin.example"); logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != "/" {
		t.Fatalf("sign-out redirect: status=%d location=%q", logout.Code, logout.Header().Get("Location"))
	}
	if reload := adminResponse(handler, http.MethodGet, "/admin/usage", nil, admin, ""); reload.Code != http.StatusSeeOther || reload.Header().Get("Location") != "/" || len(reload.Result().Cookies()) != 0 {
		t.Fatalf("signed-out page reload: status=%d location=%q cookies=%v", reload.Code, reload.Header().Get("Location"), reload.Result().Cookies())
	}
}

func TestOAuthOnlyHomeOmitsAdminLogin(t *testing.T) {
	cfg := config.AdminConfig{GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/github/callback"}}
	handler, _, _ := testOAuthAdminHandler(t, cfg, nil)
	page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 {
		t.Fatalf("user login: status=%d cookies=%v", page.Code, page.Result().Cookies())
	}
	home := adminResponse(handler, http.MethodGet, "/", nil, page.Result().Cookies()[0], "")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), `href="/auth/login"`) ||
		strings.Contains(home.Body.String(), `href="/admin/login"`) {
		t.Fatalf("OAuth-only home: status=%d body=%q", home.Code, home.Body.String())
	}
}

func TestAdminErrorWithoutSessionIsStatusOnly(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	response := adminResponse(handler, http.MethodGet, "/admin/usage", nil, nil, "")
	if response.Code != http.StatusForbidden || response.Body.Len() != 0 || response.Header().Get("Content-Type") != "" ||
		len(response.Result().Cookies()) != 0 {
		t.Fatalf("sessionless error: status=%d type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	post := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"username": {"operator"}, "password": {"correct-password"}}, nil, "")
	if post.Code != http.StatusConflict || post.Body.Len() != 0 || post.Header().Get("Content-Type") != "" ||
		len(post.Result().Cookies()) != 0 {
		t.Fatalf("sessionless login: status=%d type=%q body=%q", post.Code, post.Header().Get("Content-Type"), post.Body.String())
	}
	json := adminResponse(handler, http.MethodGet, "/admin/usage.json", nil, nil, "")
	if json.Code != http.StatusForbidden || json.Body.Len() != 0 || json.Header().Get("Content-Type") != "" {
		t.Fatalf("JSON endpoint unexpectedly rendered UI: status=%d type=%q body=%q", json.Code, json.Header().Get("Content-Type"), json.Body.String())
	}
	guest := adminGuestCookie(t, handler)
	for range 3 {
		if page := adminResponse(handler, http.MethodGet, "/admin/login", nil, guest, ""); page.Code != http.StatusOK {
			t.Fatalf("guest page: status=%d", page.Code)
		}
	}
	limited := adminResponse(handler, http.MethodGet, "/admin/login", nil, guest, "")
	if limited.Code != http.StatusTooManyRequests || limited.Body.Len() != 0 || limited.Header().Get("Content-Type") != "" {
		t.Fatalf("JaWS admission limit: status=%d type=%q body=%q", limited.Code, limited.Header().Get("Content-Type"), limited.Body.String())
	}
}

func TestUnknownAdminBrowserRoutes(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	guest := adminGuestCookie(t, handler)
	login := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"username": {"operator"}, "password": {"correct-password"}}, guest, "")
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("admin login: status=%d cookies=%v", login.Code, login.Result().Cookies())
	}
	admin := login.Result().Cookies()[0]
	for _, path := range []string{"/admin/unknown", "/admin/login/"} {
		response := adminResponse(handler, http.MethodGet, path, nil, admin, "")
		if response.Code != http.StatusNotFound || response.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
			!strings.Contains(response.Body.String(), "Request could not be completed") {
			t.Fatalf("unknown admin browser route %s: status=%d type=%q body=%q",
				path, response.Code, response.Header().Get("Content-Type"), response.Body.String())
		}
	}
	unsupported := adminResponse(handler, http.MethodPut, "/admin/login", nil, admin, "")
	if unsupported.Code < http.StatusBadRequest || unsupported.Body.Len() != 0 || unsupported.Header().Get("Content-Type") != "" {
		t.Fatalf("unsupported admin method: status=%d type=%q body=%q",
			unsupported.Code, unsupported.Header().Get("Content-Type"), unsupported.Body.String())
	}
}

func TestAdminGuestErrorNoticesReserveSignInSlot(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	guest := adminGuestCookie(t, handler)
	for range 2 {
		notice := adminResponse(handler, http.MethodGet, "/admin/usage", nil, guest, "")
		if notice.Code != http.StatusForbidden || notice.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("available guest notice: status=%d type=%q", notice.Code, notice.Header().Get("Content-Type"))
		}
	}
	limited := adminResponse(handler, http.MethodGet, "/admin/usage", nil, guest, "")
	if limited.Code != http.StatusForbidden || limited.Body.Len() != 0 || limited.Header().Get("Content-Type") != "" {
		t.Fatalf("guest notice consumed sign-in slot: status=%d type=%q body=%q", limited.Code, limited.Header().Get("Content-Type"), limited.Body.String())
	}
	if page := adminResponse(handler, http.MethodGet, "/admin/login", nil, guest, ""); page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), `name="password"`) {
		t.Fatalf("reserved sign-in page unavailable: status=%d body=%q", page.Code, page.Body.String())
	}
}

func TestConcurrentAdminGuestNoticesReserveSignInSlot(t *testing.T) {
	for round := range 10 {
		handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
		guest := adminGuestCookie(t, handler)
		if second := adminResponse(handler, http.MethodGet, "/admin/login", nil, guest, ""); second.Code != http.StatusOK {
			t.Fatalf("second guest page: status=%d", second.Code)
		}
		const requests = 16
		start := make(chan struct{})
		responses := make(chan *httptest.ResponseRecorder, requests)
		var ready sync.WaitGroup
		ready.Add(requests)
		for range requests {
			go func() {
				ready.Done()
				<-start
				responses <- adminResponse(handler, http.MethodGet, "/admin/usage", nil, guest, "")
			}()
		}
		ready.Wait()
		close(start)
		rendered := 0
		for range requests {
			response := <-responses
			if response.Code != http.StatusForbidden {
				t.Fatalf("round %d guest notice status=%d", round, response.Code)
			}
			if response.Body.Len() != 0 {
				rendered++
			}
		}
		if rendered > maxGuestPageRequests-3 {
			t.Fatalf("round %d guest notices consumed sign-in slot: rendered=%d", round, rendered)
		}
		if page := adminResponse(handler, http.MethodGet, "/admin/login", nil, guest, ""); page.Code != http.StatusOK {
			t.Fatalf("round %d reserved sign-in page unavailable: status=%d", round, page.Code)
		}
	}
}

func TestAdminLoginRateLimitUsesJaWSWhenGuestHasRoom(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	form := url.Values{"username": {"operator"}, "password": {"wrong-password"}}
	for range 5 {
		if failed := adminResponse(handler, http.MethodPost, "/admin/login", form, nil, ""); failed.Code != http.StatusUnauthorized {
			t.Fatalf("failed login: status=%d", failed.Code)
		}
	}
	guest := adminGuestCookie(t, handler)
	limited := adminResponse(handler, http.MethodPost, "/admin/login", form, guest, "")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		!strings.Contains(limited.Body.String(), "Request could not be completed") {
		t.Fatalf("login rate limit notice: status=%d type=%q body=%q", limited.Code, limited.Header().Get("Content-Type"), limited.Body.String())
	}
}

func TestAdminBrowserErrorPreservesHeaders(t *testing.T) {
	handler, jw, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	guest := adminGuestCookie(t, handler)
	failure := (&adminAccess{jaws: jw}).browserErrors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		http.SetCookie(w, &http.Cookie{Name: "notice", Value: "kept"})
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	response := adminResponse(failure, http.MethodGet, "/admin/usage", nil, guest, "")
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "30" ||
		len(response.Result().Cookies()) != 1 || response.Result().Cookies()[0].Value != "kept" ||
		response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("error headers: status=%d headers=%v", response.Code, response.Header())
	}
}
