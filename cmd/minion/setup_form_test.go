package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/config"
)

func setupFormVersion(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `name="version" value="`)
	if !ok {
		t.Fatal("setup form has no config version")
	}
	version, _, ok := strings.Cut(rest, `"`)
	if !ok || len(version) != 64 {
		t.Fatalf("invalid setup version %q", version)
	}
	return version
}

func TestLoadSetupConfigRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxAdvancedConfig + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadSetupConfig(path, config.AdminConfig{}); err == nil || !strings.Contains(err.Error(), "editor limit") {
		t.Fatalf("oversized setup config: %v", err)
	}
}

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
	previousTransport := http.DefaultTransport
	http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
		t.Errorf("setup made an external request: %s", request.URL)
		return nil, errors.New("unexpected setup request")
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
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
	form := url.Values{"bot_login": {"mona-cat_octo"}, "github_token": {"secret-token"},
		"repository": {"Example/Repo"}, "repository_id": {"42"}, "owner_id": {"7"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", form, nil, ""); response.Code != http.StatusForbidden {
		t.Fatalf("anonymous setup save status=%d, want 403", response.Code)
	}
	loginPage := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if loginPage.Code != http.StatusOK || len(loginPage.Result().Cookies()) != 1 {
		t.Fatalf("setup login page status=%d cookies=%v", loginPage.Code, loginPage.Result().Cookies())
	}
	if home := adminResponse(handler, http.MethodGet, "/", nil, loginPage.Result().Cookies()[0], ""); home.Code != http.StatusOK ||
		!strings.Contains(home.Body.String(), "Sign in as admin to finish setup") {
		t.Fatalf("signed-out setup home status=%d body=%q", home.Code, home.Body.String())
	}
	login := adminResponse(handler, http.MethodPost, "/admin/login", url.Values{
		"password": {"correct-password"},
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
		!strings.Contains(home.Body.String(), `name="repository_id"`) || !strings.Contains(home.Body.String(), `name="owner_id"`) ||
		strings.Contains(home.Body.String(), "Sign in as admin to save") ||
		strings.Contains(home.Body.String(), "secret-token") {
		t.Fatalf("setup form status=%d body=%q", home.Code, home.Body.String())
	}
	form.Set("version", setupFormVersion(t, home.Body.String()))
	invalid := form.Clone()
	invalid.Set("github_token", "bad secret token")
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", invalid, cookie, ""); response.Code == http.StatusSeeOther ||
		strings.Contains(response.Body.String(), "bad secret token") {
		t.Fatalf("invalid setup save status=%d echoed token=%t", response.Code, strings.Contains(response.Body.String(), "bad secret token"))
	}
	missingToken := form.Clone()
	missingToken.Set("github_token", "")
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", missingToken, cookie, ""); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "Enter a GitHub bot token") {
		t.Fatalf("missing setup token status=%d body=%q", response.Code, response.Body.String())
	}
	for _, tc := range []struct{ field, value string }{{"repository_id", "0"}, {"owner_id", "-1"}, {"repository_id", "not-a-number"}, {"owner_id", "9223372036854775808"}} {
		invalidID := form.Clone()
		invalidID.Set(tc.field, tc.value)
		if response := adminResponse(handler, http.MethodPost, "/admin/setup", invalidID, cookie, ""); response.Code != http.StatusConflict {
			t.Fatalf("invalid %s=%q setup status=%d", tc.field, tc.value, response.Code)
		}
	}
	unchanged, err := config.Load(path)
	if err != nil || unchanged.BotLogin != "" || unchanged.GitHub.Token != "" || len(unchanged.Repositories) != 0 {
		t.Fatalf("invalid setup changed config: bot=%q tokenSet=%t repositories=%d error=%v",
			unchanged.BotLogin, unchanged.GitHub.Token != "", len(unchanged.Repositories), err)
	}
	if _, err := config.Update(path, func(cfg *config.Config) error {
		cfg.StatePath = "var/state.json" // Same validated path as the omitted default.
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stale := adminResponse(handler, http.MethodPost, "/admin/setup", form, cookie, ""); stale.Code != http.StatusConflict ||
		!strings.Contains(stale.Body.String(), "configuration changed; reload") {
		t.Fatalf("equivalent-path stale setup save status=%d body=%q", stale.Code, stale.Body.String())
	}
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), `"state_path": "var/state.json"`) {
		t.Fatalf("stale setup changed the operator's path: data=%s error=%v", data, err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	form.Set("version", setupFormVersion(t, home.Body.String()))
	saved := adminResponse(handler, http.MethodPost, "/admin/setup", form, cookie, "")
	if saved.Code != http.StatusSeeOther || saved.Header().Get("Location") != "/" {
		t.Fatalf("setup save status=%d body=%q", saved.Code, saved.Body.String())
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.BotLogin != "mona-cat_octo" || cfg.GitHub.Token != "secret-token" ||
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
		!strings.Contains(home.Body.String(), "For each selected AI, set its API key") ||
		!strings.Contains(home.Body.String(), "sign in to its CLI on the executor host") ||
		!strings.Contains(home.Body.String(), "install skills as its service account") ||
		strings.Contains(home.Body.String(), "secret-token") || strings.Contains(home.Body.String(), "Save setup") {
		t.Fatalf("saved setup home status=%d body=%q", home.Code, home.Body.String())
	}
	_, err = config.Update(path, func(cfg *config.Config) error {
		cfg.GitHub.Token = "new-token"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := adminResponse(handler, http.MethodPost, "/admin/setup", form, cookie, "")
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "configuration changed; reload") {
		t.Fatalf("stale setup save status=%d body=%q", stale.Code, stale.Body.String())
	}
	cfg, err = config.Load(path)
	if err != nil || cfg.GitHub.Token != "new-token" {
		t.Fatalf("stale setup overwrote newer token: token=%q error=%v", cfg.GitHub.Token, err)
	}
	_, err = config.Update(path, func(cfg *config.Config) error {
		cfg.BotLogin = ""
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Leave blank to keep the saved token") ||
		!strings.Contains(home.Body.String(), `name="repository_id" type="number" min="1" step="1" value="42"`) ||
		!strings.Contains(home.Body.String(), `name="owner_id" type="number" min="1" step="1" value="7"`) ||
		strings.Contains(home.Body.String(), "The first repository starts") ||
		strings.Contains(home.Body.String(), "new-token") {
		t.Fatalf("partial setup form status=%d body=%q", home.Code, home.Body.String())
	}
	partial := form.Clone()
	partial.Set("version", setupFormVersion(t, home.Body.String()))
	partial.Set("github_token", "")
	if saved := adminResponse(handler, http.MethodPost, "/admin/setup", partial, cookie, ""); saved.Code != http.StatusSeeOther {
		t.Fatalf("partial setup save status=%d body=%q", saved.Code, saved.Body.String())
	}
	cfg, err = config.Load(path)
	if err != nil || cfg.BotLogin != "mona-cat_octo" || cfg.GitHub.Token != "new-token" {
		t.Fatalf("partial setup lost saved token: bot=%q token=%q error=%v", cfg.BotLogin, cfg.GitHub.Token, err)
	}
	_, err = config.Update(path, func(cfg *config.Config) error {
		cfg.GitHub.Token = ""
		cfg.Repositories[0].ID = 43
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	form.Set("version", setupFormVersion(t, adminResponse(handler, http.MethodGet, "/", nil, cookie, "").Body.String()))
	denied := adminResponse(handler, http.MethodPost, "/admin/setup", form, cookie, "")
	if denied.Code != http.StatusConflict || !strings.Contains(denied.Body.String(), "first repository differs") {
		t.Fatalf("changed first repository setup status=%d body=%q", denied.Code, denied.Body.String())
	}
	unchanged, err = config.Load(path)
	if err != nil || unchanged.GitHub.Token != "" || len(unchanged.Repositories) != 1 || unchanged.Repositories[0].ID != 43 {
		t.Fatalf("rejected setup changed config: tokenSet=%t repositories=%v error=%v",
			unchanged.GitHub.Token != "", unchanged.Repositories, err)
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
		{"trimmed provider", " codex ", map[string]config.CommandMapping{
			"review": {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
		}, "review", ""},
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
			form.Set("version", setupFormVersion(t, adminResponse(handler, http.MethodGet, "/", nil, cookie, "").Body.String()))
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
			if tc.name == "no commands" {
				home := adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
				if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Setup is incomplete") ||
					!strings.Contains(home.Body.String(), "add a command in Advanced config") || strings.Contains(home.Body.String(), "Setup saved") {
					t.Fatalf("no-command setup home status=%d body=%q", home.Code, home.Body.String())
				}
			}
		})
	}
}
