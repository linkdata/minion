package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
)

func TestSetupFormSavesBotAndFirstRepository(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.local.json")
	initial := config.Config{Admin: &config.AdminConfig{
		Listen: "127.0.0.1:8377", Administrators: []config.AdminAccount{testAdminAccount()},
	}}
	data, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer secret-token" && auth != "Bearer empty-branch-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"id":8,"login":"example-bot"}`))
		case "/repos/example/repo":
			if auth == "Bearer empty-branch-token" {
				_, _ = w.Write([]byte(`{"id":42,"full_name":"example/repo","owner":{"id":7}}`))
			} else {
				_, _ = w.Write([]byte(`{"id":42,"full_name":"example/repo","owner":{"id":7},"default_branch":"main"}`))
			}
		case "/repos/example/repo/notifications":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/example/repo/collaborators/example-bot/permission":
			_, _ = w.Write([]byte(`{"permission":"write"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	previousClient := newSetupGitHubClient
	newSetupGitHubClient = func(token string) *github.Client {
		client := github.New(token)
		client.BaseURL = api.URL
		return client
	}
	t.Cleanup(func() { newSetupGitHubClient = previousClient })
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandlerWithSetupPath(jw, *initial.Admin, nil, nil, true, path)
	if err != nil {
		jw.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })
	form := url.Values{"bot_login": {"example-bot"}, "github_token": {"secret-token"},
		"repository": {"Example/Repo"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", form, nil, ""); response.Code != http.StatusForbidden {
		t.Fatalf("anonymous setup save status=%d, want 403", response.Code)
	}
	loginPage := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if loginPage.Code != http.StatusOK || len(loginPage.Result().Cookies()) != 1 {
		t.Fatalf("setup login page status=%d cookies=%v", loginPage.Code, loginPage.Result().Cookies())
	}
	if home := adminResponse(handler, http.MethodGet, "/", nil, loginPage.Result().Cookies()[0], ""); home.Code != http.StatusOK ||
		!strings.Contains(home.Body.String(), "Sign in as admin to save the bot account and first repository") {
		t.Fatalf("signed-out setup home status=%d body=%q", home.Code, home.Body.String())
	}
	login := adminResponse(handler, http.MethodPost, "/admin/login", url.Values{
		"username": {"admin"}, "password": {"correct-password"},
	}, loginPage.Result().Cookies()[0], "")
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("setup login status=%d cookies=%v", login.Code, login.Result().Cookies())
	}
	cookie := login.Result().Cookies()[0]
	if page := adminResponse(handler, http.MethodGet, "/admin/login", nil, cookie, ""); page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), "Continue to Minion") {
		t.Fatalf("signed-in setup login page status=%d body=%q", page.Code, page.Body.String())
	}
	home := adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Save setup") ||
		!strings.Contains(home.Body.String(), "Save the bot account and first repository below") ||
		!strings.Contains(home.Body.String(), "one configured command, if any") ||
		!strings.Contains(home.Body.String(), `name="repository" value="" maxlength="201"`) ||
		strings.Contains(home.Body.String(), "Sign in as admin to save") ||
		strings.Contains(home.Body.String(), `name="repository_id"`) || strings.Contains(home.Body.String(), `name="owner_id"`) ||
		strings.Contains(home.Body.String(), "secret-token") {
		t.Fatalf("setup form status=%d body=%q", home.Code, home.Body.String())
	}
	invalid := url.Values{"bot_login": {"example-bot"}, "github_token": {"bad secret token"},
		"repository": {"Example/Repo"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", invalid, cookie, ""); response.Code == http.StatusSeeOther ||
		strings.Contains(response.Body.String(), "bad secret token") {
		t.Fatalf("invalid setup save status=%d echoed token=%t", response.Code, strings.Contains(response.Body.String(), "bad secret token"))
	}
	wrongLogin := url.Values{"bot_login": {"wrong-bot"}, "github_token": {"secret-token"},
		"repository": {"Example/Repo"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", wrongLogin, cookie, ""); response.Code == http.StatusSeeOther {
		t.Fatalf("wrong bot login was saved: status=%d", response.Code)
	}
	missingRepo := url.Values{"bot_login": {"example-bot"}, "github_token": {"secret-token"},
		"repository": {"Example/Missing"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", missingRepo, cookie, ""); response.Code == http.StatusSeeOther {
		t.Fatalf("missing repository was saved: status=%d", response.Code)
	}
	emptyBranch := url.Values{"bot_login": {"example-bot"}, "github_token": {"empty-branch-token"},
		"repository": {"Example/Repo"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", emptyBranch, cookie, ""); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "default branches") {
		t.Fatalf("repository without default branch setup status=%d body=%q", response.Code, response.Body.String())
	}
	unchanged, err := config.Load(path)
	if err != nil || unchanged.BotLogin != "" || unchanged.GitHub.Token != "" || len(unchanged.Repositories) != 0 {
		t.Fatalf("invalid setup changed config: bot=%q tokenSet=%t repositories=%d error=%v",
			unchanged.BotLogin, unchanged.GitHub.Token != "", len(unchanged.Repositories), err)
	}
	saved := adminResponse(handler, http.MethodPost, "/admin/setup", form, cookie, "")
	if saved.Code != http.StatusSeeOther || saved.Header().Get("Location") != "/" {
		t.Fatalf("setup save status=%d body=%q", saved.Code, saved.Body.String())
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.BotLogin != "example-bot" || cfg.GitHub.Token != "secret-token" ||
		len(cfg.Repositories) != 1 || cfg.Repositories[0].Name != "example/repo" ||
		cfg.Repositories[0].ID != 42 || cfg.Repositories[0].OwnerID != 7 ||
		!cfg.Repositories[0].DenyAllActors || len(cfg.Repositories[0].Actions) != 1 || cfg.Repositories[0].Actions[0] != "review" {
		t.Fatalf("saved setup config is incomplete or differs from submitted values: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("config permission after setup: %v", info.Mode())
		}
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Setup saved") ||
		strings.Contains(home.Body.String(), "secret-token") || strings.Contains(home.Body.String(), "Save setup") {
		t.Fatalf("saved setup home status=%d body=%q", home.Code, home.Body.String())
	}
	_, err = config.Update(path, func(cfg *config.Config) error {
		cfg.GitHub.Token = ""
		cfg.Repositories = append(cfg.Repositories, config.Repository{
			Name: "example/second", ID: 43, OwnerID: 7, Actions: []string{"review"}, DenyAllActors: true,
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	denied := adminResponse(handler, http.MethodPost, "/admin/setup", form, cookie, "")
	if denied.Code != http.StatusConflict || !strings.Contains(denied.Body.String(), "all configured repositories") {
		t.Fatalf("inaccessible second repository setup status=%d body=%q", denied.Code, denied.Body.String())
	}
	unchanged, err = config.Load(path)
	if err != nil || unchanged.GitHub.Token != "" || len(unchanged.Repositories) != 2 {
		t.Fatalf("inaccessible second repository changed setup config: tokenSet=%t repositories=%d error=%v",
			unchanged.GitHub.Token != "", len(unchanged.Repositories), err)
	}
	for _, tc := range []struct {
		name, main      string
		commands        map[string]config.CommandMapping
		want, wantError string
	}{
		{"custom commands", "", map[string]config.CommandMapping{
			"zeta":  {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
			"alpha": {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
		}, "alpha", ""},
		{"unsupported command is skipped", "claude", map[string]config.CommandMapping{
			"audit": {Action: "security-audit", Skills: []string{"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"}},
			"zeta":  {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
		}, "zeta", ""},
		{"only unsupported command", "claude", map[string]config.CommandMapping{
			"audit": {Action: "security-audit", Skills: []string{"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"}},
		}, "", "No configured command can run"},
		{"no commands", "", map[string]config.CommandMapping{}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := config.Update(path, func(cfg *config.Config) error {
				cfg.Repositories = nil
				cfg.Main = tc.main
				if tc.main == "claude" {
					cfg.Claude.Effort = "medium"
				}
				cfg.Commands = tc.commands
				cfg.GitHub.Token = ""
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			response := adminResponse(handler, http.MethodPost, "/admin/setup", form, cookie, "")
			if tc.wantError != "" {
				if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), tc.wantError) {
					t.Fatalf("unsupported setup status=%d body=%q", response.Code, response.Body.String())
				}
				cfg, err := config.Load(path)
				if err != nil || len(cfg.Repositories) != 0 || cfg.GitHub.Token != "" {
					t.Fatalf("failed setup changed config: repositories=%v tokenSet=%t error=%v", cfg.Repositories, cfg.GitHub.Token != "", err)
				}
				return
			}
			if response.Code != http.StatusSeeOther {
				t.Fatalf("custom setup save status=%d body=%q", response.Code, response.Body.String())
			}
			cfg, err := config.Load(path)
			if err != nil || len(cfg.Repositories) != 1 {
				t.Fatalf("saved setup config: repositories=%v error=%v", cfg.Repositories, err)
			}
			if got := strings.Join(cfg.Repositories[0].Actions, ","); got != tc.want {
				t.Fatalf("saved commands=%q want=%q", got, tc.want)
			}
		})
	}
}
