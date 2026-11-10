package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

func TestSetupSavesBotOfflineAndAddsRepositoriesByURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.local.json")
	initial := config.Config{Admin: &config.AdminConfig{
		Listen: "127.0.0.1:8377", Administrators: []config.AdminAccount{testAdminAccount()},
	}}
	data, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	previousTransport := http.DefaultTransport
	allowGitHub, calls := false, 0
	http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if !allowGitHub {
			t.Errorf("offline setup called GitHub: %s", request.URL)
			return nil, errors.New("unexpected setup request")
		}
		if request.URL.Host != "api.github.com" || request.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("unexpected GitHub request: %s authorization=%q", request.URL, request.Header.Get("Authorization"))
		}
		response := httptest.NewRecorder()
		switch request.URL.Path {
		case "/repos/example/repo":
			_, _ = response.WriteString(`{"id":42,"full_name":"example/repo","owner":{"id":7},"default_branch":"main"}`)
		case "/repos/example/other":
			_, _ = response.WriteString(`{"id":43,"full_name":"example/other","owner":{"id":7},"default_branch":"main"}`)
		case "/repos/example/repo/notifications", "/repos/example/other/notifications":
			_, _ = response.WriteString(`[]`)
		case "/repos/example/repo/collaborators/mona-cat_octo/permission", "/repos/example/other/collaborators/mona-cat_octo/permission":
			_, _ = response.WriteString(`{"permission":"write"}`)
		default:
			t.Errorf("unexpected GitHub path %s", request.URL.Path)
			response.WriteHeader(http.StatusNotFound)
		}
		return response.Result(), nil
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
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", url.Values{
		"version": {"missing"}, "bot_login": {"mona-cat_octo"}, "github_token": {"secret-token"},
	}, nil, ""); response.Code != http.StatusForbidden {
		t.Fatalf("anonymous setup save status=%d, want 403", response.Code)
	}
	loginPage := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if loginPage.Code != http.StatusOK || len(loginPage.Result().Cookies()) != 1 {
		t.Fatalf("setup login page status=%d cookies=%v", loginPage.Code, loginPage.Result().Cookies())
	}
	login := adminResponse(handler, http.MethodPost, "/admin/login", url.Values{
		"password": {"correct-password"},
	}, loginPage.Result().Cookies()[0], "")
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("setup login status=%d cookies=%v", login.Code, login.Result().Cookies())
	}
	cookie := login.Result().Cookies()[0]
	home := adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Save bot credentials") ||
		!strings.Contains(home.Body.String(), "Advanced config and GitHub auth test") ||
		strings.Contains(home.Body.String(), `action="/admin/repositories/add"`) ||
		strings.Contains(home.Body.String(), `name="repository_id"`) {
		t.Fatalf("initial setup form status=%d body=%q", home.Code, home.Body.String())
	}
	credentials := url.Values{"version": {setupFormVersion(t, home.Body.String())},
		"bot_login": {"mona-cat_octo"}, "github_token": {"secret-token"}}
	missingToken := credentials.Clone()
	missingToken.Set("github_token", "")
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", missingToken, cookie, ""); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "Enter a GitHub bot token") {
		t.Fatalf("missing setup token status=%d body=%q", response.Code, response.Body.String())
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", credentials, cookie, ""); response.Code != http.StatusSeeOther || calls != 0 {
		t.Fatalf("offline credential save status=%d GitHub calls=%d", response.Code, calls)
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.BotLogin != "mona-cat_octo" || cfg.GitHub.Token != "secret-token" || len(cfg.Repositories) != 0 {
		t.Fatalf("saved credentials: bot=%q tokenSet=%t repositories=%d error=%v", cfg.BotLogin, cfg.GitHub.Token != "", len(cfg.Repositories), err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), `action="/admin/repositories/add"`) ||
		!strings.Contains(home.Body.String(), `name="url" type="url"`) ||
		strings.Contains(home.Body.String(), "secret-token") || strings.Contains(home.Body.String(), `name="repository_id"`) {
		t.Fatalf("repository setup form status=%d body=%q", home.Code, home.Body.String())
	}
	add := url.Values{"version": {setupFormVersion(t, home.Body.String())}, "url": {"https://github.com/Example/Repo"}}
	if denied := adminResponse(handler, http.MethodPost, "/admin/repositories/add", add, nil, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("anonymous repository add status=%d", denied.Code)
	}
	invalidURL := add.Clone()
	invalidURL.Set("url", "https://other.example/Example/Repo")
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", invalidURL, cookie, ""); response.Code != http.StatusConflict || calls != 0 {
		t.Fatalf("invalid repository URL status=%d GitHub calls=%d", response.Code, calls)
	}
	if _, err := config.Update(path, func(cfg *config.Config) error {
		cfg.StatePath = "var/state.json"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", add, cookie, ""); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "configuration changed; reload") || calls != 0 {
		t.Fatalf("stale repository add status=%d GitHub calls=%d", response.Code, calls)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	add.Set("version", setupFormVersion(t, home.Body.String()))
	allowGitHub = true
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", add, cookie, ""); response.Code != http.StatusSeeOther {
		t.Fatalf("repository add status=%d body=%q", response.Code, response.Body.String())
	}
	cfg, err = config.Load(path)
	if err != nil || len(cfg.Repositories) != 1 || cfg.Repositories[0].Name != "example/repo" ||
		cfg.Repositories[0].ID != 42 || cfg.Repositories[0].OwnerID != 7 || !cfg.Repositories[0].DenyAllActors ||
		len(cfg.Repositories[0].Actions) != 1 || cfg.Repositories[0].Actions[0] != "review" {
		t.Fatalf("added repository: repositories=%+v error=%v", cfg.Repositories, err)
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", add, cookie, ""); response.Code != http.StatusConflict {
		t.Fatalf("duplicate repository status=%d", response.Code)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if !strings.Contains(home.Body.String(), "Setup saved") || !strings.Contains(home.Body.String(), "example/repo") ||
		!strings.Contains(home.Body.String(), `action="/admin/repositories/add"`) {
		t.Fatalf("completed setup no longer supports adding repositories: %s", home.Body.String())
	}
	add.Set("version", setupFormVersion(t, home.Body.String()))
	add.Set("url", "https://github.com/Example/Other")
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", add, cookie, ""); response.Code != http.StatusSeeOther {
		t.Fatalf("second repository add status=%d body=%q", response.Code, response.Body.String())
	}
	cfg, err = config.Load(path)
	if err != nil || len(cfg.Repositories) != 2 || cfg.Repositories[1].Name != "example/other" || cfg.Repositories[1].ID != 43 {
		t.Fatalf("second repository was not appended: repositories=%+v error=%v", cfg.Repositories, err)
	}
}

func TestSetupRepositoryActions(t *testing.T) {
	for _, tc := range []struct {
		name, main string
		commands   map[string]config.CommandMapping
		want       string
		wantError  bool
	}{
		{name: "defaults", want: "review"},
		{name: "no commands", commands: map[string]config.CommandMapping{}},
		{name: "custom commands", commands: map[string]config.CommandMapping{
			"zeta":  {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
			"alpha": {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
		}, want: "alpha"},
		{name: "unsupported command", main: "claude", commands: map[string]config.CommandMapping{
			"audit": {Action: "security-audit", Skills: []string{"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"}},
			"zeta":  {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
		}, want: "zeta"},
		{name: "only unsupported command", main: "claude", commands: map[string]config.CommandMapping{
			"audit": {Action: "security-audit", Skills: []string{"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"}},
		}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actions, err := setupRepositoryActions(config.Config{Main: tc.main, Commands: tc.commands})
			if (err != nil) != tc.wantError || strings.Join(actions, ",") != tc.want {
				t.Fatalf("actions=%v error=%v, want %q error=%t", actions, err, tc.want, tc.wantError)
			}
		})
	}
}
