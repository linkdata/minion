package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/skills"
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
		case "/repos/owner/new/collaborators/alice/permission":
			_, _ = w.Write([]byte(`{"permission":"write","user":{"id":77,"login":"alice"}}`))
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
	if !repo.DenyAllActors || len(repo.AllowedActors) != 0 || repo.AllowsActor("another", 88) {
		t.Fatalf("last requester removal did not preserve explicit deny-all: %+v", repo)
	}
	post("/admin/users/allow-any", url.Values{"repository": {"owner/existing"}}, http.StatusSeeOther)
	repo, _ = configForAdminTest(t, path).Repository("owner/existing")
	if repo.DenyAllActors || len(repo.AllowedActors) != 0 || !repo.AllowsActor("another", 88) {
		t.Fatalf("allow-any did not restore omitted allowlist: %+v", repo)
	}
	post("/admin/repositories/add", url.Values{"name": {"owner/new"}, "actions": {"review"}}, http.StatusSeeOther)
	repo, ok := live.snapshot().Config.Repository("owner/new")
	if !ok || repo.ID != 11 || repo.OwnerID != 2 || !repo.DenyAllActors || len(repo.AllowedActors) != 0 || repo.AllowsActor("another", 88) || len(repo.Actions) != 1 || repo.Actions[0] != "review" {
		t.Fatalf("repository not pinned: %+v, found=%t", repo, ok)
	}
	post("/admin/users/add", url.Values{"repository": {"owner/new"}, "login": {"alice"}}, http.StatusSeeOther)
	repo, _ = live.snapshot().Config.Repository("owner/new")
	if repo.DenyAllActors || !repo.AllowsActor("alice", 77) {
		t.Fatalf("adding requester did not clear deny-all: %+v", repo)
	}
	post("/admin/users/remove", url.Values{"repository": {"owner/new"}, "login": {"alice"}}, http.StatusSeeOther)
	post("/admin/repositories/actions", url.Values{"name": {"owner/new"}, "actions": {"comment"}}, http.StatusSeeOther)
	repo, _ = live.snapshot().Config.Repository("owner/new")
	if len(repo.Actions) != 1 || repo.Actions[0] != "comment" || !repo.DenyAllActors {
		t.Fatalf("edited repository commands or access changed: %+v", repo)
	}
	owner.GitHub = nil
	post("/admin/users/deny-all", url.Values{"repository": {"owner/existing"}}, http.StatusSeeOther)
	repo, _ = configForAdminTest(t, path).Repository("owner/existing")
	if !repo.DenyAllActors || len(repo.AllowedActors) != 0 || len(repo.AllowedActorIDs) != 0 || repo.AllowsActor("another", 88) {
		t.Fatalf("offline deny-all did not revoke any-writer access: %+v", repo)
	}
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

func TestAdminRequesterAddRejectsChangedRepositoryIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, details string
	}{
		{"repository ID", `{"id":12,"full_name":"owner/repo","owner":{"id":22}}`},
		{"owner ID", `{"id":11,"full_name":"owner/repo","owner":{"id":23}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, path := liveConfigFixture(t, false)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/owner/repo/collaborators/alice/permission":
					_, _ = w.Write([]byte(`{"permission":"write","user":{"id":77,"login":"alice"}}`))
				case "/repos/owner/repo":
					_, _ = w.Write([]byte(tc.details))
				default:
					t.Errorf("unexpected GitHub path %q", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			client := github.New("fixture-token")
			client.BaseURL = server.URL
			mux := http.NewServeMux()
			registerAdminConfigActions(mux, live, &action.Orchestrator{GitHub: client})
			form := url.Values{"repository": {"owner/repo"}, "login": {"alice"}}
			req := httptest.NewRequest(http.MethodPost, "/admin/users/add", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, req)
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "identity") {
				t.Fatalf("changed repository identity: status=%d body=%q", response.Code, response.Body.String())
			}
			repo, _ := configForAdminTest(t, path).Repository("owner/repo")
			if len(repo.AllowedActors) != 0 || len(repo.AllowedActorIDs) != 0 {
				t.Fatalf("requester saved for wrong repository: %+v", repo)
			}
		})
	}
}

func TestAdminRepositoryCommandsRejectUnsupportedMainAI(t *testing.T) {
	cfg := config.Config{BotLogin: "bot", Main: "claude", Claude: config.ClaudeConfig{Effort: "high"}, AlwaysOnSkills: []string{}, Commands: map[string]config.CommandMapping{
		"review":         {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
		"security-audit": {Action: "security-audit", Skills: []string{"https://github.com/owner/skills/tree/main/skills/security-audit"}},
	}, Repositories: []config.Repository{
		{Name: "owner/legacy", ID: 1, OwnerID: 2, Actions: []string{"security-audit", "review"}},
		{Name: "owner/new", ID: 3, OwnerID: 2, Actions: []string{"review"}},
	}}
	view := configurationView(adminConfigSnapshot{Config: cfg})
	if len(view.Commands) != 1 || view.Commands[0] != "review" || len(view.AllCommands) != 2 || view.AllCommands[1] != "security-audit" {
		t.Fatalf("unsupported command offered in repository form: %v", view.Commands)
	}
	if err := validateAdminActions(cfg, []string{"security-audit"}, nil); err == nil {
		t.Fatal("unsupported command accepted by repository action handler")
	}
	if err := validateAdminActions(cfg, []string{"security-audit", "review"}, cfg.Repositories[0].Actions); err != nil {
		t.Fatalf("existing unsupported command could not be kept: %v", err)
	}
	var body strings.Builder
	page := struct {
		Dot                adminConfigurationView
		HeadHTML, TailHTML template.HTML
	}{Dot: view}
	if err := adminConfigurationHTML.ExecuteTemplate(&body, "admin repositories", page); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.String(), `value="security-audit" checked`) || !strings.Contains(body.String(), `value="security-audit" disabled`) || !strings.Contains(body.String(), "unavailable with the current main AI") {
		t.Fatalf("existing unsupported command is hidden in editor: %s", body.String())
	}
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	live, err := newAdminLiveConfig(path, loaded)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerAdminConfigActions(mux, live, nil)
	post := func(name string, want int) {
		t.Helper()
		form := url.Values{"name": {name}, "actions": {"review", "security-audit"}}
		r := httptest.NewRequest(http.MethodPost, "/admin/repositories/actions", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("POST %s: status=%d body=%q", name, w.Code, w.Body.String())
		}
	}
	post("owner/legacy", http.StatusSeeOther)
	if got, _ := configForAdminTest(t, path).Repository("owner/legacy"); len(got.Actions) != 2 || got.Actions[0] != "review" || got.Actions[1] != "security-audit" {
		t.Fatalf("existing unsupported command was removed: %+v", got)
	}
	post("owner/new", http.StatusConflict)
}

func TestAdminRepositoryCommandsRequirePublishingHostAndRemoteWorkerCapability(t *testing.T) {
	cfg := config.Config{Main: "codex", RemoteExecutor: &config.RemoteExecutorConfig{},
		Commands: map[string]config.CommandMapping{"audit": {Action: "security-audit", Skills: []string{"builtin://anti-prompt-injection"}}}}
	selected := map[string]bool{"audit": true}
	view := configurationView(adminConfigSnapshot{Config: cfg, Remote: true, RemoteCommands: selected})
	if runtime.GOOS == "windows" {
		if len(view.Commands) != 0 || validateAdminActions(cfg, []string{"audit"}, nil) == nil {
			t.Fatalf("Windows publishing host offered audit: %v", view.Commands)
		}
		return
	}
	if len(view.Commands) != 1 || view.Commands[0] != "audit" {
		t.Fatalf("supported remote audit not offered: %v", view.Commands)
	}
	if err := validateAdminActions(cfg, []string{"audit"}, nil); err != nil {
		t.Fatalf("supported remote audit rejected: %v", err)
	}
}

func TestAdminRemovesConfiguredLegacyRequester(t *testing.T) {
	live, path := liveConfigFixture(t, false)
	if err := live.save(func(cfg *config.Config) error {
		cfg.Repositories[0].AllowedActors = []string{"old__user"}
		cfg.Repositories[0].AllowedActorIDs = map[string]int64{"old__user": 77}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerAdminConfigActions(mux, live, nil)
	form := url.Values{"repository": {"owner/repo"}, "login": {"old__user"}}
	add := httptest.NewRequest(http.MethodPost, "/admin/users/add", strings.NewReader(form.Encode()))
	add.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	addResponse := httptest.NewRecorder()
	mux.ServeHTTP(addResponse, add)
	if addResponse.Code != http.StatusConflict || !strings.Contains(addResponse.Body.String(), "valid GitHub login") {
		t.Fatalf("legacy login was accepted for a new requester: status=%d body=%q", addResponse.Code, addResponse.Body.String())
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/users/remove", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	repo, _ := configForAdminTest(t, path).Repository("owner/repo")
	if w.Code != http.StatusSeeOther || !repo.DenyAllActors || len(repo.AllowedActors) != 0 {
		t.Fatalf("legacy requester removal: status=%d repo=%+v body=%q", w.Code, repo, w.Body.String())
	}
}

func TestAdminRepositoryActionExpansionRequiresRemoteRestart(t *testing.T) {
	live, _ := liveConfigFixture(t, true)
	mux := http.NewServeMux()
	registerAdminConfigActions(mux, live, adminPreflightOwner(t, false))
	form := url.Values{"name": {"owner/repo"}, "actions": {"review", "audit"}}
	r := httptest.NewRequest(http.MethodPost, "/admin/repositories/actions", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "restarting the remote worker") {
		t.Fatalf("remote action expansion: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestAdminRepositoryActionReenablePreflightsRemote(t *testing.T) {
	_, path := liveConfigFixture(t, true)
	cfg, err := config.Update(path, func(cfg *config.Config) error {
		cfg.Repositories[0].Actions = []string{"review", "audit"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.save(func(cfg *config.Config) error {
		cfg.Repositories[0].Actions = []string{"review"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !live.remoteSelected["audit"] {
		t.Fatal("test setup did not freeze audit in the remote selection")
	}
	mux := http.NewServeMux()
	registerAdminConfigActions(mux, live, adminPreflightOwner(t, true))
	form := url.Values{"name": {"owner/repo"}, "actions": {"review", "audit"}}
	r := httptest.NewRequest(http.MethodPost, "/admin/repositories/actions", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Repository is not ready") || !strings.Contains(w.Body.String(), "archived") {
		t.Fatalf("remote reenable skipped repository preflight: status=%d body=%q", w.Code, w.Body.String())
	}
	saved, _ := configForAdminTest(t, path).Repository("owner/repo")
	if len(saved.Actions) != 1 || saved.Actions[0] != "review" {
		t.Fatalf("failed preflight changed actions: %v", saved.Actions)
	}
}

func TestAdminRepositoryActionsRejectStalePreflight(t *testing.T) {
	_, path := liveConfigFixture(t, false)
	cfg, err := config.Update(path, func(cfg *config.Config) error {
		cfg.Commands = map[string]config.CommandMapping{
			"review":  {Action: "review", Skills: []string{skills.BuiltinAntiPromptInjection}},
			"audit":   {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}},
			"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}},
		}
		cfg.Repositories[0].Actions = []string{"review", "audit"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := newAdminLiveConfig(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner := adminPreflightOwner(t, false, func() {
		if err := live.save(func(cfg *config.Config) error {
			cfg.Repositories[0].Actions = []string{"review"}
			return nil
		}); err != nil {
			t.Errorf("concurrent command removal: %v", err)
		}
	})
	mux := http.NewServeMux()
	registerAdminConfigActions(mux, live, owner)
	form := url.Values{"name": {"owner/repo"}, "actions": {"review", "audit", "comment"}}
	r := httptest.NewRequest(http.MethodPost, "/admin/repositories/actions", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Repository commands changed; reload and try again") {
		t.Fatalf("stale repository edit: status=%d body=%q", w.Code, w.Body.String())
	}
	saved, _ := configForAdminTest(t, path).Repository("owner/repo")
	if len(saved.Actions) != 1 || saved.Actions[0] != "review" {
		t.Fatalf("stale edit restored removed command: %v", saved.Actions)
	}
}

func TestAdminRepositoryActionsRejectChangedIdentityDuringPreflight(t *testing.T) {
	for _, test := range []struct {
		name    string
		id      int64
		ownerID int64
	}{
		{name: "repository ID", id: 12, ownerID: 22},
		{name: "owner ID", id: 11, ownerID: 23},
	} {
		t.Run(test.name, func(t *testing.T) {
			live, path := liveConfigFixture(t, false)
			owner := adminPreflightOwner(t, false, func() {
				if err := live.save(func(cfg *config.Config) error {
					cfg.Repositories[0].ID, cfg.Repositories[0].OwnerID = test.id, test.ownerID
					return nil
				}); err != nil {
					t.Errorf("concurrent repository replacement: %v", err)
				}
			})
			mux := http.NewServeMux()
			registerAdminConfigActions(mux, live, owner)
			form := url.Values{"name": {"owner/repo"}, "actions": {"review", "audit"}}
			r := httptest.NewRequest(http.MethodPost, "/admin/repositories/actions", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Repository identity changed; reload and try again") {
				t.Fatalf("stale repository identity: status=%d body=%q", w.Code, w.Body.String())
			}
			saved, _ := configForAdminTest(t, path).Repository("owner/repo")
			if !saved.MatchesIdentity(test.id, test.ownerID) || len(saved.Actions) != 1 || saved.Actions[0] != "review" {
				t.Fatalf("stale edit changed replacement repository: %+v", saved)
			}
		})
	}
}

func adminPreflightOwner(t *testing.T, archived bool, onNotifications ...func()) *action.Orchestrator {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("GitHub authorization was absent")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/owner/repo/notifications":
			if len(onNotifications) != 0 {
				onNotifications[0]()
			}
			_, _ = w.Write([]byte(`[]`))
		case "/repos/owner/repo":
			if archived {
				_, _ = w.Write([]byte(`{"id":11,"full_name":"owner/repo","owner":{"id":22},"default_branch":"main","archived":true}`))
			} else {
				_, _ = w.Write([]byte(`{"id":11,"full_name":"owner/repo","owner":{"id":22},"default_branch":"main"}`))
			}
		case "/repos/owner/repo/collaborators/bot/permission":
			_, _ = w.Write([]byte(`{"permission":"write","user":{"id":9,"login":"bot"}}`))
		default:
			t.Errorf("unexpected GitHub preflight path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New("fixture-token")
	client.BaseURL = server.URL
	return &action.Orchestrator{GitHub: client, BotLogin: "bot", BotID: 9}
}

func TestAdminSkillTrailingSlashDoesNotReinstallActiveSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	const source = "https://github.com/owner/project/tree/main/skills/other"
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"bot_login":"bot","commands":{"review":{"action":"review","skills":["builtin://anti-prompt-injection"]}},"always_on_skills":["` + source + `"],"repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]}`
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
	mux := http.NewServeMux()
	registerAdminConfigActions(mux, live, nil)
	post := func(route, target string, want int, message string) {
		t.Helper()
		form := url.Values{"target": {target}, "url": {source + "/"}}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(form.Encode())).WithContext(ctx)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != want || !strings.Contains(w.Body.String(), message) {
			t.Fatalf("POST %s: status=%d body=%q", route, w.Code, w.Body.String())
		}
	}
	post("/admin/skills/add", "always", http.StatusConflict, "already configured for this target")
	post("/admin/skills/add", "command:review", http.StatusConflict, "already configured but missing")
	if got := configForAdminTest(t, path).AlwaysOnSkills; len(got) != 1 || got[0] != source {
		t.Fatalf("active source changed after rejected add: %v", got)
	}
	post("/admin/skills/remove", "always", http.StatusSeeOther, "")
	if got := configForAdminTest(t, path).AlwaysOnSkills; len(got) != 0 {
		t.Fatalf("slash-terminated source was not removed: %v", got)
	}
}

func TestAdminSkillPromotionInstallsOnlyMissingCriticCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	const source = "https://github.com/owner/project/tree/main/skills/shared"
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	codexDir := filepath.Join(home, ".agents", "skills", "shared")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldSkill := []byte("Active Codex instructions.\n")
	oldMetadata := []byte(`{"url":"` + source + `","commit":"` + sha + `"}`)
	for name, content := range map[string][]byte{"SKILL.md": oldSkill, ".minion-install.json": oldMetadata} {
		if err := os.WriteFile(filepath.Join(codexDir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"bot_login":"bot","adversarial":"claude","claude":{"effort":"high"},"commands":{"review":{"action":"review","skills":["builtin://anti-prompt-injection","` + source + `"]}},"repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]}`
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
	if _, installed, err := skills.InstalledPathFor(source, "codex"); err != nil || !installed {
		t.Fatalf("active Codex skill is unavailable: %t, %v", installed, err)
	}
	if _, installed, err := skills.InstalledPathFor(source, "claude"); err != nil || installed {
		t.Fatalf("Claude skill should be missing: %t, %v", installed, err)
	}
	const claudeSkill = "New Claude instructions.\n"
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "archive/skills/shared/SKILL.md", Mode: 0o600, Size: int64(len(claudeSkill))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte(claudeSkill)); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	previousTransport := http.DefaultTransport
	var requests []string
	http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "api.github.com" {
			t.Errorf("unexpected skill host %q", request.URL.Host)
		}
		requests = append(requests, request.URL.Path)
		recorder := httptest.NewRecorder()
		switch request.URL.Path {
		case "/repos/owner/project/commits/main":
			_, _ = recorder.WriteString(`{"sha":"` + sha + `"}`)
		case "/repos/owner/project/tarball/" + sha:
			_, _ = recorder.Write(archive.Bytes())
		default:
			t.Errorf("unexpected skill download %q", request.URL.Path)
			recorder.WriteHeader(http.StatusNotFound)
		}
		return recorder.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	mux := http.NewServeMux()
	registerAdminConfigActions(mux, live, nil)
	form := url.Values{"target": {"always"}, "url": {source}}
	request := httptest.NewRequest(http.MethodPost, "/admin/skills/add", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("promote skill: status=%d body=%q", response.Code, response.Body.String())
	}
	if len(requests) != 2 || requests[0] != "/repos/owner/project/commits/main" || requests[1] != "/repos/owner/project/tarball/"+sha {
		t.Fatalf("skill downloads = %v", requests)
	}
	for name, want := range map[string][]byte{"SKILL.md": oldSkill, ".minion-install.json": oldMetadata} {
		got, err := os.ReadFile(filepath.Join(codexDir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("active Codex %s changed: %q, %v", name, got, err)
		}
	}
	claudePath, installed, err := skills.InstalledPathFor(source, "claude")
	if err != nil || !installed {
		t.Fatalf("Claude skill was not installed: %q, %t, %v", claudePath, installed, err)
	}
	if got, err := os.ReadFile(claudePath); err != nil || string(got) != claudeSkill {
		t.Fatalf("Claude skill content = %q, %v", got, err)
	}
	if got := configForAdminTest(t, path).AlwaysOnSkills; len(got) == 0 || got[len(got)-1] != source {
		t.Fatalf("skill promotion was not saved: %v", got)
	}
}

func TestAdminSkillAddWaitsForActiveSameNameSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	const oldSource = "https://github.com/old/project/tree/main/skills/shared"
	const newSource = "https://github.com/new/project/tree/main/skills/shared"
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"bot_login":"bot","adversarial":"claude","claude":{"effort":"high"},"commands":{"review":{"action":"review","skills":["builtin://anti-prompt-injection"]}},"always_on_skills":["` + oldSource + `"],"repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]}`
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
	installed := filepath.Join(home, ".claude", "skills", "shared")
	if err := os.MkdirAll(installed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "SKILL.md"), []byte("---\nname: shared\n---\nOld instructions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := `{"url":"` + oldSource + `","commit":"` + strings.Repeat("a", 40) + `"}`
	if err := os.WriteFile(filepath.Join(installed, ".minion-install.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, present, err := skills.InstalledPathFor(oldSource, "claude"); err != nil || !present {
		t.Fatalf("old critic skill is not installed: %t, %v", present, err)
	}
	if _, present, err := skills.InstalledPathFor(newSource, "codex"); err != nil || present {
		t.Fatalf("new main skill should be missing: %t, %v", present, err)
	}
	if err := live.save(func(cfg *config.Config) error { cfg.AlwaysOnSkills = []string{}; return nil }); err != nil {
		t.Fatal(err)
	}
	if !live.snapshot().Pending || len(live.current().AlwaysOnSkills) != 1 || len(live.snapshot().Config.AlwaysOnSkills) != 0 {
		t.Fatal("old skill must remain active until the monitor applies its removal")
	}
	mux := http.NewServeMux()
	registerAdminConfigActions(mux, live, nil)
	form := url.Values{"target": {"command:review"}, "url": {newSource}}
	post := func() *httptest.ResponseRecorder {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		r := httptest.NewRequest(http.MethodPost, "/admin/skills/add", strings.NewReader(form.Encode())).WithContext(ctx)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	w := post()
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "still active from another source") {
		t.Fatalf("pending same-name replacement: status=%d body=%q", w.Code, w.Body.String())
	}
	if body, err := os.ReadFile(filepath.Join(installed, "SKILL.md")); err != nil || !strings.Contains(string(body), "Old instructions.") {
		t.Fatalf("active critic skill was replaced: %q, %v", body, err)
	}
	if got := configForAdminTest(t, path).Commands["review"].Skills; len(got) != 1 || got[0] != "builtin://anti-prompt-injection" {
		t.Fatalf("rejected replacement was saved: %v", got)
	}
	codexInstall := filepath.Join(home, ".agents", "skills", "shared")
	if err := os.MkdirAll(codexInstall, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexInstall, "SKILL.md"), []byte("---\nname: shared\n---\nOld instructions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexInstall, ".minion-install.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := skills.InstalledPathFor(newSource, "codex"); !errors.Is(err, skills.ErrSourceMismatch) {
		t.Fatalf("old installed source should be a safe mismatch: %v", err)
	}
	next, generation := live.next()
	live.applied(next, generation, nil)
	if len(live.current().AlwaysOnSkills) != 0 {
		t.Fatal("old skill is still active after the monitor boundary")
	}
	w = post()
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Install skill:") {
		t.Fatalf("safe replacement did not reach installer: status=%d body=%q", w.Code, w.Body.String())
	}
	if err := os.WriteFile(filepath.Join(codexInstall, ".minion-install.json"), []byte("not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	w = post()
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Check installed skill:") || strings.Contains(w.Body.String(), "Install skill:") {
		t.Fatalf("invalid metadata reached installer: status=%d body=%q", w.Code, w.Body.String())
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
