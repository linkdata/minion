package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/linkdata/jaws"
	"github.com/linkdata/jaws/jawstest"
	"github.com/linkdata/jaws/lib/ui"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
)

func dashboardTestServer(t *testing.T) (http.Handler, *jaws.Jaws, *state.Store, *action.Orchestrator) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := &action.Orchestrator{Store: store, MaxRuntime: 7 * time.Minute, InactivityTimeout: time.Minute}
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()}}, owner)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })
	return handler, jw, store, owner
}

func dashboardTestAccepted(at time.Time) *state.AcceptedAction {
	return &state.AcceptedAction{JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "local",
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2, SubjectType: "Issue", SubjectNumber: 7,
		SourceKey: "source", SourceNodeID: "source-node", Requester: "writer", RequesterID: 3,
		CommandName: "comment", Action: "comment", AcceptedAt: at, Deadline: at.Add(7 * time.Minute)}
}

func dashboardTestCookie(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	response := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"username": {"operator"}, "password": {"correct-password"}}, nil, "")
	if response.Code != http.StatusSeeOther || len(response.Result().Cookies()) != 1 {
		t.Fatalf("admin login: status=%d cookies=%d", response.Code, len(response.Result().Cookies()))
	}
	return response.Result().Cookies()[0]
}

func dashboardLiveRequest(t *testing.T, jw *jaws.Jaws, owner *action.Orchestrator, cookie *http.Cookie, filter adminDashboardFilter) *jawstest.TestRequest {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "https://admin.example/", nil)
	request.AddCookie(cookie)
	tr := jawstest.NewTestRequest(jw, request)
	<-tr.ReadyCh
	t.Cleanup(func() {
		tr.Close()
		for range tr.OutCh {
		}
		<-tr.DoneCh
	})
	rw := ui.RequestWriter{Request: tr.Request, Writer: tr.Recorder}
	if err := rw.Template("section", "dashboard-live", &adminDashboardPage{owner: owner, Filter: filter}); err != nil {
		t.Fatal(err)
	}
	return tr
}

func dashboardReadPatch(t *testing.T, tr *jawstest.TestRequest, want string, timeout time.Duration, forbidden ...string) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case message, ok := <-tr.OutCh:
			if !ok {
				t.Fatalf("live channel closed before patch containing %q", want)
			}
			for _, text := range forbidden {
				if strings.Contains(message.Data, text) {
					t.Fatalf("live patch exposed %q: %s", text, message.Data)
				}
			}
			if strings.Contains(message.Data, want) {
				return message.Data
			}
		case <-deadline:
			t.Fatalf("live patch missing %q", want)
		}
	}
}

func TestAdminDashboardRequiresSessionAndKeepsFiltersPerRequest(t *testing.T) {
	handler, _, store, _ := dashboardTestServer(t)
	store.Data.Accepted = dashboardTestAccepted(time.Now().UTC())
	store.Data.AcceptedTotal = 3
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if denied := adminResponse(handler, http.MethodGet, "/", nil, nil, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("anonymous dashboard status=%d, want 403", denied.Code)
	}
	if denied := adminResponse(handler, http.MethodGet, "/admin/dashboard.css", nil, nil, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("anonymous dashboard CSS status=%d, want 403", denied.Code)
	}
	cookie := dashboardTestCookie(t, handler)
	matching := adminResponse(handler, http.MethodGet, "/?repo=OWNER&requester=writ&action=comment", nil, cookie, "")
	if matching.Code != http.StatusOK || matching.Header().Get("Cache-Control") != "no-store" ||
		matching.Header().Get("Content-Security-Policy") == "" || !strings.Contains(matching.Body.String(), "<td>owner/repo</td>") ||
		!strings.Contains(matching.Body.String(), `<p class="metric">3</p>`) ||
		!strings.Contains(matching.Body.String(), `<p class="metric">1</p>`) || !strings.Contains(matching.Body.String(), "7m0s") ||
		!strings.Contains(matching.Body.String(), "1m0s") {
		t.Fatalf("matching dashboard: status=%d body=%s", matching.Code, matching.Body.String())
	}
	filtered := adminResponse(handler, http.MethodGet, "/?repo=other", nil, cookie, "")
	if filtered.Code != http.StatusOK || strings.Contains(filtered.Body.String(), "<td>owner/repo</td>") ||
		!strings.Contains(filtered.Body.String(), "No current action matches these filters.") ||
		!strings.Contains(filtered.Body.String(), `name="repo" value="other"`) {
		t.Fatalf("request-local filter failed: status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	plain := adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if plain.Code != http.StatusOK || !strings.Contains(plain.Body.String(), "<td>owner/repo</td>") ||
		strings.Contains(plain.Body.String(), `name="repo" value="other"`) {
		t.Fatalf("filter leaked to another request: status=%d body=%s", plain.Code, plain.Body.String())
	}
	hostile := adminResponse(handler, http.MethodGet, "/?repo=%3Cscript%3Ealert(1)%3C%2Fscript%3E", nil, cookie, "")
	if hostile.Code != http.StatusOK || strings.Contains(hostile.Body.String(), "<script>alert(1)</script>") ||
		!strings.Contains(hostile.Body.String(), "&lt;script&gt;") {
		t.Fatalf("unescaped filter: status=%d body=%s", hostile.Code, hostile.Body.String())
	}
	longRepo := strings.Repeat("o", 100) + "/" + strings.Repeat("r", 100)
	longRequester := strings.Repeat("u", 100) + "[bot]"
	store.Data.Accepted.Repository, store.Data.Accepted.Requester = longRepo, longRequester
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	longURL := "/?repo=" + url.QueryEscape(longRepo) + "&requester=" + url.QueryEscape(longRequester)
	longNames := adminResponse(handler, http.MethodGet, longURL, nil, cookie, "")
	if longNames.Code != http.StatusOK || !strings.Contains(longNames.Body.String(), "<td>"+longRepo+"</td>") ||
		!strings.Contains(longNames.Body.String(), "<td>"+longRequester+"</td>") ||
		!strings.Contains(longNames.Body.String(), `maxlength="201"`) || !strings.Contains(longNames.Body.String(), `maxlength="105"`) {
		t.Fatalf("valid maximum-length filters failed: status=%d body=%s", longNames.Code, longNames.Body.String())
	}
	for _, tooLong := range []string{"/?repo=" + strings.Repeat("r", 202), "/?requester=" + strings.Repeat("u", 106)} {
		if invalid := adminResponse(handler, http.MethodGet, tooLong, nil, cookie, ""); invalid.Code != http.StatusBadRequest {
			t.Fatalf("overlong filter %q status=%d, want 400", tooLong, invalid.Code)
		}
	}
	if invalid := adminResponse(handler, http.MethodGet, "/?action=publish", nil, cookie, ""); invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid action filter status=%d, want 400", invalid.Code)
	}
	css := adminResponse(handler, http.MethodGet, "/admin/dashboard.css", nil, cookie, "")
	if css.Code != http.StatusOK || css.Header().Get("Cache-Control") != "no-store" || !strings.Contains(css.Body.String(), ".metrics") {
		t.Fatalf("authenticated stylesheet: status=%d", css.Code)
	}
	if logout := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); logout.Code != http.StatusSeeOther {
		t.Fatalf("logout status=%d", logout.Code)
	}
	if denied := adminResponse(handler, http.MethodGet, "/", nil, cookie, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("logged-out dashboard status=%d, want 403", denied.Code)
	}
}

func TestAdminDashboardLiveUpdatesStopAfterLogout(t *testing.T) {
	handler, jw, store, owner := dashboardTestServer(t)
	cookie := dashboardTestCookie(t, handler)
	tr := dashboardLiveRequest(t, jw, owner, cookie, adminDashboardFilter{Repo: "owner/repo"})
	other := dashboardLiveRequest(t, jw, owner, cookie, adminDashboardFilter{Repo: "other/repo"})
	if !strings.Contains(tr.BodyString(), "No action is in progress.") {
		t.Fatalf("initial live region: %s", tr.BodyString())
	}
	store.Data.Accepted = dashboardTestAccepted(time.Now().UTC())
	store.Data.AcceptedTotal = 1
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	jw.Dirty(adminDashboardDirty)
	if patch := dashboardReadPatch(t, tr, "owner/repo", 3*time.Second); !strings.Contains(patch, "Current action") {
		t.Fatalf("live patch omitted action section: %s", patch)
	}
	if patch := dashboardReadPatch(t, other, "No current action matches these filters.", 3*time.Second, "owner/repo"); strings.Contains(patch, "owner/repo") {
		t.Fatalf("other request received the matching action: %s", patch)
	}
	if logout := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); logout.Code != http.StatusSeeOther {
		t.Fatalf("logout status=%d", logout.Code)
	}
	jw.Dirty(adminDashboardDirty)
	if patch := dashboardReadPatch(t, tr, "Admin session ended", 3*time.Second, "owner/repo", "Selected worker"); strings.Contains(patch, "owner/repo") || strings.Contains(patch, "Selected worker") {
		t.Fatalf("revoked session received private live state: %s", patch)
	}
	if patch := dashboardReadPatch(t, other, "Admin session ended", 3*time.Second, "owner/repo", "Selected worker"); strings.Contains(patch, "owner/repo") || strings.Contains(patch, "Selected worker") {
		t.Fatalf("other revoked session received private live state: %s", patch)
	}
}

func TestAdminDashboardTickerPublishesSavedChanges(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := &action.Orchestrator{Store: store}
	admin, err := startAdminServer(t.Context(), config.AdminConfig{Listen: "127.0.0.1:0",
		Administrators: []config.AdminAccount{testAdminAccount()}}, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	baseURL := "http://" + admin.listener.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	login, err := client.PostForm(baseURL+"/admin/login", url.Values{"username": {"operator"}, "password": {"correct-password"}})
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	if login.StatusCode != http.StatusSeeOther || len(login.Cookies()) != 1 {
		t.Fatalf("network login: status=%d cookies=%d", login.StatusCode, len(login.Cookies()))
	}
	cookie := login.Cookies()[0]
	wsContext, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	connect := func(path string) *websocket.Conn {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(cookie)
		page, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		pageBody, readErr := io.ReadAll(io.LimitReader(page.Body, 1<<20))
		closeErr := page.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read dashboard: read=%v close=%v", readErr, closeErr)
		}
		if page.StatusCode != http.StatusOK || !strings.Contains(string(pageBody), "No action is in progress.") {
			t.Fatalf("network dashboard %s: status=%d body=%s", path, page.StatusCode, pageBody)
		}
		_, afterKey, found := strings.Cut(string(pageBody), `<meta name="jawsKey" content="`)
		if !found {
			t.Fatal("dashboard omitted JaWS connection key")
		}
		key, _, found := strings.Cut(afterKey, `"`)
		if !found || key == "" {
			t.Fatal("dashboard JaWS connection key is empty")
		}
		header := http.Header{}
		header.Set("Cookie", cookie.Name+"="+cookie.Value)
		header.Set("Origin", baseURL)
		conn, upgrade, err := websocket.Dial(wsContext, "ws://"+admin.listener.Addr().String()+"/jaws/"+key,
			&websocket.DialOptions{HTTPHeader: header})
		if err != nil {
			t.Fatalf("authenticated dashboard WebSocket %s: %v", path, err)
		}
		if upgrade == nil || upgrade.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("dashboard WebSocket upgrade: %+v", upgrade)
		}
		return conn
	}
	matching := connect("/?repo=owner")
	defer matching.Close(websocket.StatusNormalClosure, "")
	other := connect("/?repo=other")
	defer other.Close(websocket.StatusNormalClosure, "")
	store.Data.Accepted = dashboardTestAccepted(time.Now().UTC())
	store.Data.AcceptedTotal = 1
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	readPatch := func(conn *websocket.Conn, want string, forbidden ...string) string {
		t.Helper()
		for {
			_, patch, err := conn.Read(wsContext)
			if err != nil {
				t.Fatalf("ticker did not patch authenticated WebSocket: %v", err)
			}
			for _, text := range forbidden {
				if strings.Contains(string(patch), text) {
					t.Fatalf("filtered WebSocket patch exposed %q: %s", text, patch)
				}
			}
			if strings.Contains(string(patch), want) {
				return string(patch)
			}
		}
	}
	if patch := readPatch(matching, "owner/repo"); !strings.Contains(patch, "Current action") {
		t.Fatalf("ticker patch omitted action section: %s", patch)
	}
	readPatch(other, "No current action matches these filters.", "owner/repo")
}
