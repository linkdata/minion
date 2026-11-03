package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func dashboardReadPatch(t *testing.T, tr *jawstest.TestRequest, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case message, ok := <-tr.OutCh:
			if !ok {
				t.Fatalf("live channel closed before patch containing %q", want)
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
		!strings.Contains(matching.Body.String(), "Tracked accepted actions") || !strings.Contains(matching.Body.String(), "7m0s") ||
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
	if patch := dashboardReadPatch(t, other, "No current action matches these filters.", 3*time.Second); strings.Contains(patch, "owner/repo") {
		t.Fatalf("other request received the matching action: %s", patch)
	}
	if logout := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); logout.Code != http.StatusSeeOther {
		t.Fatalf("logout status=%d", logout.Code)
	}
	jw.Dirty(adminDashboardDirty)
	if patch := dashboardReadPatch(t, tr, "Admin session ended", 3*time.Second); strings.Contains(patch, "owner/repo") || strings.Contains(patch, "Selected worker") {
		t.Fatalf("revoked session received private live state: %s", patch)
	}
	if patch := dashboardReadPatch(t, other, "Admin session ended", 3*time.Second); strings.Contains(patch, "owner/repo") || strings.Contains(patch, "Selected worker") {
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
	cookie := dashboardTestCookie(t, admin.server.Handler)
	tr := dashboardLiveRequest(t, admin.jaws, owner, cookie, adminDashboardFilter{})
	store.Data.Accepted = dashboardTestAccepted(time.Now().UTC())
	store.Data.AcceptedTotal = 1
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if patch := dashboardReadPatch(t, tr, "owner/repo", 5*time.Second); !strings.Contains(patch, "Current action") {
		t.Fatalf("ticker patch omitted action section: %s", patch)
	}
}
