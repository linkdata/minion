package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
)

func TestAdminConfigConflictNoticeShowsReason(t *testing.T) {
	live, _ := liveConfigFixture(t, false)
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()}}, &action.Orchestrator{}, nil, live)
	if err != nil {
		jw.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })
	cookie := dashboardTestCookie(t, handler)
	response := adminResponse(handler, http.MethodPost, "/admin/repositories/remove", url.Values{"name": {"owner/repo"}}, cookie, "https://admin.example")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "At least one repository is required") {
		t.Fatalf("admin conflict notice: status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestAdminConflictReasonIsBoundedAndPrivate(t *testing.T) {
	account := testAdminAccount()
	handler, jw, _ := testAdminHandler(t, []config.AdminAccount{account})
	admin := dashboardTestCookie(t, handler)
	guest := adminGuestCookie(t, handler)
	failure := (&adminAccess{jaws: jw, account: account, passwordSet: true}).browserErrors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, strings.Repeat("x", 1024), http.StatusConflict)
	}))
	for _, test := range []struct {
		name   string
		cookie *http.Cookie
		show   bool
	}{
		{"admin", admin, true},
		{"guest", guest, false},
	} {
		response := adminResponse(failure, http.MethodPost, "/admin/stop", nil, test.cookie, "")
		body := response.Body.String()
		if response.Code != http.StatusConflict || strings.Contains(body, strings.Repeat("x", 513)) || strings.Contains(body, strings.Repeat("x", 512)) != test.show {
			t.Fatalf("%s conflict reason: status=%d body=%q", test.name, response.Code, body)
		}
	}
}

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
		{"bad login", http.MethodPost, "/admin/login", url.Values{"password": {"wrong-password"}}, guest, "", http.StatusUnauthorized},
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
		url.Values{"password": {"correct-password"}}, guest, "")
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
				!strings.Contains(response.Body.String(), "/jaws/.tail/") ||
				strings.Contains(response.Body.String(), `href="/admin/login"`) {
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
}

func TestFailedAdminLoginReservesDirectRetry(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	home := adminResponse(handler, http.MethodGet, "/", nil, nil, "")
	if home.Code != http.StatusOK || len(home.Result().Cookies()) != 1 {
		t.Fatalf("guest home: status=%d cookies=%v", home.Code, home.Result().Cookies())
	}
	guest := home.Result().Cookies()[0]
	if login := adminResponse(handler, http.MethodGet, "/admin/login", nil, guest, ""); login.Code != http.StatusOK {
		t.Fatalf("guest login page: status=%d", login.Code)
	}
	failed := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"password": {"wrong-password"}}, guest, "")
	if failed.Code != http.StatusUnauthorized || !strings.Contains(failed.Body.String(), `href="/admin/login"`) {
		t.Fatalf("failed login has no direct retry: status=%d body=%q", failed.Code, failed.Body.String())
	}
	if retry := adminResponse(handler, http.MethodGet, "/admin/login", nil, guest, ""); retry.Code != http.StatusOK ||
		!strings.Contains(retry.Body.String(), `name="password"`) {
		t.Fatalf("reserved retry page unavailable: status=%d body=%q", retry.Code, retry.Body.String())
	}
}

func TestOAuthOnlyAdminLoginShowsPasswordSetup(t *testing.T) {
	cfg := config.AdminConfig{Administrators: []config.AdminAccount{{Username: "admin"}}, GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
		RedirectURL: "https://admin.example/auth/github/callback"}}
	handler, _, _ := testOAuthAdminHandler(t, cfg, nil)
	page := adminResponse(handler, http.MethodGet, "/auth/login", nil, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 {
		t.Fatalf("user login: status=%d cookies=%v", page.Code, page.Result().Cookies())
	}
	setup := adminResponse(handler, http.MethodGet, "/admin/login", nil, page.Result().Cookies()[0], "")
	if setup.Code != http.StatusOK || !strings.Contains(setup.Body.String(), "The admin password is not set") ||
		strings.Contains(setup.Body.String(), `name="password"`) {
		t.Fatalf("OAuth-only admin setup page: status=%d body=%q", setup.Code, setup.Body.String())
	}
	if denied := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"password": {"guess"}}, page.Result().Cookies()[0], ""); denied.Code != http.StatusForbidden {
		t.Fatalf("OAuth-only admin sign-in: status=%d, want 403", denied.Code)
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
		url.Values{"password": {"correct-password"}}, nil, "")
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
		url.Values{"password": {"correct-password"}}, guest, "")
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
		if notice.Code != http.StatusForbidden || notice.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
			!strings.Contains(notice.Body.String(), `href="/admin/login"`) {
			t.Fatalf("available guest notice: status=%d type=%q body=%q", notice.Code, notice.Header().Get("Content-Type"), notice.Body.String())
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

func TestConcurrentAdminGuestErrorNoticesReserveSignInSlot(t *testing.T) {
	account := testAdminAccount()
	handler, jw, _ := testAdminHandler(t, []config.AdminAccount{account})
	guest := adminGuestCookie(t, handler)
	const concurrent = 16
	var ready, done sync.WaitGroup
	ready.Add(concurrent)
	done.Add(concurrent)
	release := make(chan struct{})
	failure := (&adminAccess{jaws: jw, account: account, passwordSet: true}).browserErrors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ready.Done()
		<-release
		w.WriteHeader(http.StatusForbidden)
	}))
	responses := make([]*httptest.ResponseRecorder, concurrent)
	for i := range responses {
		go func() {
			defer done.Done()
			responses[i] = adminResponse(failure, http.MethodGet, "/admin/usage", nil, guest, "")
		}()
	}
	ready.Wait()
	close(release)
	done.Wait()
	notices := 0
	for _, response := range responses {
		if response.Code != http.StatusForbidden {
			t.Fatalf("concurrent notice: status=%d body=%q", response.Code, response.Body.String())
		}
		if response.Header().Get("Content-Type") == "text/html; charset=utf-8" {
			notices++
			continue
		}
		if response.Body.Len() != 0 || response.Header().Get("Content-Type") != "" {
			t.Fatalf("limited notice: type=%q body=%q", response.Header().Get("Content-Type"), response.Body.String())
		}
	}
	if notices != maxGuestPageRequests-2 {
		t.Fatalf("concurrent notices=%d, want %d", notices, maxGuestPageRequests-2)
	}
	if page := adminResponse(handler, http.MethodGet, "/admin/login", nil, guest, ""); page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), `name="password"`) {
		t.Fatalf("reserved sign-in page unavailable: status=%d body=%q", page.Code, page.Body.String())
	}
}

func TestSignedInOAuthAdminErrorIsNotGuestLimited(t *testing.T) {
	cfg := config.AdminConfig{
		Administrators: []config.AdminAccount{testAdminAccount()},
		GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret",
			RedirectURL: "https://admin.example/auth/github/callback"},
	}
	handler, jw, _ := testOAuthAdminHandler(t, cfg, nil)
	key := (userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key()
	cookie := userUsageCookie(t, jw, key, false)
	for range maxGuestPageRequests {
		if page := userUsageRequest(handler, http.MethodGet, "/auth/login", "", cookie); page.Code != http.StatusOK {
			t.Fatalf("signed-in OAuth page: status=%d body=%q", page.Code, page.Body.String())
		}
	}
	denied := userUsageRequest(handler, http.MethodGet, "/admin/usage", "", cookie)
	if denied.Code != http.StatusForbidden || denied.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		!strings.Contains(denied.Body.String(), "Request could not be completed") {
		t.Fatalf("signed-in OAuth notice: status=%d type=%q body=%q", denied.Code, denied.Header().Get("Content-Type"), denied.Body.String())
	}
}

func TestAdminLoginRateLimitUsesJaWSWhenGuestHasRoom(t *testing.T) {
	handler, _, _ := testAdminHandler(t, []config.AdminAccount{testAdminAccount()})
	form := url.Values{"password": {"wrong-password"}}
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
