package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
)

func TestAdminConfigActionsUpdateRequestersRepositoriesAndSkills(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"bot_login":"fixture-bot","commands":{"review":{"action":"review","skills":["builtin://anti-prompt-injection"]},"comment":{"action":"comment","skills":["https://github.com/owner/project/tree/main/skills/caveman"]}},"always_on_skills":["builtin://anti-prompt-injection","https://github.com/owner/project/tree/main/skills/caveman"],"repositories":[{"name":"owner/existing","id":1,"owner_id":2,"actions":["review"],"required_clean_rounds":2}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("GitHub authorization was absent")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/owner/existing/collaborators/alice/permission":
			_, _ = w.Write([]byte(`{"permission":"write","user":{"id":77,"login":"alice"}}`))
		case "/repos/owner/existing":
			_, _ = w.Write([]byte(`{"id":1,"full_name":"owner/existing","owner":{"id":2},"default_branch":"main"}`))
		case "/repos/owner/existing/notifications":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/owner/existing/collaborators/fixture-bot/permission":
			_, _ = w.Write([]byte(`{"permission":"write","user":{"id":9,"login":"fixture-bot"}}`))
		case "/repos/owner/new":
			_, _ = w.Write([]byte(`{"id":11,"full_name":"owner/new","owner":{"id":2},"default_branch":"main"}`))
		case "/repos/owner/new/notifications":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/owner/new/collaborators/fixture-bot/permission":
			_, _ = w.Write([]byte(`{"permission":"write","user":{"id":9,"login":"fixture-bot"}}`))
		default:
			t.Errorf("unexpected GitHub path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New("fixture-token")
	client.BaseURL = server.URL
	owner := &action.Orchestrator{GitHub: client, BotLogin: "fixture-bot", BotID: 9}
	mux := http.NewServeMux()
	registerAdminConfigActions(mux, live, owner)
	post := func(route string, form url.Values, want int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("POST %s: status %d, want %d: %s", route, response.Code, want, response.Body.String())
		}
		if want == http.StatusSeeOther && response.Header().Get("Location") == "" {
			t.Fatalf("POST %s did not redirect", route)
		}
	}
	post("/admin/users/add", url.Values{"repository": {"owner/existing"}, "login": {"alice"}}, http.StatusSeeOther)
	repo, _ := live.snapshot().Config.Repository("owner/existing")
	if len(repo.AllowedActors) != 1 || repo.AllowedActorIDs["alice"] != 77 {
		t.Fatalf("requester not pinned: %+v", repo)
	}
	post("/admin/repositories/actions", url.Values{"name": {"owner/existing"}, "actions": {"review", "comment"}}, http.StatusSeeOther)
	repo, _ = live.snapshot().Config.Repository("owner/existing")
	if len(repo.Actions) != 2 || len(repo.AllowedActors) != 1 || repo.AllowedActorIDs["alice"] != 77 || repo.RequiredCleanRounds == nil || *repo.RequiredCleanRounds != 2 {
		t.Fatalf("editing commands lost repository settings: %+v", repo)
	}
	post("/admin/users/remove", url.Values{"repository": {"owner/existing"}, "login": {"alice"}}, http.StatusSeeOther)
	repo, _ = configForAdminTest(t, path).Repository("owner/existing")
	if repo.AllowedActors == nil || len(repo.AllowedActors) != 0 {
		t.Fatalf("last requester removal did not preserve explicit deny-all: %+v", repo)
	}
	post("/admin/users/allow-any", url.Values{"repository": {"owner/existing"}}, http.StatusSeeOther)
	repo, _ = configForAdminTest(t, path).Repository("owner/existing")
	if repo.AllowedActors != nil {
		t.Fatalf("allow-any did not restore omitted allowlist: %+v", repo)
	}
	post("/admin/repositories/add", url.Values{"name": {"owner/new"}, "actions": {"review"}}, http.StatusSeeOther)
	repo, ok := live.snapshot().Config.Repository("owner/new")
	if !ok || repo.ID != 11 || repo.OwnerID != 2 || repo.AllowedActors == nil || len(repo.AllowedActors) != 0 || len(repo.Actions) != 1 || repo.Actions[0] != "review" {
		t.Fatalf("repository not pinned: %+v, found=%t", repo, ok)
	}
	post("/admin/repositories/actions", url.Values{"name": {"owner/new"}, "actions": {"comment"}}, http.StatusSeeOther)
	repo, _ = live.snapshot().Config.Repository("owner/new")
	if len(repo.Actions) != 1 || repo.Actions[0] != "comment" || repo.AllowedActors == nil {
		t.Fatalf("edited repository commands or access changed: %+v", repo)
	}
	owner.GitHub = nil
	post("/admin/repositories/actions", url.Values{"name": {"owner/existing"}, "actions": {"review"}}, http.StatusSeeOther)
	repo, _ = live.snapshot().Config.Repository("owner/existing")
	if len(repo.Actions) != 1 || repo.Actions[0] != "review" {
		t.Fatalf("narrowing commands offline failed: %+v", repo)
	}
	post("/admin/repositories/remove", url.Values{"name": {"owner/new"}}, http.StatusSeeOther)
	if len(live.snapshot().Config.Repositories) != 1 {
		t.Fatal("removed repository remains configured")
	}
	post("/admin/repositories/remove", url.Values{"name": {"owner/existing"}}, http.StatusConflict)
	post("/admin/skills/remove", url.Values{"target": {"always"}, "url": {"https://github.com/owner/project/tree/main/skills/caveman"}}, http.StatusSeeOther)
	if got := live.snapshot().Config.AlwaysOnSkills; len(got) != 1 || got[0] != "builtin://anti-prompt-injection" {
		t.Fatalf("skill association was not removed: %v", got)
	}
	post("/admin/skills/remove", url.Values{"target": {"command:review"}, "url": {"builtin://anti-prompt-injection"}}, http.StatusConflict)
	post("/admin/skills/add", url.Values{"target": {"command:comment"}, "url": {"builtin://anti-prompt-injection"}}, http.StatusSeeOther)
	if got := live.snapshot().Config.Commands["comment"].Skills; len(got) != 2 || got[1] != "builtin://anti-prompt-injection" {
		t.Fatalf("already installed skill was not linked: %v", got)
	}
	post("/admin/users/add", url.Values{"repository": {"owner/existing"}, "login": {"alice", "bob"}}, http.StatusBadRequest)
	post("/admin/repositories/actions", url.Values{"name": {"owner/existing"}, "actions": {"review", "review"}}, http.StatusConflict)
	post("/admin/repositories/actions", url.Values{"name": {"owner/existing"}}, http.StatusConflict)
}

func TestAdminRepositoryCommandsRejectUnsupportedMainAI(t *testing.T) {
	cfg := config.Config{Main: "claude", Commands: map[string]config.CommandMapping{
		"review":         {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
		"security-audit": {Action: "security-audit", Skills: []string{"builtin://anti-prompt-injection"}},
	}}
	view := configurationView(adminConfigSnapshot{Config: cfg})
	if len(view.Commands) != 1 || view.Commands[0] != "review" {
		t.Fatalf("unsupported command offered in repository form: %v", view.Commands)
	}
	if err := validateAdminActions(cfg, []string{"security-audit"}); err == nil {
		t.Fatal("unsupported command accepted by repository action handler")
	}
}

func TestAdminRepositoryActionExpansionRequiresRemoteRestart(t *testing.T) {
	live, _ := liveConfigFixture(t, true)
	mux := http.NewServeMux()
	registerAdminConfigActions(mux, live, nil)
	form := url.Values{"name": {"owner/repo"}, "actions": {"review", "fix"}}
	r := httptest.NewRequest(http.MethodPost, "/admin/repositories/actions", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "restarting the remote worker") {
		t.Fatalf("remote action expansion: status=%d body=%q", w.Code, w.Body.String())
	}
}

func configForAdminTest(t *testing.T, path string) config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
