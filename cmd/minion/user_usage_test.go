package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/usage"
)

func userUsageCookie(t *testing.T, jw *jaws.Jaws, key string, admin bool) *http.Cookie {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://admin.example/auth/login", nil)
	r.TLS = &tls.ConnectionState{}
	r.RemoteAddr = "192.0.2.7:12345"
	w := httptest.NewRecorder()
	sess := jw.NewSession(w, r)
	if sess == nil {
		t.Fatal("create JaWS session")
	}
	if admin {
		sess.Set(adminSessionKey, "operator")
	} else {
		provider, rest, found := strings.Cut(key, ":")
		if !found {
			t.Fatalf("invalid fixture identity %q", key)
		}
		host, subject, found := strings.Cut(rest, ":")
		if !found {
			t.Fatalf("invalid fixture identity %q", key)
		}
		subject, _ = url.QueryUnescape(subject)
		sess.Set("oidc_claims."+provider, map[string]any{"iss": "https://" + host, "sub": subject})
		sess.Set("oauth2authexpiry."+provider, time.Now().Add(time.Hour))
	}
	return sess.Cookie()
}

func userUsageRequest(handler http.Handler, method, path, origin string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
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

func userUsageTestHandler(t *testing.T, owner *action.Orchestrator, clients ...*http.Client) (*jaws.Jaws, *oauthAccess, http.Handler) {
	t.Helper()
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	jw.CookieName = "__Host-minion_session"
	cfg := config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()}, GitHubOAuth: &config.OAuthClient{
		ClientID: "id", ClientSecret: "secret", RedirectURL: "https://admin.example/auth/github/callback"}}
	var client *http.Client
	if len(clients) != 0 {
		client = clients[0]
	}
	oauth, err := newOAuthAccess(jw, cfg, filepath.Join(t.TempDir(), "state.json"), client)
	if err != nil {
		t.Fatal(err)
	}
	oauth.usageGate.apiTokens, oauth.usageGate.apiBurst, oauth.usageGate.accountBurst = 1000, 1000, 1000
	handler, err := newAdminHandler(jw, cfg, owner, oauth)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	go func() { jw.Serve(); close(ready) }()
	t.Cleanup(func() { jw.Close(); <-ready })
	return jw, oauth, handler
}

func TestUserUsageLinkStoreUnavailable(t *testing.T) {
	jw, oauth, handler := userUsageTestHandler(t, nil)
	cookie := userUsageCookie(t, jw, (userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key(), false)
	oauth.links.mu.Lock()
	oauth.links.bad = errors.New("uncertain link write")
	oauth.links.mu.Unlock()
	response := userUsageRequest(handler, http.MethodGet, "/user/usage", "", cookie)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Location") != "" {
		t.Fatalf("bad link store redirected a signed-in user: status=%d location=%q", response.Code, response.Header().Get("Location"))
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") ||
		!strings.Contains(response.Body.String(), `name="jawsKey"`) || strings.Contains(response.Body.String(), "org/private-") {
		t.Fatalf("bad link store did not render a safe JaWS notice: content-type=%q body=%q", got, response.Body.String())
	}
}

func userUsageRecord(id int, repo string, repoID, requesterID int64, user, status string) usage.Record {
	started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(id) * time.Minute)
	finished := started.Add(time.Minute)
	return usage.Record{ID: fmt.Sprintf("%032x", id), JobID: fmt.Sprintf("job-%d", id), Repo: repo,
		RepositoryID: repoID, User: user, RequesterID: requesterID, Action: "review", Number: 1,
		Provider: "codex", Model: "main", StartedAt: started, FinishedAt: &finished, Status: status,
		ExecutorID: "local", SourceRef: strings.Repeat("a", 40)}
}

func userUsageContinuePath(body string) string {
	return userUsageLinkPath(body, "Continue checking repositories")
}

func userUsageRetryPath(body string) string {
	return userUsageLinkPath(body, "Recheck from this position")
}

func userUsageSignedPath(a *oauthAccess, id int64, repo string, after int) string {
	account := a.links.account((userIdentity{Provider: "github", Host: "github.com", Subject: strconv.FormatInt(id, 10)}).key())
	return a.signedUserUsageURL(account, repo, after)
}

func userUsageLinkPath(body, label string) string {
	end := strings.Index(body, `>`+label+`</a>`)
	if end < 0 {
		return ""
	}
	start := strings.LastIndex(body[:end], `href="`)
	if start < 0 {
		return ""
	}
	tail := body[start+len(`href="`):]
	quote := strings.IndexByte(tail, '"')
	if quote < 0 {
		return ""
	}
	return html.UnescapeString(tail[:quote])
}

func TestUserUsageWebservListenerRoutes(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repositories/102" {
			t.Errorf("unexpected GitHub lookup: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"id":102,"full_name":"org/public","private":false,"visibility":"public"}`)
	}))
	defer api.Close()
	githubClient := github.New("fixture-token")
	githubClient.BaseURL = api.URL
	usageStore := &usage.Store{Dir: t.TempDir()}
	record := userUsageRecord(1, "org/public", 102, 42, "alice", "succeeded")
	record.CodexCalls, record.Tokens, record.UsageSource, record.UsageComplete = 1,
		usage.Tokens{Input: 3, Output: 4}, "executor_reported", true
	if err := usageStore.Save(record); err != nil {
		t.Fatal(err)
	}
	admin, err := startAdminServer(t.Context(), config.AdminConfig{
		Listen: "127.0.0.1:0", Administrators: []config.AdminAccount{testAdminAccount()},
		GitHubOAuth: &config.OAuthClient{ClientID: "id", ClientSecret: "secret", RedirectURL: "http://127.0.0.1/auth/github/callback"},
	}, &action.Orchestrator{GitHub: githubClient, Usage: usageStore}, filepath.Join(t.TempDir(), "state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	baseURL := "http://" + admin.listener.Addr().String()
	get := func(path string, cookie *http.Cookie) (int, http.Header, string) {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header, string(body)
	}
	if status, headers, body := get("/user/usage", nil); status != http.StatusSeeOther ||
		headers.Get("Location") != "/auth/login?next=%2Fuser%2Fusage" || strings.Contains(body, "org/public") {
		t.Fatalf("anonymous usage route: status=%d location=%q body=%s", status, headers.Get("Location"), body)
	}
	if status, _, body := get("/user/usage.css", nil); status != http.StatusForbidden ||
		strings.Contains(body, "org/public") || strings.Contains(body, ".topbar") {
		t.Fatalf("anonymous user stylesheet route: status=%d body=%s", status, body)
	}
	seed := httptest.NewRequest(http.MethodGet, baseURL+"/auth/login", nil)
	seed.RemoteAddr = "127.0.0.1:12345"
	session := admin.jaws.NewSession(nil, seed)
	if session == nil {
		t.Fatal("create user session")
	}
	session.Set("oidc_claims.github", map[string]any{"iss": "https://github.com", "sub": "42"})
	session.Set("oauth2authexpiry.github", time.Now().Add(time.Hour))
	cookie := session.Cookie()
	if status, _, body := get("/user/usage", cookie); status != http.StatusOK ||
		!strings.Contains(body, "Your usage") || !strings.Contains(body, "org/public") ||
		!strings.Contains(body, `<h2>Reported tokens</h2><p class="metric">7</p>`) ||
		!strings.Contains(body, `name="jawsKey"`) {
		t.Fatalf("signed-in usage route: status=%d body=%s", status, body)
	}
	_, _, page := get("/user/usage", cookie)
	key := adminJaWSKey(t, page)
	if status, _, _ := get("/jaws/"+key, nil); status != http.StatusForbidden {
		t.Fatalf("anonymous usage JaWS request status=%d", status)
	}
	if status, _, _ := get("/jaws/"+key, cookie); status == http.StatusForbidden {
		t.Fatalf("authenticated usage JaWS request denied: status=%d", status)
	}
	if status, headers, body := get("/user/usage.css", cookie); status != http.StatusOK ||
		!strings.HasPrefix(headers.Get("Content-Type"), "text/css") || !strings.Contains(body, ".topbar") {
		t.Fatalf("user stylesheet route: status=%d content-type=%q body=%s", status, headers.Get("Content-Type"), body)
	}
}

func TestUserUsageExpiredSessionReturnsToContinuation(t *testing.T) {
	oauthClient := &http.Client{Transport: oauthRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://github.com/login/oauth/access_token":
			return oauthJSON(http.StatusOK, `{"access_token":"token","token_type":"Bearer","expires_in":3600}`), nil
		case "https://api.github.com/user":
			return oauthJSON(http.StatusOK, `{"id":99,"login":"reader"}`), nil
		}
		t.Errorf("unexpected OAuth request: %s", r.URL)
		return oauthJSON(http.StatusNotFound, `{}`), nil
	})}
	owner := &action.Orchestrator{Usage: &usage.Store{Dir: t.TempDir()}}
	jw, oauth, handler := userUsageTestHandler(t, owner, oauthClient)
	oldCookie := userUsageCookie(t, jw, (userIdentity{Provider: "github", Host: "github.com", Subject: "99"}).key(), false)
	request := httptest.NewRequest(http.MethodGet, "https://admin.example/user/usage", nil)
	request.RemoteAddr = "192.0.2.7:12345"
	request.AddCookie(oldCookie)
	jw.GetSession(request).Close()

	target := userUsageSignedPath(oauth, 99, "org/repo-31", 3)
	denied := userUsageRequest(handler, http.MethodGet, target, "", oldCookie)
	if denied.Code != http.StatusSeeOther || denied.Header().Get("Location") != "/auth/login?next="+url.QueryEscape(target) {
		t.Fatalf("expired continuation: status=%d location=%q", denied.Code, denied.Header().Get("Location"))
	}
	login := userUsageRequest(handler, http.MethodGet, denied.Header().Get("Location"), "", oldCookie)
	startURL := userUsageLinkPath(login.Body.String(), "Sign in with GitHub")
	if login.Code != http.StatusOK || startURL != "/auth/github/login?next="+url.QueryEscape(target) || len(login.Result().Cookies()) != 1 {
		t.Fatalf("continuation sign-in link: status=%d link=%q cookies=%v", login.Code, startURL, login.Result().Cookies())
	}
	start := userUsageRequest(handler, http.MethodGet, startURL, "", login.Result().Cookies()[0])
	location, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || location.Query().Get("state") == "" {
		t.Fatalf("start sign-in: status=%d location=%q cookies=%v error=%v", start.Code, start.Header().Get("Location"), start.Result().Cookies(), err)
	}
	callback := userUsageRequest(handler, http.MethodGet, "/auth/github/callback?code=proof&state="+url.QueryEscape(location.Query().Get("state")), "", login.Result().Cookies()[0])
	if callback.Code != http.StatusFound || callback.Header().Get("Location") != target || len(callback.Result().Cookies()) != 1 {
		t.Fatalf("sign-in return: status=%d location=%q body=%q", callback.Code, callback.Header().Get("Location"), callback.Body.String())
	}
	if page := userUsageRequest(handler, http.MethodGet, target, "", callback.Result().Cookies()[0]); page.Code != http.StatusOK {
		t.Fatalf("returned usage page: status=%d body=%q", page.Code, page.Body.String())
	}
}

func TestUserUsageSignInReturnRejectsOtherTargets(t *testing.T) {
	for _, raw := range []string{"//evil.example/user/usage", "https://evil.example/user/usage", "/admin/usage", "/user/usage#fragment", "/user/usage?other=x", "/user/usage?repo=bad%2Frepo%2Fpath", "/user/usage?after=-1"} {
		if got := userUsageReturnTarget(raw); got != "" {
			t.Errorf("accepted unsafe sign-in return %q: %q", raw, got)
		}
	}
	_, _, handler := userUsageTestHandler(t, nil)
	for _, target := range []string{"//evil.example/user/usage", "/admin/usage"} {
		login := userUsageRequest(handler, http.MethodGet, "/auth/login?next="+url.QueryEscape(target), "")
		if login.Code != http.StatusOK || strings.Contains(login.Body.String(), "evil.example") ||
			userUsageLinkPath(login.Body.String(), "Sign in with GitHub") != "/auth/github/login" {
			t.Fatalf("unsafe login return %q: %d/%s", target, login.Code, login.Body.String())
		}
	}
}

func TestUserUsageEstimatePrecision(t *testing.T) {
	var page strings.Builder
	render := func(dot userUsagePage) {
		t.Helper()
		if err := userUsageHTML.Execute(&page, struct {
			Dot      userUsagePage
			HeadHTML string
			TailHTML string
		}{Dot: dot}); err != nil {
			t.Fatal(err)
		}
	}
	tiny := usage.Summary{Repo: "org/repo", EstimatedCostUSD: 1e-7, EstimatedCostKnownCalls: 1}
	tinyRow := usage.Summary{Repo: "org/repo", EstimatedCostUSD: 2e-7, EstimatedCostKnownCalls: 1}
	render(userUsagePage{Total: tiny, Repositories: []usage.Summary{tinyRow}})
	for _, want := range []string{"USD 1e-07 reported estimated total", "USD 2e-07 reported estimated total"} {
		if strings.Count(page.String(), want) != 1 {
			t.Fatalf("positive total and row estimates were not distinct: %s", page.String())
		}
	}
	page.Reset()
	large := usage.Summary{Repo: "org/repo", EstimatedCostUSD: 123456789.12, EstimatedCostKnownCalls: 1}
	largeRow := usage.Summary{Repo: "org/repo", EstimatedCostUSD: 123456789.34, EstimatedCostKnownCalls: 1}
	render(userUsagePage{Total: large, Repositories: []usage.Summary{largeRow}})
	for _, want := range []string{"USD 123456789.12 reported estimated total", "USD 123456789.34 reported estimated total"} {
		if strings.Count(html.UnescapeString(page.String()), want) != 1 {
			t.Fatalf("large total and row estimates lost fractional dollars: %s", page.String())
		}
	}
	page.Reset()
	a, b := 0.3, 0.2
	noisy := a - b
	render(userUsagePage{Total: usage.Summary{EstimatedCostUSD: noisy, EstimatedCostKnownCalls: 1}})
	if !strings.Contains(page.String(), "USD 0.1 reported estimated total") ||
		strings.Contains(page.String(), "0.099999999999999") {
		t.Fatalf("binary subtraction noise leaked into estimate: %s", page.String())
	}
	page.Reset()
	render(userUsagePage{Total: usage.Summary{EstimatedCostUnknownCalls: 3}})
	if !strings.Contains(page.String(), "Estimated cost and currency</dt><dd>Unknown (calls with any reported amount: 0; calls with missing or incomplete estimates: 3)") {
		t.Fatalf("missing estimate count was hidden: %s", page.String())
	}
}

func finishUserUsageCall(t *testing.T, record *usage.Record, provider, role string, tokens usage.Tokens, coverage usage.Coverage, complete bool, estimate *float64) {
	t.Helper()
	call := usage.Call{Ordinal: len(record.Calls) + 1, StartedAt: record.StartedAt.Add(time.Duration(len(record.Calls)+1) * time.Second),
		Provider: provider, Model: role, Role: role, Round: 1}
	if role == "critic" {
		call.ReviewPhase = "full"
	}
	if _, err := record.BeginCall(call); err != nil {
		t.Fatal(err)
	}
	finish := call.StartedAt.Add(time.Second)
	call.FinishedAt, call.Tokens, call.UsageCoverage, call.Complete = &finish, tokens, coverage, complete
	call.EstimatedCostUSD = estimate
	call.EstimatedCostComplete = estimate != nil && complete
	if _, err := record.FinishCall(call); err != nil {
		t.Fatal(err)
	}
}

func TestUserUsageRevokedPrivateRowsHideIdentityAndSignedCursor(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repositories/"):
			id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repositories/"), 10, 64)
			if err != nil || id < 1001 || id > 1020 {
				t.Errorf("unexpected repository lookup: %s", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"id":%d,"full_name":"org/private-%02d","private":true,"visibility":"private"}`, id, id-1000)
		case r.URL.Path == "/user/42":
			fmt.Fprint(w, `{"id":42,"login":"alice"}`)
		case strings.Contains(r.URL.Path, "/collaborators/"):
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected GitHub lookup: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	githubClient := github.New("fixture-token")
	githubClient.BaseURL = api.URL
	store := &usage.Store{Dir: t.TempDir()}
	for i := 1; i <= 20; i++ {
		record := userUsageRecord(i, fmt.Sprintf("org/private-%02d", i), int64(1000+i), 42, "alice", "succeeded")
		record.CodexCalls, record.Tokens, record.UsageSource, record.UsageComplete = 1, usage.Tokens{Output: 1}, "executor_reported", true
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	jw, _, handler := userUsageTestHandler(t, &action.Orchestrator{GitHub: githubClient, Usage: store})
	cookie := userUsageCookie(t, jw, (userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key(), false)
	first := userUsageRequest(handler, http.MethodGet, "/user/usage", "", cookie)
	continueURL := userUsageContinuePath(first.Body.String())
	if first.Code != http.StatusOK || continueURL == "" || !strings.Contains(first.Body.String(), `<h2>Reported tokens</h2><p class="metric">20</p>`) || strings.Contains(first.Body.String(), "org/private-") {
		t.Fatalf("revoked private page disclosed identity or lost total: status=%d continue=%q body=%s", first.Code, continueURL, first.Body.String())
	}
	for _, path := range []string{"/user/usage?after=19", continueURL + "&after=1000000", continueURL + "&repo=org%2Fprivate-01"} {
		forged := userUsageRequest(handler, http.MethodGet, path, "", cookie)
		if forged.Code != http.StatusBadRequest || strings.Contains(forged.Body.String(), "org/private-") || strings.Contains(forged.Body.String(), "Reported tokens") {
			t.Fatalf("forged cursor exposed data: path=%q status=%d body=%q", path, forged.Code, forged.Body.String())
		}
	}
	other := userUsageCookie(t, jw, (userIdentity{Provider: "github", Host: "github.com", Subject: "99"}).key(), false)
	if cross := userUsageRequest(handler, http.MethodGet, continueURL, "", other); cross.Code != http.StatusBadRequest || strings.Contains(cross.Body.String(), "org/private-") {
		t.Fatalf("cross-account cursor accepted: status=%d body=%s", cross.Code, cross.Body.String())
	}
	issued := userUsageRequest(handler, http.MethodGet, continueURL, "", cookie)
	if issued.Code != http.StatusOK || strings.Contains(issued.Body.String(), "org/private-") || !strings.Contains(issued.Body.String(), `<h2>Reported tokens</h2><p class="metric">20</p>`) {
		t.Fatalf("issued cursor failed: status=%d body=%s", issued.Code, issued.Body.String())
	}
}

func TestUserUsageOwnTotalsPrivateAccessAndRetention(t *testing.T) {
	var revoked, wrongID, wrongRepoID, nameReused, renameReuseAfterPermission, unknownVisibility, verificationFailed atomic.Bool
	permissionCalls := atomic.Int64{}
	apiCalls := atomic.Int64{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		switch strings.ToLower(r.URL.Path) {
		case "/user/42":
			fmt.Fprint(w, `{"id":42,"login":"new-alice"}`)
		case "/repositories/101":
			if verificationFailed.Load() {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			id := 101
			if wrongRepoID.Load() {
				id = 999
			}
			fmt.Fprintf(w, `{"id":%d,"full_name":"org/accessible","private":true,"visibility":"private"}`, id)
		case "/repositories/103":
			fmt.Fprint(w, `{"id":103,"full_name":"org/hidden","private":true,"visibility":"private"}`)
		case "/repositories/102":
			if unknownVisibility.Load() {
				fmt.Fprint(w, `{"id":102,"full_name":"org/public"}`)
			} else {
				fmt.Fprint(w, `{"id":102,"full_name":"org/public","private":false,"visibility":"public"}`)
			}
		case "/repos/org/accessible":
			id := 101
			if wrongRepoID.Load() || nameReused.Load() {
				id = 999
			}
			fmt.Fprintf(w, `{"id":%d,"full_name":"org/accessible","private":true,"visibility":"private"}`, id)
		case "/repos/org/accessible/collaborators/new-alice/permission":
			permissionCalls.Add(1)
			if renameReuseAfterPermission.Load() {
				nameReused.Store(true)
			}
			if revoked.Load() {
				http.NotFound(w, r)
				return
			}
			id := 42
			if wrongID.Load() {
				id = 44
			}
			fmt.Fprintf(w, `{"permission":"read","user":{"id":%d,"login":"new-alice"}}`, id)
		case "/repos/org/hidden/collaborators/new-alice/permission":
			permissionCalls.Add(1)
			fmt.Fprint(w, `{"permission":"none","user":{"id":42,"login":"new-alice"}}`)
		case "/repos/org/public/collaborators/new-alice/permission":
			if !unknownVisibility.Load() {
				t.Error("public repository required a collaborator check")
			}
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected GitHub API request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	client := github.New("fixture-token")
	client.BaseURL = api.URL
	usageStore := &usage.Store{Dir: t.TempDir()}
	owner := &action.Orchestrator{GitHub: client, Usage: usageStore, WorkRoot: t.TempDir()}
	jw, oauth, handler := userUsageTestHandler(t, owner)
	first := (userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key()
	userCookie := userUsageCookie(t, jw, first, false)
	adminCookie := userUsageCookie(t, jw, "", true)

	mainAndCritic := userUsageRecord(1, "org/old-name", 101, 42, "alice", "failed")
	mainAndCritic.CriticProvider, mainAndCritic.CriticModel = "claude", "critic"
	finishUserUsageCall(t, &mainAndCritic, "codex", "main", usage.Tokens{Input: 10, CacheCreationInput: 2, CachedInput: 3, Output: 2}, usage.Coverage{Scope: "invocation"}, true, nil)
	criticEstimate := 0.125
	finishUserUsageCall(t, &mainAndCritic, "claude", "critic", usage.Tokens{Input: 20, Output: 3},
		usage.Coverage{Scope: "known_subset", UnverifiedHelpers: true, PartialReport: true}, false, &criticEstimate)
	if err := usageStore.Save(mainAndCritic); err != nil {
		t.Fatal(err)
	}
	late := userUsageRecord(2, "org/accessible", 101, 42, "alice", "failed") // Terminal stop, then a late usage receipt.
	pending := usage.Call{Ordinal: 1, StartedAt: late.StartedAt.Add(time.Second), Provider: "codex", Model: "main", Role: "main", Round: 1}
	if _, err := late.BeginCall(pending); err != nil {
		t.Fatal(err)
	}
	if err := usageStore.Save(late); err != nil {
		t.Fatal(err)
	}
	lateAt := late.FinishedAt.Add(time.Second)
	pending.FinishedAt, pending.Tokens, pending.UsageCoverage, pending.Complete = &lateAt,
		usage.Tokens{Input: 7, Output: 1}, usage.Coverage{Scope: "invocation"}, true
	if _, err := late.FinishCall(pending); err != nil {
		t.Fatal(err)
	}
	if err := usageStore.Save(late); err != nil {
		t.Fatal(err)
	}
	public := userUsageRecord(3, "org/public", 102, 42, "alice", "succeeded")
	public.CodexCalls, public.Tokens, public.UsageSource, public.UsageComplete = 1,
		usage.Tokens{Input: 3, Output: 1}, "executor_reported", true
	hidden := userUsageRecord(4, "org/hidden", 103, 42, "alice", "failed")
	hidden.CodexCalls, hidden.Tokens, hidden.UsageSource, hidden.UsageComplete = 1,
		usage.Tokens{Input: 5, Output: 1}, "executor_reported", true
	other := userUsageRecord(5, "other/private", 104, 44, "alice", "succeeded")
	other.CodexCalls, other.Tokens, other.UsageSource, other.UsageComplete = 1,
		usage.Tokens{Input: 999, Output: 1}, "executor_reported", true
	legacy := userUsageRecord(6, "legacy/private", 105, 0, "alice", "succeeded")
	legacy.CodexCalls, legacy.Tokens, legacy.UsageSource, legacy.UsageComplete = 1,
		usage.Tokens{Input: 998, Output: 1}, "executor_reported", true
	for _, record := range []usage.Record{public, hidden, other, legacy} {
		if err := usageStore.Save(record); err != nil {
			t.Fatal(err)
		}
	}

	response := userUsageRequest(handler, http.MethodGet, "/user/usage", "", userCookie)
	body := response.Body.String()
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(body, `<h2>Reported tokens</h2><p class="metric">53</p>`) ||
		!strings.Contains(body, `<h2>Unallocated tokens</h2><p class="metric">10</p>`) ||
		!strings.Contains(body, `<h2>Usage-recorded actions</h2><p class="metric">4</p>`) ||
		!strings.Contains(body, `<h2>Incomplete attempts</h2><p class="metric">1</p>`) ||
		!strings.Contains(body, "4 (1 succeeded, 3 failed or stopped, 0 running)") ||
		!strings.Contains(body, "Cache creation input</dt><dd>2") ||
		!strings.Contains(body, "Cached input</dt><dd>3") ||
		!strings.Contains(body, "Call checkpoints</dt><dd>4 Codex, 1 Claude") ||
		!regexp.MustCompile(`<th scope="row">org/accessible</th><td class="number">2</td><td class="number">43</td>\s*<td class="number">0</td><td class="number">1</td>\s*<td>Unknown</td><td>Unknown</td><td>USD 0\.125 known estimated subtotal \(calls with any reported amount: 1; calls with missing or incomplete estimates: 3; counts may overlap\)</td><td>Unknown</td>`).MatchString(body) ||
		!regexp.MustCompile(`<th scope="row">org/public</th><td class="number">1</td><td class="number">4</td>\s*<td class="number">4</td><td class="number">0</td>\s*<td>Unknown</td><td>Unknown</td><td>Unknown \(calls with any reported amount: 0; calls with missing or incomplete estimates: 1\)</td><td>Unknown</td>`).MatchString(body) || strings.Contains(body, "org/old-name") ||
		strings.Contains(body, "org/hidden") || strings.Contains(body, "other/private") ||
		strings.Contains(body, "legacy/private") ||
		!strings.Contains(body, "Billed credits</dt><dd>Unknown") ||
		!strings.Contains(body, "Reported cost and currency</dt><dd>Unknown") ||
		!strings.Contains(body, "Estimated cost and currency</dt><dd>USD 0.125 known estimated subtotal (calls with any reported amount: 1; calls with missing or incomplete estimates: 5; counts may overlap)") ||
		!strings.Contains(body, "Billed cost and currency</dt><dd>Unknown") ||
		!strings.Contains(body, `<th scope="col">Reported cost and currency</th>`) ||
		!strings.Contains(body, `<th scope="col">Estimated cost and currency</th>`) ||
		!strings.Contains(body, `<th scope="col">Billed cost and currency</th>`) ||
		!strings.Contains(body, `aria-labelledby="accessible-repositories-heading" tabindex="0"`) ||
		!strings.Contains(body, `<h2 id="accessible-repositories-heading">Accessible repositories</h2>`) ||
		!strings.Contains(body, "executor-reported claims") ||
		!strings.Contains(body, "main, critic, and helper usage") {
		t.Fatalf("owner usage page: status=%d body=%s", response.Code, body)
	}

	if permissionCalls.Load() < 2 {
		t.Fatalf("private repositories were not reauthorized: calls=%d", permissionCalls.Load())
	}
	if filtered := userUsageRequest(handler, http.MethodGet, "/user/usage?repo=ORG%2FPUBLIC", "", userCookie); filtered.Code != http.StatusOK ||
		!strings.Contains(filtered.Body.String(), "org/public") || strings.Contains(filtered.Body.String(), "org/accessible") ||
		!strings.Contains(filtered.Body.String(), `<h2>Reported tokens</h2><p class="metric">53</p>`) {
		t.Fatalf("repository filter changed owner total or exposed other rows: status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	beforeFiltered := apiCalls.Load()
	if renamed := userUsageRequest(handler, http.MethodGet, "/user/usage?repo=ORG%2FACCESSIBLE", "", userCookie); renamed.Code != http.StatusOK ||
		!strings.Contains(renamed.Body.String(), `<th scope="row">org/accessible</th>`) || strings.Contains(renamed.Body.String(), "org/old-name") ||
		strings.Contains(renamed.Body.String(), `<th scope="row">org/public</th>`) || apiCalls.Load()-beforeFiltered != 4 ||
		userUsageContinuePath(renamed.Body.String()) != "" {
		t.Fatalf("filter did not match verified canonical name after rename: status=%d body=%s", renamed.Code, renamed.Body.String())
	}
	setQuota := func(n int) {
		oauth.usageGate.mu.Lock()
		oauth.usageGate.apiTokens, oauth.usageGate.apiBurst, oauth.usageGate.accountBurst = n, n, n
		oauth.usageGate.apiRefillAt = time.Now().Add(time.Hour) // Hold this exact 3/4-call boundary through slow runs.
		oauth.usageGate.accounts = make(map[string]userUsageAccountBudget)
		oauth.usageGate.mu.Unlock()
	}
	setQuota(3)
	before := apiCalls.Load()
	short := userUsageRequest(handler, http.MethodGet, "/user/usage?repo=org%2Faccessible", "", userCookie)
	if short.Code != http.StatusOK || apiCalls.Load() != before || strings.Contains(short.Body.String(), `<th scope="row">org/accessible</th>`) ||
		userUsageContinuePath(short.Body.String()) != "" ||
		userUsageRetryPath(short.Body.String()) != "/user/usage?repo=org%2Faccessible" ||
		!strings.Contains(short.Body.String(), "Repository verification is incomplete at this position") ||
		strings.Contains(short.Body.String(), "No matching accessible repository") {
		t.Fatalf("three tokens consumed a private lookup without enough budget to bind permission: %d/%s", short.Code, short.Body.String())
	}
	setQuota(4)
	before = apiCalls.Load()
	full := userUsageRequest(handler, http.MethodGet, "/user/usage?repo=org%2Faccessible", "", userCookie)
	if full.Code != http.StatusOK || apiCalls.Load()-before != 4 || !strings.Contains(full.Body.String(), `<th scope="row">org/accessible</th>`) {
		t.Fatalf("four-call private permission proof did not reach the row: %d/%s (%d calls)", full.Code, full.Body.String(), apiCalls.Load()-before)
	}
	setQuota(1000)
	oauth.usageGate.verificationTimeout = 0
	before = apiCalls.Load()
	expired := userUsageRequest(handler, http.MethodGet, "/user/usage?repo=org%2Faccessible", "", userCookie)
	oauth.usageGate.verificationTimeout = 8 * time.Second
	if expired.Code != http.StatusOK || apiCalls.Load() != before || userUsageContinuePath(expired.Body.String()) != "" ||
		userUsageRetryPath(expired.Body.String()) != "/user/usage?repo=org%2Faccessible" ||
		!strings.Contains(expired.Body.String(), "Repository verification is incomplete at this position") {
		t.Fatalf("expired verification before GitHub lookup offered the wrong navigation: %d/%s", expired.Code, expired.Body.String())
	}
	for _, path := range []string{"/user/usage?repo=org%2Fhidden&repo=org%2Fpublic", "/user/usage?other=value", "/user/usage?repo=bad%2Frepo%2Fpath", "/user/usage?after=-1", "/user/usage?who=-1", "/user/usage?retry=-1", "/user/usage?shape=2.3"} {
		if response := userUsageRequest(handler, http.MethodGet, path, "", userCookie); response.Code != http.StatusBadRequest {
			t.Errorf("invalid filter %s: status=%d", path, response.Code)
		}
	}
	if guest := userUsageRequest(handler, http.MethodGet, "/user/usage", ""); guest.Code != http.StatusSeeOther || guest.Header().Get("Location") != "/auth/login?next=%2Fuser%2Fusage" ||
		strings.Contains(guest.Body.String(), "org/accessible") || strings.Contains(guest.Body.String(), "Reported tokens") {
		t.Fatalf("guest usage response: status=%d body=%s", guest.Code, guest.Body.String())
	}
	if admin := userUsageRequest(handler, http.MethodGet, "/user/usage", "", adminCookie); admin.Code != http.StatusSeeOther ||
		strings.Contains(admin.Body.String(), "org/accessible") || strings.Contains(admin.Body.String(), "Reported tokens") {
		t.Fatalf("admin-only session reached user usage: status=%d body=%s", admin.Code, admin.Body.String())
	}
	logoutCookie := userUsageCookie(t, jw, first, false)
	if logout := userUsageRequest(handler, http.MethodPost, "/auth/logout", "https://admin.example", logoutCookie); logout.Code != http.StatusSeeOther {
		t.Fatalf("user logout status=%d", logout.Code)
	}
	if denied := userUsageRequest(handler, http.MethodGet, "/user/usage", "", logoutCookie); denied.Code != http.StatusSeeOther ||
		strings.Contains(denied.Body.String(), "org/accessible") || strings.Contains(denied.Body.String(), "Reported tokens") {
		t.Fatalf("revoked user reached usage: status=%d body=%s", denied.Code, denied.Body.String())
	}
	if stylesheet := userUsageRequest(handler, http.MethodGet, "/user/usage.css", "", userCookie); stylesheet.Code != http.StatusOK ||
		stylesheet.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatalf("user usage stylesheet: status=%d", stylesheet.Code)
	}

	revoked.Store(true)
	if response := userUsageRequest(handler, http.MethodGet, "/user/usage", "", userCookie); response.Code != http.StatusOK ||
		strings.Contains(response.Body.String(), "org/accessible") ||
		!strings.Contains(response.Body.String(), `<h2>Reported tokens</h2><p class="metric">53</p>`) {
		t.Fatalf("revoked private access lost aggregate or exposed row: status=%d body=%s", response.Code, response.Body.String())
	}
	revoked.Store(false)

	wrongID.Store(true)
	if response := userUsageRequest(handler, http.MethodGet, "/user/usage", "", userCookie); strings.Contains(response.Body.String(), "org/accessible") ||
		!strings.Contains(response.Body.String(), `<h2>Reported tokens</h2><p class="metric">53</p>`) {
		t.Fatalf("permission for another numeric ID exposed private repository: %s", response.Body.String())
	}
	wrongID.Store(false)
	wrongRepoID.Store(true)
	if response := userUsageRequest(handler, http.MethodGet, "/user/usage", "", userCookie); strings.Contains(response.Body.String(), "org/accessible") ||
		!strings.Contains(response.Body.String(), `<h2>Reported tokens</h2><p class="metric">53</p>`) {
		t.Fatalf("reused repository name exposed previous repository usage: %s", response.Body.String())
	}
	wrongRepoID.Store(false)
	renameReuseAfterPermission.Store(true)
	if response := userUsageRequest(handler, http.MethodGet, "/user/usage", "", userCookie); strings.Contains(response.Body.String(), `<th scope="row">org/accessible</th>`) ||
		!strings.Contains(response.Body.String(), "Some repository usage could not be verified") ||
		!strings.Contains(response.Body.String(), `<h2>Reported tokens</h2><p class="metric">53</p>`) {
		t.Fatalf("name reuse between permission and ID recheck exposed another repository's usage: %s", response.Body.String())
	}
	renameReuseAfterPermission.Store(false)
	nameReused.Store(false)
	verificationFailed.Store(true)
	failed := userUsageRequest(handler, http.MethodGet, "/user/usage", "", userCookie)
	if response := failed; response.Code != http.StatusOK ||
		strings.Contains(response.Body.String(), "org/accessible") ||
		!strings.Contains(response.Body.String(), "Some repository usage could not be verified") ||
		userUsageRetryPath(response.Body.String()) != "/user/usage" ||
		!strings.Contains(response.Body.String(), `<h2>Reported tokens</h2><p class="metric">53</p>`) {
		t.Fatalf("GitHub failure looked like access denial or lost totals: %s", response.Body.String())
	}
	verificationFailed.Store(false)
	if retried := userUsageRequest(handler, http.MethodGet, userUsageRetryPath(failed.Body.String()), "", userCookie); retried.Code != http.StatusOK ||
		!strings.Contains(retried.Body.String(), `<th scope="row">org/accessible</th>`) {
		t.Fatalf("GitHub recovery skipped the failed repository: status=%d body=%s", retried.Code, retried.Body.String())
	}
	unknownVisibility.Store(true)
	if response := userUsageRequest(handler, http.MethodGet, "/user/usage", "", userCookie); strings.Contains(response.Body.String(), "org/public") ||
		!strings.Contains(response.Body.String(), `<h2>Reported tokens</h2><p class="metric">53</p>`) {
		t.Fatalf("unknown visibility was treated as public: %s", response.Body.String())
	}
	unknownVisibility.Store(false)
	revoked.Store(true)
	before = apiCalls.Load()
	revokedOwned := userUsageRequest(handler, http.MethodGet, "/user/usage?repo=org%2Faccessible", "", userCookie)
	revokedCalls := apiCalls.Load() - before
	before = apiCalls.Load()
	unownedName := userUsageRequest(handler, http.MethodGet, "/user/usage?repo=org%2Fmissing", "", userCookie)
	unownedCalls := apiCalls.Load() - before
	normalizeFilter := func(body string) string {
		_, body, _ = strings.Cut(body, "<main>")
		body, _, _ = strings.Cut(body, "</main>")
		return strings.ReplaceAll(strings.ReplaceAll(body, "org/accessible", "org/checked"), "org/missing", "org/checked")
	}
	if revokedOwned.Code != http.StatusOK || unownedName.Code != http.StatusOK ||
		normalizeFilter(revokedOwned.Body.String()) != normalizeFilter(unownedName.Body.String()) ||
		userUsageContinuePath(revokedOwned.Body.String()) != "" || userUsageRetryPath(revokedOwned.Body.String()) != "" ||
		!strings.Contains(revokedOwned.Body.String(), "No matching accessible repository was verified") ||
		revokedCalls != unownedCalls {
		t.Fatalf("filtered revoked-owned repository revealed historic usage: revoked=%d/%s (%d API calls) unowned=%d/%s (%d API calls)", revokedOwned.Code, revokedOwned.Body.String(), revokedCalls, unownedName.Code, unownedName.Body.String(), unownedCalls)
	}
	if response := userUsageRequest(handler, http.MethodGet, "/user/usage", "", userCookie); strings.Contains(response.Body.String(), "org/accessible") ||
		strings.Contains(response.Body.String(), "org/hidden") || !strings.Contains(response.Body.String(), "org/public") ||
		strings.Contains(response.Body.String(), "Some repository usage could not be verified") ||
		!strings.Contains(response.Body.String(), `<h2>Reported tokens</h2><p class="metric">53</p>`) ||
		!strings.Contains(response.Body.String(), "Estimated cost and currency</dt><dd>USD 0.125 known estimated subtotal") {
		t.Fatalf("lost private access changed aggregate or leaked breakdown: %s", response.Body.String())
	}

	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	stateStore, err := state.Open(filepath.Join(t.TempDir(), "state.json"), now)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	stateStore.Data.TerminalDecisions = map[string]state.TerminalDecision{"late": {
		AttemptID: late.ID, Outcome: "failed", DecidedAt: now.Add(-31 * 24 * time.Hour)}}
	if err := stateStore.Save(); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(owner.WorkRoot, "attempt-records", late.ID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(artifact), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("private evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := owner.PrunePrivateRecords(t.Context(), stateStore, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("private artifact remained after retention: %v", err)
	}
	oauth.usageGate.mu.Lock()
	oauth.usageGate.readAt = time.Now().Add(-userUsageSnapshotTTL)
	oauth.usageGate.mu.Unlock()
	if response := userUsageRequest(handler, http.MethodGet, "/user/usage", "", userCookie); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `<h2>Reported tokens</h2><p class="metric">53</p>`) ||
		!strings.Contains(response.Body.String(), "Estimated cost and currency</dt><dd>USD 0.125 known estimated subtotal") {
		t.Fatalf("retention removed owner billing total: status=%d body=%s", response.Code, response.Body.String())
	}
	oauth.usageGate.mu.Lock()
	oauth.usageGate.apiTokens, oauth.usageGate.apiRefillAt = 0, time.Now()
	oauth.usageGate.mu.Unlock()
	if response := userUsageRequest(handler, http.MethodGet, "/user/usage", "", userCookie); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Some repository usage could not be verified") ||
		!strings.Contains(response.Body.String(), `<h2>Reported tokens</h2><p class="metric">53</p>`) ||
		strings.Contains(response.Body.String(), "org/accessible") {
		t.Fatalf("API quota exhaustion changed totals or exposed unverified row: status=%d body=%s", response.Code, response.Body.String())
	}
	usageStore.Dir = t.TempDir()
	legacyOnlyRecord := userUsageRecord(7, "org/legacy-only", 0, 42, "alice", "failed")
	legacyOnlyRecord.CodexCalls, legacyOnlyRecord.Tokens, legacyOnlyRecord.UsageSource, legacyOnlyRecord.UsageComplete =
		1, usage.Tokens{Input: 7, Output: 2}, "executor_reported", true
	if err := usageStore.Save(legacyOnlyRecord); err != nil {
		t.Fatal(err)
	}
	oauth.usageGate.mu.Lock()
	oauth.usageGate.readAt = time.Time{}
	oauth.usageGate.apiTokens = 1000
	oauth.usageGate.mu.Unlock()
	before = apiCalls.Load()
	if legacyOnly := userUsageRequest(handler, http.MethodGet, "/user/usage?repo=org%2Flegacy-only", "", userCookie); legacyOnly.Code != http.StatusOK ||
		!strings.Contains(legacyOnly.Body.String(), `<h2>Reported tokens</h2><p class="metric">9</p>`) ||
		!strings.Contains(legacyOnly.Body.String(), "No matching accessible repository was verified") ||
		strings.Contains(legacyOnly.Body.String(), "Some repository usage could not be verified") || apiCalls.Load() != before {
		t.Fatalf("legacy-only filter called GitHub or showed a repository row: status=%d body=%s", legacyOnly.Code, legacyOnly.Body.String())
	}
}

func TestUserUsageGateBoundsDiskAndGitHubWork(t *testing.T) {
	store := &usage.Store{Dir: t.TempDir()}
	first := userUsageRecord(1, "org/first", 101, 42, "alice", "failed")
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	gate := newUserUsageGate()
	records, busy, err := gate.read(store.Dir)
	if err != nil || busy || len(records) != 1 {
		t.Fatalf("first snapshot: records=%d busy=%t error=%v", len(records), busy, err)
	}
	second := userUsageRecord(2, "org/second", 102, 42, "alice", "failed")
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}
	records, busy, err = gate.read(store.Dir)
	if err != nil || busy || len(records) != 1 {
		t.Fatalf("snapshot TTL did not bound disk work: records=%d busy=%t error=%v", len(records), busy, err)
	}
	gate.mu.Lock()
	gate.readAt = time.Now().Add(-userUsageSnapshotTTL)
	gate.mu.Unlock()
	gate.scan <- struct{}{}
	_, busy, err = gate.read(store.Dir)
	if err != nil || !busy {
		t.Fatalf("concurrent full scan was allowed: busy=%t error=%v", busy, err)
	}
	<-gate.scan
	records, busy, err = gate.read(store.Dir)
	if err != nil || busy || len(records) != 2 {
		t.Fatalf("expired snapshot did not refresh: records=%d busy=%t error=%v", len(records), busy, err)
	}
	for _, account := range []string{"account-a", "account-b"} {
		for range userUsageAccountBurst {
			if !gate.takeGitHubCall(t.Context(), account) {
				t.Fatalf("GitHub call budget ended early for %s", account)
			}
		}
		if gate.takeGitHubCall(t.Context(), account) {
			t.Fatalf("account %s exceeded its share", account)
		}
	}
	if gate.takeGitHubCall(t.Context(), "account-c") {
		t.Fatal("global GitHub call budget allowed an extra call")
	}
	gate.mu.Lock()
	gate.apiRefillAt = time.Now().Add(-2 * userUsageAPIInterval)
	budget := gate.accounts["account-a"]
	budget.refillAt = time.Now().Add(-userUsageAccountInterval)
	gate.accounts["account-a"] = budget
	gate.mu.Unlock()
	for _, account := range []string{"account-a", "account-c"} {
		if !gate.takeGitHubCall(t.Context(), account) {
			t.Fatalf("shared budget did not leave a refill for %s", account)
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	gate.mu.Lock()
	before := gate.apiTokens
	gate.mu.Unlock()
	if gate.takeGitHubCall(canceled, "account-c") {
		t.Fatal("canceled request consumed GitHub call budget")
	}
	gate.mu.Lock()
	after := gate.apiTokens
	gate.mu.Unlock()
	if before != after {
		t.Fatalf("canceled request changed GitHub tokens from %d to %d", before, after)
	}
	gate.mu.Lock()
	gate.readAt = time.Now().Add(-userUsageSnapshotTTL)
	gate.mu.Unlock()
	badPath := filepath.Join(store.Dir, "broken.json")
	if err := os.WriteFile(badPath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gate.read(store.Dir); err == nil {
		t.Fatal("malformed usage store was accepted")
	}
	if err := os.Remove(badPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gate.read(store.Dir); err == nil {
		t.Fatal("failed scan was immediately retried")
	}
	gate.mu.Lock()
	gate.readAt = time.Now().Add(-userUsageSnapshotTTL)
	gate.mu.Unlock()
	if records, busy, err := gate.read(store.Dir); err != nil || busy || len(records) != 2 {
		t.Fatalf("failed scan did not recover after TTL: records=%d busy=%t error=%v", len(records), busy, err)
	}
}

func TestUserUsageFilteredAndPagedRowsStayReachable(t *testing.T) {
	var repositoryFailure atomic.Int64
	var slow, lastRepositoryFailure atomic.Bool
	var apiCalls, slowRepositoryCalls atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		path := r.URL.Path
		if path == "/user/42" {
			fmt.Fprint(w, `{"id":42,"login":"current"}`)
			return
		}
		if strings.HasPrefix(path, "/repositories/") {
			id, err := strconv.Atoi(strings.TrimPrefix(path, "/repositories/"))
			if err != nil || id < 200 || id > 231 {
				t.Errorf("unexpected repository ID path %s", path)
				http.NotFound(w, r)
				return
			}
			if id == 231 && slow.Load() {
				slowRepositoryCalls.Add(1)
				<-r.Context().Done()
				return
			}
			if id == 231 && lastRepositoryFailure.Load() {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			if id == 201 && repositoryFailure.Load() != 0 {
				if repositoryFailure.Load() == 1 {
					http.NotFound(w, r)
				} else {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				}
				return
			}
			fmt.Fprintf(w, `{"id":%d,"full_name":"org/repo-%02d","private":true,"visibility":"private"}`, id, id-200)
			return
		}
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) >= 3 && parts[0] == "repos" && parts[1] == "org" && strings.HasPrefix(parts[2], "repo-") {
			number, err := strconv.Atoi(strings.TrimPrefix(parts[2], "repo-"))
			if err == nil && number >= 0 && number <= 31 {
				if len(parts) == 6 && parts[3] == "collaborators" && parts[4] == "current" && parts[5] == "permission" {
					fmt.Fprint(w, `{"permission":"read","user":{"id":42,"login":"current"}}`)
					return
				}
				if len(parts) == 3 {
					fmt.Fprintf(w, `{"id":%d,"full_name":"org/repo-%02d","private":true,"visibility":"private"}`, number+200, number)
					return
				}
			}
		}
		t.Errorf("unexpected GitHub API request: %s", path)
		http.NotFound(w, r)
	}))
	defer api.Close()
	client := github.New("fixture-token")
	client.BaseURL = api.URL
	store := &usage.Store{Dir: t.TempDir()}
	for id := 1; id <= 31; id++ {
		if err := store.Save(userUsageRecord(id, fmt.Sprintf("org/repo-%02d", id), int64(id+200), 42, "old-login", "failed")); err != nil {
			t.Fatal(err)
		}
	}
	jw, oauth, handler := userUsageTestHandler(t, &action.Orchestrator{GitHub: client, Usage: store})
	cookie := userUsageCookie(t, jw, (userIdentity{Provider: "github", Host: "github.com", Subject: "42"}).key(), false)
	setBudget := func(global, account int) {
		oauth.usageGate.mu.Lock()
		oauth.usageGate.apiTokens, oauth.usageGate.apiBurst, oauth.usageGate.accountBurst = global, global, account
		oauth.usageGate.apiRefillAt = time.Now()
		oauth.usageGate.accounts = make(map[string]userUsageAccountBudget)
		oauth.usageGate.mu.Unlock()
	}
	setBudget(userUsageAPIBurst, userUsageAccountBurst)
	path := "/user/usage?repo=org%2Frepo-31"
	startCalls := apiCalls.Load()
	found := false
	for range 8 {
		before := apiCalls.Load()
		page := userUsageRequest(handler, http.MethodGet, path, "", cookie)
		if page.Code != http.StatusOK || apiCalls.Load()-before > userUsagePageCallLimit || strings.Contains(page.Body.String(), `<th scope="row">org/repo-01</th>`) {
			t.Fatalf("filtered scan exceeded cap or exposed another row: status=%d calls=%d body=%s", page.Code, apiCalls.Load()-before, page.Body.String())
		}
		if strings.Contains(page.Body.String(), `<th scope="row">org/repo-31</th>`) {
			found = true
			break
		}
		if !strings.Contains(page.Body.String(), "No matching accessible repository on this page") {
			t.Fatalf("early page announced final denial: %d/%s", page.Code, page.Body.String())
		}
		path = userUsageContinuePath(page.Body.String())
		if !strings.Contains(path, "repo=org%2Frepo-31") {
			t.Fatalf("filter continuation lost name: %q", path)
		}
	}
	if !found {
		t.Fatal("31st private repository was not reachable")
	}
	if used := apiCalls.Load() - startCalls; used > userUsageAccountBurst {
		t.Fatalf("31st repo needed refill: calls=%d burst=%d", used, userUsageAccountBurst)
	} else {
		t.Logf("31st private repo verified with %d calls", used)
	}
	setBudget(1000, 1000)
	missing := userUsageRequest(handler, http.MethodGet, "/user/usage?repo=org%2Fmissing", "", cookie)
	if missing.Code != http.StatusOK || !strings.Contains(missing.Body.String(), "No matching accessible repository on this page") {
		t.Fatalf("missing filter leaked row: %d/%s", missing.Code, missing.Body.String())
	}
	path = userUsageContinuePath(missing.Body.String())
	finalDenial := false
	for range 8 {
		if path == "" {
			break
		}
		page := userUsageRequest(handler, http.MethodGet, path, "", cookie)
		path = userUsageContinuePath(page.Body.String())
		if path == "" && page.Code == http.StatusOK && strings.Contains(page.Body.String(), "No matching accessible repository was verified") {
			finalDenial = true
		}
	}
	if !finalDenial {
		t.Fatal("filtered scan did not end with neutral result")
	}
	first := userUsageRequest(handler, http.MethodGet, "/user/usage", "", cookie)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "org/repo-01") || strings.Contains(first.Body.String(), "org/repo-31") || userUsageContinuePath(first.Body.String()) != userUsageSignedPath(oauth, 42, "", 4) {
		t.Fatalf("first repository page: %d/%s", first.Code, first.Body.String())
	}
	last := userUsageRequest(handler, http.MethodGet, userUsageSignedPath(oauth, 42, "", 30), "", cookie)
	if last.Code != http.StatusOK || !strings.Contains(last.Body.String(), "org/repo-31") || strings.Contains(last.Body.String(), "org/repo-01") || userUsageContinuePath(last.Body.String()) != "" {
		t.Fatalf("last repository page: %d/%s", last.Code, last.Body.String())
	}
	for _, failure := range []int64{1, 2} {
		repositoryFailure.Store(failure)
		failed := userUsageRequest(handler, http.MethodGet, "/user/usage", "", cookie)
		forward, retry := userUsageContinuePath(failed.Body.String()), userUsageRetryPath(failed.Body.String())
		if failed.Code != http.StatusOK || forward == "" || retry != "/user/usage" || !strings.Contains(failed.Body.String(), "Some repository usage could not be verified") || strings.Contains(failed.Body.String(), `<th scope="row">org/repo-01</th>`) || !strings.Contains(failed.Body.String(), `<th scope="row">org/repo-02</th>`) {
			t.Fatalf("unavailable group %d blocked later row or lost retry: %d/%s", failure, failed.Code, failed.Body.String())
		}
		filtered := userUsageRequest(handler, http.MethodGet, "/user/usage?repo=org%2Frepo-01", "", cookie)
		if filtered.Code != http.StatusOK || userUsageRetryPath(filtered.Body.String()) != "/user/usage?repo=org%2Frepo-01" || userUsageContinuePath(filtered.Body.String()) == "" || strings.Contains(filtered.Body.String(), `<th scope="row">org/repo-01</th>`) {
			t.Fatalf("filtered failure %d lost neutral retry/continue: %d/%s", failure, filtered.Code, filtered.Body.String())
		}
		repositoryFailure.Store(0)
		if recovered := userUsageRequest(handler, http.MethodGet, retry, "", cookie); recovered.Code != http.StatusOK || !strings.Contains(recovered.Body.String(), `<th scope="row">org/repo-01</th>`) {
			t.Fatalf("recovered repo skipped: %d/%s", recovered.Code, recovered.Body.String())
		}
	}
	slow.Store(true)
	oauth.usageGate.verificationTimeout = 500 * time.Millisecond
	beforeSlow := slowRepositoryCalls.Load()
	started := time.Now()
	timed := userUsageRequest(handler, http.MethodGet, userUsageSignedPath(oauth, 42, "", 30), "", cookie)
	if time.Since(started) > 2*time.Second || timed.Code != http.StatusOK || slowRepositoryCalls.Load() <= beforeSlow || userUsageRetryPath(timed.Body.String()) != userUsageSignedPath(oauth, 42, "", 30) || strings.Contains(timed.Body.String(), "org/repo-31") {
		t.Fatalf("deadline lost retry or exposed row: status=%d body=%s", timed.Code, timed.Body.String())
	}
	slow.Store(false)
	oauth.usageGate.verificationTimeout = 8 * time.Second
	lastRepositoryFailure.Store(true)
	filteredFailed := userUsageRequest(handler, http.MethodGet, userUsageSignedPath(oauth, 42, "org/repo-31", 30), "", cookie)
	lastRepositoryFailure.Store(false)
	if filteredFailed.Code != http.StatusOK || userUsageRetryPath(filteredFailed.Body.String()) != userUsageSignedPath(oauth, 42, "org/repo-31", 30) || strings.Contains(filteredFailed.Body.String(), "No matching accessible repository was verified") {
		t.Fatalf("last filtered failure sounded final: %d/%s", filteredFailed.Code, filteredFailed.Body.String())
	}
	if err := store.Save(userUsageRecord(32, "org/repo-00", 200, 42, "old-login", "failed")); err != nil {
		t.Fatal(err)
	}
	oauth.usageGate.mu.Lock()
	oauth.usageGate.readAt = time.Now().Add(-userUsageSnapshotTTL)
	oauth.usageGate.mu.Unlock()
	shifted := userUsageRequest(handler, http.MethodGet, userUsageContinuePath(first.Body.String()), "", cookie)
	if shifted.Code != http.StatusOK || !strings.Contains(shifted.Body.String(), `<th scope="row">org/repo-04</th>`) || !strings.Contains(shifted.Body.String(), `<th scope="row">org/repo-05</th>`) || !strings.Contains(shifted.Body.String(), "new usage or account-link changes can repeat or skip rows") {
		t.Fatalf("new lower repository ID skipped later groups: %d/%s", shifted.Code, shifted.Body.String())
	}
}
