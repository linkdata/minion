package main

import (
	"bytes"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
)

func TestAdminPolicyPageRendersInheritedSettingsAndEscapesText(t *testing.T) {
	live, _ := liveConfigFixture(t, false)
	view := policyView(live.snapshot())
	if view.Global != 1 || view.Custom || view.Instructions != action.DefaultAIInstructions {
		t.Fatalf("default policy = %+v", view)
	}
	view.Instructions = `<script>alert("x")</script>`
	view.Custom = true
	var body bytes.Buffer
	page := struct {
		Dot                adminPolicyView
		HeadHTML, TailHTML template.HTML
	}{Dot: view}
	if err := adminPolicyHTML.ExecuteTemplate(&body, "admin policy", page); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.String(), "&lt;script&gt;") || strings.Contains(body.String(), "<script>alert") ||
		!strings.Contains(body.String(), "/admin/policy/instructions/reset") {
		t.Fatalf("instructions page is unsafe or lacks reset: %s", body.String())
	}
}

func TestAdminPolicyRequesterPinsGitHubIdentity(t *testing.T) {
	live, path := liveConfigFixture(t, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/collaborators/writer/permission" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected GitHub policy check: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"permission":"write","user":{"login":"writer","id":77}}`))
	}))
	t.Cleanup(server.Close)
	client := github.New("fixture-token")
	client.BaseURL = server.URL
	mux := http.NewServeMux()
	registerAdminPolicyActions(mux, live, &action.Orchestrator{GitHub: client})
	post := func(route string, form url.Values) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("POST %s = %d: %s", route, w.Code, w.Body.String())
		}
	}
	post("/admin/policy/requester", url.Values{"repository": {"owner/repo"}, "login": {"writer"}, "rounds": {"4"}})
	saved := configForAdminTest(t, path)
	if len(saved.RequesterPolicies) != 1 || saved.RequesterPolicies[0].ID != 77 || saved.RequiredCleanRounds(11, 77) != 4 {
		t.Fatalf("requester policy = %+v", saved.RequesterPolicies)
	}
	post("/admin/policy/requester/remove", url.Values{"id": {"77"}})
	saved = configForAdminTest(t, path)
	if len(saved.RequesterPolicies) != 0 || saved.RequiredCleanRounds(11, 77) != 1 {
		t.Fatalf("requester override was not cleared: %+v", saved.RequesterPolicies)
	}
}

func TestAdminPolicyRoutesRequireAdminAndSaveSettings(t *testing.T) {
	live, path := liveConfigFixture(t, false)
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	owner := &action.Orchestrator{BotLogin: "bot", BotID: 9}
	handler, err := newAdminHandler(jw, config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()}}, owner, nil, live)
	if err != nil {
		jw.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })
	if denied := adminResponse(handler, http.MethodGet, "/admin/policy", nil, nil, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("anonymous policy page = %d", denied.Code)
	}
	cookie := dashboardTestCookie(t, handler)
	for _, route := range []string{"/admin/status", "/admin/users", "/admin/repositories", "/admin/skills", "/admin/policy"} {
		page := adminResponse(handler, http.MethodGet, route, nil, cookie, "")
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="jawsKey"`) || !strings.Contains(page.Body.String(), "/static/bootstrap") {
			t.Fatalf("admin page %s = %d: %s", route, page.Code, page.Body.String())
		}
	}
	if denied := adminResponse(handler, http.MethodPost, "/admin/policy/global", url.Values{"rounds": {"3"}}, cookie, "https://other.example"); denied.Code != http.StatusForbidden {
		t.Fatalf("cross-origin policy change = %d", denied.Code)
	}
	for _, step := range []struct {
		route string
		form  url.Values
	}{
		{"/admin/policy/global", url.Values{"rounds": {"3"}}},
		{"/admin/policy/repository", url.Values{"repository": {"owner/repo"}, "rounds": {"2"}}},
		{"/admin/policy/instructions", url.Values{"instructions": {"Use the configured review style."}}},
	} {
		response := adminResponse(handler, http.MethodPost, step.route, step.form, cookie, "https://admin.example")
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin/policy" {
			t.Fatalf("POST %s = %d: %s", step.route, response.Code, response.Body.String())
		}
	}
	saved := configForAdminTest(t, path)
	if saved.RequiredCleanRounds(11, 7) != 2 || saved.AIInstructions != "Use the configured review style." || !live.snapshot().Pending {
		t.Fatalf("saved policy = %+v", saved)
	}
	for _, step := range []struct {
		route string
		form  url.Values
	}{
		{"/admin/policy/repository", url.Values{"repository": {"owner/repo"}, "rounds": {""}}},
		{"/admin/policy/instructions/reset", nil},
	} {
		response := adminResponse(handler, http.MethodPost, step.route, step.form, cookie, "https://admin.example")
		if response.Code != http.StatusSeeOther {
			t.Fatalf("POST %s = %d: %s", step.route, response.Code, response.Body.String())
		}
	}
	saved = configForAdminTest(t, path)
	if saved.Repositories[0].RequiredCleanRounds != nil || saved.RequiredCleanRounds(11, 7) != 3 || saved.AIInstructions != "" {
		t.Fatalf("reset policy = %+v", saved)
	}
}
