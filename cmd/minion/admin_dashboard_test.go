package main

import (
	"bytes"
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
	handler, err := newAdminHandler(jw, config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()}}, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })
	return handler, jw, store, owner
}

func dashboardTestAccepted(at time.Time) *state.AcceptedAction {
	return &state.AcceptedAction{MainProvider: "codex", MainSettings: frozenTestCodex, JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "local",
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2, SubjectType: "Issue", SubjectNumber: 7,
		SourceKey: "source", SourceNodeID: "source-node", Requester: "writer", RequesterID: 3,
		CommandName: "comment", Action: "comment", AcceptedAt: at, Deadline: at.Add(7 * time.Minute)}
}

func dashboardTestCookie(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	response := adminResponse(handler, http.MethodPost, "/admin/login",
		url.Values{"password": {"correct-password"}}, adminGuestCookie(t, handler), "")
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
	if err := rw.Template("section", "dashboard-action", &adminDashboardPage{owner: owner, Filter: filter}); err != nil {
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

func TestAdminDashboardRendersWorkerTelemetry(t *testing.T) {
	pages := adminDashboardTemplate
	var page struct {
		Auth struct{ IsAdmin bool }
		Dot  struct{ View adminDashboardView }
	}
	page.Auth.IsAdmin = true
	page.Dot.View = adminDashboardView{Snapshot: action.DashboardSnapshot{Worker: action.DashboardWorker{
		Kind: "remote", Readiness: "busy", State: "busy", Phase: "critic", Round: 2, ReviewPhase: "verification",
		LastContact: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC), ReportedProgressAge: 7 * time.Second,
	}}}
	var body bytes.Buffer
	if err := pages.ExecuteTemplate(&body, "dashboard-live", page); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<dt>Location</dt><dd>remote</dd>",
		"<dt>Readiness</dt><dd>busy</dd>",
		"<dt>Reported state</dt><dd>busy</dd>",
		"<dt>Phase</dt><dd>critic / round 2 / verification</dd>",
		"<dt>Last worker status received</dt><dd>2026-11-03T12:00:00Z</dd>",
		"<dt>Worker-reported progress age at last status</dt><dd>7s</dd>",
	} {
		if !strings.Contains(body.String(), want) {
			t.Fatalf("worker telemetry missing %q: %s", want, body.String())
		}
	}
	var status bytes.Buffer
	if err := pages.ExecuteTemplate(&status, "dashboard-status", page); err != nil {
		t.Fatal(err)
	}
	const workerStatus = "Worker readiness busy. Reported state busy. Worker phase critic / round 2 / verification. Tracked accepted actions 0."
	if status.String() != workerStatus+" In progress 0." {
		t.Fatalf("live status summary: %q", status.String())
	}
	var stopStatus bytes.Buffer
	if err := pages.ExecuteTemplate(&stopStatus, "dashboard-stop-status", page); err != nil || stopStatus.Len() != 0 {
		t.Fatalf("unexpected initial stop announcement: %q, %v", stopStatus.String(), err)
	}
	page.Dot.View.Snapshot.TrackedAccepted = 1
	status.Reset()
	if err := pages.ExecuteTemplate(&status, "dashboard-status", page); err != nil ||
		status.String() != strings.Replace(workerStatus, "actions 0.", "actions 1.", 1)+" In progress 0." {
		t.Fatalf("count-only change did not update live status summary: %q, %v", status.String(), err)
	}
	page.Dot.View.Snapshot.TrackedAccepted = 0
	page.Dot.View.Action = &action.DashboardAction{Repository: "owner/repo", Requester: "writer", Action: "review",
		Phase: "critic", AcceptedAt: time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC), Deadline: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	page.Dot.View.Snapshot.InProgress = 1
	const actionSummary = " In progress 1. Current review action in owner/repo for writer, accepted 2026-11-03T11:00:00Z, phase critic."
	status.Reset()
	if err := pages.ExecuteTemplate(&status, "dashboard-status", page); err != nil ||
		status.String() != workerStatus+actionSummary {
		t.Fatalf("action phase did not change live status summary: %q, %v", status.String(), err)
	}
	firstAction := page.Dot.View.Action
	replacement := *firstAction
	replacement.AcceptedAt = replacement.AcceptedAt.Add(time.Second)
	page.Dot.View.Action = &replacement
	status.Reset()
	if err := pages.ExecuteTemplate(&status, "dashboard-status", page); err != nil ||
		!strings.Contains(status.String(), "accepted 2026-11-03T11:00:01Z") ||
		strings.Contains(status.String(), "accepted 2026-11-03T11:00:00Z") {
		t.Fatalf("same-phase replacement did not change live status summary: %q, %v", status.String(), err)
	}
	page.Dot.View.Action = firstAction
	var actionBefore bytes.Buffer
	if err := pages.ExecuteTemplate(&actionBefore, "dashboard-action", page); err != nil {
		t.Fatal(err)
	}
	page.Dot.View.Snapshot.Worker.LastContact = page.Dot.View.Snapshot.Worker.LastContact.Add(time.Second)
	page.Dot.View.Snapshot.Worker.ReportedProgressAge = 13 * time.Second
	body.Reset()
	if err := pages.ExecuteTemplate(&body, "dashboard-live", page); err != nil ||
		!strings.Contains(body.String(), "<dt>Last worker status received</dt><dd>2026-11-03T12:00:01Z</dd>") ||
		!strings.Contains(body.String(), "<dt>Worker-reported progress age at last status</dt><dd>13s</dd>") ||
		strings.Contains(body.String(), "<dd>2026-11-03T12:00:00Z</dd>") || strings.Contains(body.String(), "<dd>7s</dd>") {
		t.Fatalf("worker telemetry did not update: %q, %v", body.String(), err)
	}
	status.Reset()
	if err := pages.ExecuteTemplate(&status, "dashboard-status", page); err != nil ||
		status.String() != workerStatus+actionSummary {
		t.Fatalf("heartbeat changed live status summary: %q, %v", status.String(), err)
	}
	page.Dot.View.Snapshot.Worker.ReviewPhase = "full"
	status.Reset()
	if err := pages.ExecuteTemplate(&status, "dashboard-status", page); err != nil ||
		status.String() != "Worker readiness busy. Reported state busy. Worker phase critic / round 2 / full. Tracked accepted actions 0."+actionSummary {
		t.Fatalf("review phase did not change live status summary: %q, %v", status.String(), err)
	}
	page.Dot.View.Snapshot.Worker.State = "stopping"
	page.Dot.View.Snapshot.Worker.Phase = "cleanup"
	page.Dot.View.Snapshot.Worker.Round = 0
	page.Dot.View.Snapshot.Worker.ReviewPhase = ""
	status.Reset()
	if err := pages.ExecuteTemplate(&status, "dashboard-status", page); err != nil ||
		status.String() != "Worker readiness busy. Reported state stopping. Worker phase cleanup. Tracked accepted actions 0."+actionSummary {
		t.Fatalf("worker state did not change live status summary: %q, %v", status.String(), err)
	}
	var actionAfter bytes.Buffer
	if err := pages.ExecuteTemplate(&actionAfter, "dashboard-action", page); err != nil ||
		actionBefore.String() != actionAfter.String() || !strings.Contains(actionAfter.String(), "owner/repo") {
		t.Fatalf("worker heartbeat changed action HTML: %q, %q, %v", actionBefore.String(), actionAfter.String(), err)
	}
	page.Dot.View.Action.JobID, page.Dot.View.Action.AttemptID = "job-1", "attempt-1"
	page.Dot.View.Action.Stoppable = true
	var stopBefore, stopAfter bytes.Buffer
	if err := pages.ExecuteTemplate(&stopBefore, "dashboard-stop-control", page); err != nil ||
		!strings.Contains(stopBefore.String(), "Force stop this attempt") {
		t.Fatalf("selected stop control missing: %q, %v", stopBefore.String(), err)
	}
	page.Dot.View.Action.Phase = "publishing"
	if err := pages.ExecuteTemplate(&stopAfter, "dashboard-stop-control", page); err != nil ||
		stopBefore.String() != stopAfter.String() {
		t.Fatalf("phase update replaced the focused stop control: %q, %q, %v", stopBefore.String(), stopAfter.String(), err)
	}
	page.Dot.View.Snapshot.LastStop = &action.DashboardStop{AttemptID: "attempt-1", Status: "requested"}
	if err := pages.ExecuteTemplate(&stopStatus, "dashboard-stop-status", page); err != nil ||
		stopStatus.String() != "Latest stop request for attempt attempt-1: requested." {
		t.Fatalf("stop request missing from dedicated status: %q, %v", stopStatus.String(), err)
	}
	page.Dot.View.Snapshot.Worker.Phase = "idle"
	status.Reset()
	if err := pages.ExecuteTemplate(&status, "dashboard-status", page); err != nil ||
		!strings.Contains(status.String(), "Worker phase idle.") || strings.Contains(status.String(), "Latest stop request") {
		t.Fatalf("worker change repeated stop announcement: %q, %v", status.String(), err)
	}
	stopStatus.Reset()
	if err := pages.ExecuteTemplate(&stopStatus, "dashboard-stop-status", page); err != nil ||
		stopStatus.String() != "Latest stop request for attempt attempt-1: requested." {
		t.Fatalf("worker change replaced stop status: %q, %v", stopStatus.String(), err)
	}
	page.Dot.View.Snapshot.LastStop.Status = "confirmed"
	stopStatus.Reset()
	if err := pages.ExecuteTemplate(&stopStatus, "dashboard-stop-status", page); err != nil ||
		stopStatus.String() != "Latest stop request for attempt attempt-1: confirmed." {
		t.Fatalf("confirmed stop was not announced: %q, %v", stopStatus.String(), err)
	}
	body.Reset()
	if err := pages.ExecuteTemplate(&body, "dashboard-live", page); err != nil ||
		!strings.Contains(body.String(), "Latest stop request") || strings.Contains(body.String(), `aria-live="polite"`) {
		t.Fatalf("visual stop panel duplicated live announcement: %q, %v", body.String(), err)
	}
	page.Auth.IsAdmin = false
	status.Reset()
	if err := pages.ExecuteTemplate(&status, "dashboard-status", page); err != nil || status.String() != "Admin session ended." {
		t.Fatalf("revoked live status: %q, %v", status.String(), err)
	}
	stopStatus.Reset()
	if err := pages.ExecuteTemplate(&stopStatus, "dashboard-stop-status", page); err != nil || stopStatus.Len() != 0 {
		t.Fatalf("revoked stop status: %q, %v", stopStatus.String(), err)
	}
	stopAfter.Reset()
	if err := pages.ExecuteTemplate(&stopAfter, "dashboard-stop-control", page); err != nil || stopAfter.Len() != 0 {
		t.Fatalf("revoked stop control: %q, %v", stopAfter.String(), err)
	}
}

func TestAdminDashboardRequiresSessionAndKeepsFiltersPerRequest(t *testing.T) {
	handler, _, store, _ := dashboardTestServer(t)
	store.Data.Accepted = dashboardTestAccepted(time.Now().UTC())
	store.Data.AcceptedTotal = 3
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if home := adminResponse(handler, http.MethodGet, "/", nil, nil, ""); home.Code != http.StatusOK ||
		!strings.Contains(home.Body.String(), `href="/admin/login"`) || strings.Contains(home.Body.String(), "Live actions") ||
		len(home.Result().Cookies()) != 1 {
		t.Fatalf("anonymous home: status=%d cookies=%v body=%q", home.Code, home.Result().Cookies(), home.Body.String())
	}
	if head := adminResponse(handler, http.MethodHead, "/", nil, nil, ""); head.Code != http.StatusOK || head.Body.Len() != 0 || len(head.Result().Cookies()) != 0 {
		t.Fatalf("anonymous home HEAD: status=%d cookies=%v body=%q", head.Code, head.Result().Cookies(), head.Body.String())
	}
	cookie := dashboardTestCookie(t, handler)
	matching := adminResponse(handler, http.MethodGet, "/?repo=OWNER&requester=writ&action=comment", nil, cookie, "")
	if matching.Code != http.StatusOK || matching.Header().Get("Cache-Control") != "no-store" ||
		matching.Header().Get("Content-Security-Policy") == "" || !strings.Contains(matching.Body.String(), "<td>owner/repo</td>") ||
		!strings.Contains(matching.Body.String(), "role=status aria-live=polite class=visually-hidden") ||
		!hasMetric(matching.Body.String(), "Tracked accepted actions", "3") || !hasMetric(matching.Body.String(), "In progress", "1") ||
		!strings.Contains(matching.Body.String(), `tabindex="0" aria-label="Current action"`) ||
		!strings.Contains(matching.Body.String(), "<dt>Hard action runtime</dt><dd>7m0s</dd>") ||
		!strings.Contains(matching.Body.String(), "<dt>Executor inactivity</dt><dd>1m0s</dd>") {
		t.Fatalf("matching dashboard: status=%d body=%s", matching.Code, matching.Body.String())
	}
	if css := adminResponse(handler, http.MethodGet, bootstrapAssetPath(t, matching.Body.String(), ".css"), nil, nil, ""); css.Code != http.StatusOK || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("public Bootstrap CSS: status=%d content-type=%q", css.Code, css.Header().Get("Content-Type"))
	}
	filtered := adminResponse(handler, http.MethodGet, "/?repo=other", nil, cookie, "")
	if filtered.Code != http.StatusOK || strings.Contains(filtered.Body.String(), "<td>owner/repo</td>") ||
		!strings.Contains(filtered.Body.String(), "No current action matches these filters.") ||
		!hasMetric(filtered.Body.String(), "Tracked accepted actions", "3") || !hasMetric(filtered.Body.String(), "In progress", "1") ||
		!strings.Contains(filtered.Body.String(), `name="repo" value="other"`) {
		t.Fatalf("request-local filter failed: status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	for _, path := range []string{"/?requester=other", "/?action=review"} {
		mismatch := adminResponse(handler, http.MethodGet, path, nil, cookie, "")
		if mismatch.Code != http.StatusOK || strings.Contains(mismatch.Body.String(), "<td>owner/repo</td>") ||
			!strings.Contains(mismatch.Body.String(), "No current action matches these filters.") ||
			!hasMetric(mismatch.Body.String(), "Tracked accepted actions", "3") || !hasMetric(mismatch.Body.String(), "In progress", "1") {
			t.Fatalf("filter %s did not preserve global counts: status=%d body=%s", path, mismatch.Code, mismatch.Body.String())
		}
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
	if logout := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != "/" {
		t.Fatalf("logout status=%d location=%q", logout.Code, logout.Header().Get("Location"))
	}
	if home := adminResponse(handler, http.MethodGet, "/", nil, cookie, ""); home.Code != http.StatusOK ||
		!strings.Contains(home.Body.String(), `href="/admin/login"`) || strings.Contains(home.Body.String(), "Live actions") {
		t.Fatalf("logged-out home: status=%d body=%q", home.Code, home.Body.String())
	}
}

func TestAdminDashboardUnchangedDirtyPatch(t *testing.T) {
	handler, jw, _, owner := dashboardTestServer(t)
	tr := dashboardLiveRequest(t, jw, owner, dashboardTestCookie(t, handler), adminDashboardFilter{})
	jw.Dirty(adminDashboardDirty)
	first := dashboardReadPatch(t, tr, "No action is in progress.", 3*time.Second)
	jw.Dirty(adminDashboardDirty)
	if second := dashboardReadPatch(t, tr, "No action is in progress.", 3*time.Second); second != first {
		t.Fatalf("unchanged dashboard sent different HTML: first=%s second=%s", first, second)
	}
}

func TestAdminForceStopUsesSessionAndSelectedAttempt(t *testing.T) {
	handler, _, store, _ := dashboardTestServer(t)
	accepted := dashboardTestAccepted(time.Now().UTC())
	store.Data.Accepted = accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"job_id": {accepted.JobID}, "attempt_id": {accepted.AttemptID}, "requested_by": {"spoofed"}}
	cookie := dashboardTestCookie(t, handler)
	if got := adminResponse(handler, http.MethodGet, "/", nil, cookie, ""); got.Code != http.StatusOK ||
		strings.Contains(got.Body.String(), "Force stop this attempt") {
		t.Fatalf("preparation showed stop control: status=%d body=%s", got.Code, got.Body.String())
	}
	if got := adminResponse(handler, http.MethodPost, "/admin/stop", form, nil, "https://admin.example"); got.Code != http.StatusForbidden {
		t.Fatalf("anonymous stop status=%d", got.Code)
	}
	if got := adminResponse(handler, http.MethodPost, "/admin/stop", form, cookie, "https://admin.example"); got.Code != http.StatusConflict {
		t.Fatalf("preparation stop status=%d", got.Code)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := store.RegisterSelectedStop(accepted.JobID, accepted.AttemptID, cancel)
	defer unregister()
	for _, test := range []struct {
		name string
		form url.Values
	}{
		{"duplicate job", url.Values{"job_id": {accepted.JobID, accepted.JobID}, "attempt_id": {accepted.AttemptID}}},
		{"duplicate attempt", url.Values{"job_id": {accepted.JobID}, "attempt_id": {accepted.AttemptID, accepted.AttemptID}}},
		{"invalid job", url.Values{"job_id": {"../job"}, "attempt_id": {accepted.AttemptID}}},
		{"invalid attempt", url.Values{"job_id": {accepted.JobID}, "attempt_id": {"../attempt"}}},
		{"oversized body", url.Values{"job_id": {accepted.JobID}, "attempt_id": {accepted.AttemptID}, "padding": {strings.Repeat("x", 1024)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := adminResponse(handler, http.MethodPost, "/admin/stop", test.form, cookie, "https://admin.example"); got.Code != http.StatusBadRequest {
				t.Fatalf("invalid stop status=%d body=%s", got.Code, got.Body.String())
			}
			saved, err := store.ReadSaved()
			if err != nil || saved.Accepted == nil || saved.Accepted.StopRequest != (state.StopRequest{}) || ctx.Err() != nil {
				t.Fatalf("invalid stop changed selected run: state=%+v canceled=%v error=%v", saved, ctx.Err(), err)
			}
		})
	}
	if got := adminResponse(handler, http.MethodGet, "/", nil, cookie, ""); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), `name="attempt_id" value="attempt-1"`) ||
		!strings.Contains(got.Body.String(), "Force stop this attempt") {
		t.Fatalf("current action missing exact-attempt stop form: status=%d body=%s", got.Code, got.Body.String())
	}
	stale := url.Values{"job_id": {accepted.JobID}, "attempt_id": {"older-attempt"}}
	if got := adminResponse(handler, http.MethodPost, "/admin/stop", stale, cookie, "https://admin.example"); got.Code != http.StatusConflict {
		t.Fatalf("stale attempt stop status=%d", got.Code)
	}
	if got := adminResponse(handler, http.MethodPost, "/admin/stop", form, cookie, "https://other.example"); got.Code != http.StatusForbidden {
		t.Fatalf("cross-origin stop status=%d", got.Code)
	}
	if got := adminResponse(handler, http.MethodPost, "/admin/stop", form, cookie, "https://admin.example"); got.Code != http.StatusSeeOther {
		t.Fatalf("stop status=%d body=%s", got.Code, got.Body.String())
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("admin stop did not cancel selected run")
	}
	saved, err := store.ReadSaved()
	if err != nil || saved.Accepted == nil || !saved.Accepted.Terminal || saved.Accepted.StopRequest.RequestedBy != "admin" ||
		saved.Accepted.StopRequest.Status != "requested" {
		t.Fatalf("stop did not save session requester/fence: state=%+v error=%v", saved, err)
	}
	if got := adminResponse(handler, http.MethodGet, "/", nil, cookie, ""); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), "Waiting for the stop to be verified.") ||
		strings.Contains(got.Body.String(), "Force stop this attempt") {
		t.Fatalf("requested status/form incorrect: status=%d body=%s", got.Code, got.Body.String())
	}
	if err := store.ResolveStop(accepted.JobID, accepted.AttemptID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	if err := store.WithLock(func() error {
		if _, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "canceled", time.Now().UTC()); err != nil {
			return err
		}
		store.Data.Accepted = nil
		return store.Save()
	}); err != nil {
		t.Fatal(err)
	}
	unregister()
	if got := adminResponse(handler, http.MethodGet, "/", nil, cookie, ""); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), "The attempt stopped cleanly.") ||
		!strings.Contains(got.Body.String(), "admin") {
		t.Fatalf("confirmed stop disappeared after accepted cleanup: status=%d body=%s", got.Code, got.Body.String())
	}
	if got := adminResponse(handler, http.MethodPost, "/admin/stop", form, cookie, "https://admin.example"); got.Code != http.StatusSeeOther {
		t.Fatalf("completed repeat status=%d", got.Code)
	}
}

func TestAdminDashboardLiveUpdatesStopAfterLogout(t *testing.T) {
	handler, jw, store, owner := dashboardTestServer(t)
	cookie := dashboardTestCookie(t, handler)
	tr := dashboardLiveRequest(t, jw, owner, cookie, adminDashboardFilter{Repo: "owner/repo"})
	other := dashboardLiveRequest(t, jw, owner, cookie, adminDashboardFilter{Repo: "other/repo"})
	otherRequester := dashboardLiveRequest(t, jw, owner, cookie, adminDashboardFilter{Requester: "other"})
	otherAction := dashboardLiveRequest(t, jw, owner, cookie, adminDashboardFilter{Action: "review"})
	unfiltered := dashboardLiveRequest(t, jw, owner, cookie, adminDashboardFilter{})
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
	for _, request := range []*jawstest.TestRequest{other, otherRequester, otherAction} {
		dashboardReadPatch(t, request, "No current action matches these filters.", 3*time.Second, "owner/repo")
	}
	dashboardReadPatch(t, unfiltered, "owner/repo", 3*time.Second)
	if logout := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); logout.Code != http.StatusSeeOther {
		t.Fatalf("logout status=%d", logout.Code)
	}
	jw.Dirty(adminDashboardDirty)
	for _, request := range []*jawstest.TestRequest{tr, other, otherRequester, otherAction, unfiltered} {
		dashboardReadPatch(t, request, "Admin session ended", 3*time.Second, "owner/repo", "Selected worker")
	}
}

func TestAdminDashboardAllLiveRegionsRedactAfterLogout(t *testing.T) {
	handler, jw, store, owner := dashboardTestServer(t)
	now := time.Now().UTC()
	store.Data.Accepted = dashboardTestAccepted(now)
	store.Data.LastStopJobID = "prior-job"
	store.Data.TerminalDecisions["prior-job"] = state.TerminalDecision{AttemptID: "prior-attempt", Outcome: "canceled", DecidedAt: now,
		StopRequest: state.StopRequest{RequestedBy: "private-operator", RequestedAt: now, Status: "confirmed"}}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	unregister := store.RegisterSelectedStop(store.Data.Accepted.JobID, store.Data.Accepted.AttemptID, func() {})
	defer unregister()
	cookie := dashboardTestCookie(t, handler)
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
	page := &adminDashboardPage{owner: owner}
	for _, part := range []struct{ tag, name string }{
		{tag: "p", name: "dashboard-status"},
		{tag: "p", name: "dashboard-stop-status"},
		{tag: "section", name: "dashboard-live"},
		{tag: "section", name: "dashboard-action"},
		{tag: "div", name: "dashboard-stop-control"},
	} {
		if err := rw.Template(part.tag, part.name, page); err != nil {
			t.Fatal(err)
		}
	}
	if initial := tr.BodyString(); !strings.Contains(initial, "owner/repo") || !strings.Contains(initial, "Selected worker") ||
		!strings.Contains(initial, "Force stop this attempt") || !strings.Contains(initial, "private-operator") {
		t.Fatalf("missing initial private dashboard regions: %s", initial)
	}
	if logout := adminResponse(handler, http.MethodPost, "/admin/logout", nil, cookie, "https://admin.example"); logout.Code != http.StatusSeeOther {
		t.Fatalf("logout status=%d", logout.Code)
	}
	jw.Dirty(adminDashboardDirty)
	ended := 0
	for range 5 {
		select {
		case patch, ok := <-tr.OutCh:
			if !ok {
				t.Fatal("live channel closed before all revoked region patches")
			}
			for _, private := range []string{"owner/repo", "Selected worker", "Worker readiness", "Force stop this attempt", "private-operator"} {
				if strings.Contains(patch.Data, private) {
					t.Fatalf("revoked live patch exposed %q: %s", private, patch.Data)
				}
			}
			if strings.Contains(patch.Data, "Admin session ended") {
				ended++
			}
		case <-time.After(3 * time.Second):
			t.Fatal("missing revoked live region patch")
		}
	}
	if ended != 3 {
		t.Fatalf("revoked dashboard sent %d session-ended patches, want 3", ended)
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
		Administrators: []config.AdminAccount{testAdminAccount()}}, owner, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	baseURL := "http://" + admin.listener.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	login, _ := adminNetworkLogin(t, client, baseURL, url.Values{"password": {"correct-password"}})
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
		key := adminJaWSKey(t, string(pageBody))
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
	seenMetrics, seenAction := false, false
	for !seenMetrics || !seenAction {
		_, patch, err := matching.Read(wsContext)
		if err != nil {
			t.Fatalf("ticker did not patch matching WebSocket: %v", err)
		}
		body := string(patch)
		if hasMetric(body, "Tracked accepted actions", "1") {
			if !hasMetric(body, "In progress", "1") {
				t.Fatalf("ticker patch omitted updated metrics: %s", body)
			}
			seenMetrics = true
		}
		if strings.Contains(body, "Current action") {
			if !strings.Contains(body, "owner/repo") {
				t.Fatalf("ticker patch omitted current action: %s", body)
			}
			seenAction = true
		}
	}
	readPatch(other, "No current action matches these filters.", "owner/repo")
}
