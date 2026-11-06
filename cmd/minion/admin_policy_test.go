package main

import (
	"bytes"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
	cfg := live.snapshot().Config
	rounds := 3
	cfg.RequesterPolicies = []config.RequesterPolicy{
		{Login: "inherited", ID: 7},
		{Login: "custom", ID: 8, RequiredCleanRounds: &rounds},
	}
	view = policyView(adminConfigSnapshot{Config: cfg})
	if len(view.Requesters) != 1 || view.Requesters[0].Name != "custom" || view.Requesters[0].Value != rounds {
		t.Fatalf("requester overrides = %+v", view.Requesters)
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
		strings.Contains(body.String(), "@inherited") || !strings.Contains(body.String(), "@custom") ||
		!strings.Contains(body.String(), `aria-label="Use global default for @custom"`) ||
		!strings.Contains(body.String(), `aria-label="Save required reviews for owner/repo"`) ||
		!strings.Contains(body.String(), "/admin/policy/instructions/reset") ||
		!strings.Contains(body.String(), "16,384 UTF-8 bytes") {
		t.Fatalf("instructions page is unsafe or lacks reset/limit guidance: %s", body.String())
	}
}

func TestAdminPolicyNoCriticDefaultSurvivesSave(t *testing.T) {
	_, path := liveConfigFixture(t, false)
	cfg, err := config.Update(path, func(cfg *config.Config) error {
		cfg.Adversarial = ""
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	view := policyView(live.snapshot())
	if view.Global != 1 || view.Critic {
		t.Fatalf("no-critic default = %+v", view)
	}
	mux := http.NewServeMux()
	registerAdminPolicyActions(mux, live, nil)
	form := url.Values{"rounds": {strconv.Itoa(view.Global)}}
	request := httptest.NewRequest(http.MethodPost, "/admin/policy/global", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("save displayed default: status=%d body=%q", response.Code, response.Body.String())
	}
	saved := configForAdminTest(t, path)
	if saved.CriticStopping.RequiredCleanRounds == nil || *saved.CriticStopping.RequiredCleanRounds != 1 {
		t.Fatalf("saved default = %+v", saved.CriticStopping.RequiredCleanRounds)
	}
	saved.Adversarial = "codex"
	if got := saved.RequiredCleanRounds(11, 7); got != 1 {
		t.Fatalf("default after enabling critic = %d, want 1", got)
	}
	zero := 0
	saved.CriticStopping.RequiredCleanRounds = &zero
	if got := policyView(adminConfigSnapshot{Config: saved}).Global; got != 0 {
		t.Fatalf("explicit zero displayed as %d", got)
	}
}

func TestAdminPolicyRequesterPinsGitHubIdentity(t *testing.T) {
	live, path := liveConfigFixture(t, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected GitHub policy check: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/repos/owner/repo":
			_, _ = w.Write([]byte(`{"id":11,"full_name":"owner/repo","owner":{"id":22}}`))
		case "/repos/owner/repo/collaborators/writer/permission":
			_, _ = w.Write([]byte(`{"permission":"write","user":{"login":"writer","id":77}}`))
		default:
			t.Errorf("unexpected GitHub policy check: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
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

func TestAdminRequesterRoutesRequireVerifiedWriteAccess(t *testing.T) {
	for _, route := range []string{"/admin/users/add", "/admin/policy/requester"} {
		for _, tc := range []struct {
			name, repository, login, permission, identity, want string
		}{
			{name: "invalid login", repository: "owner/repo", login: "old_user", want: "valid GitHub login"},
			{name: "unconfigured repository", repository: "owner/other", login: "writer", want: "Repository is not configured"},
			{name: "wrong identity", repository: "owner/repo", login: "writer", permission: "write", identity: "different", want: "Requester must have write"},
			{name: "read only", repository: "owner/repo", login: "writer", permission: "read", identity: "writer", want: "Requester must have write"},
			{name: "missing ID", repository: "owner/repo", login: "writer", permission: "write", identity: "writer", want: "Requester must have write"},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				live, path := liveConfigFixture(t, false)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/repos/owner/repo/collaborators/writer/permission" {
						t.Errorf("unexpected GitHub requester check: %s", r.URL.Path)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					id := 77
					if tc.name == "missing ID" {
						id = 0
					}
					_, _ = w.Write([]byte(`{"permission":"` + tc.permission + `","user":{"login":"` + tc.identity + `","id":` + strconv.Itoa(id) + `}}`))
				}))
				t.Cleanup(server.Close)
				client := github.New("fixture-token")
				client.BaseURL = server.URL
				mux := http.NewServeMux()
				owner := &action.Orchestrator{GitHub: client}
				registerAdminConfigActions(mux, live, owner)
				registerAdminPolicyActions(mux, live, owner)
				form := url.Values{"repository": {tc.repository}, "login": {tc.login}}
				if route == "/admin/policy/requester" {
					form.Set("rounds", "2")
				}
				r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(form.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, r)
				if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), tc.want) {
					t.Fatalf("POST %s: status=%d body=%q", route, response.Code, response.Body.String())
				}
				saved := configForAdminTest(t, path)
				if len(saved.Repositories[0].AllowedActors) != 0 || len(saved.RequesterPolicies) != 0 {
					t.Fatalf("rejected requester was saved: repo=%+v policies=%+v", saved.Repositories[0], saved.RequesterPolicies)
				}
			})
		}
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
	response := adminResponse(handler, http.MethodPost, "/admin/policy/instructions", url.Values{"instructions": {action.DefaultAIInstructions}}, cookie, "https://admin.example")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("save unchanged built-in instructions = %d: %s", response.Code, response.Body.String())
	}
	if saved := configForAdminTest(t, path); saved.AIInstructions != "" || policyView(live.snapshot()).Custom {
		t.Fatalf("built-in instructions became a custom override: %+v", saved)
	}
}
