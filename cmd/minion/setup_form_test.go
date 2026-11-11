package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

func TestRemoveInvitationOwnerFromMixedCaseConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"bot_login":"bot","github":{"token":"test-token"},"invitation_owners":["Acme","other"],"invitation_owner_ids":{"Acme":42,"other":7}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Update(path, func(cfg *config.Config) error { return removeInvitationOwner(cfg, "acme") }); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(saved.InvitationOwners, []string{"other"}) || len(saved.InvitationOwnerIDs) != 1 || saved.InvitationOwnerIDs["other"] != 7 {
		t.Fatalf("owner removal left a stale pin: owners=%v IDs=%v", saved.InvitationOwners, saved.InvitationOwnerIDs)
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
		case "/repos/example/readonly":
			_, _ = response.WriteString(`{"id":44,"full_name":"example/readonly","owner":{"id":7},"default_branch":"main"}`)
		case "/repos/example/repo/notifications", "/repos/example/other/notifications", "/repos/example/readonly/notifications":
			_, _ = response.WriteString(`[]`)
		case "/repos/example/repo/collaborators/mona-cat_octo/permission", "/repos/example/other/collaborators/mona-cat_octo/permission":
			_, _ = response.WriteString(`{"permission":"write"}`)
		case "/repos/example/readonly/collaborators/mona-cat_octo/permission":
			_, _ = response.WriteString(`{"permission":"read"}`)
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
	if page := adminResponse(handler, http.MethodGet, "/", nil, loginPage.Result().Cookies()[0], ""); page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), "Sign in as admin to finish setup") {
		t.Fatalf("signed-out setup home status=%d body=%q", page.Code, page.Body.String())
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
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Save bot credentials") ||
		!strings.Contains(home.Body.String(), `href="/admin/config"`) ||
		!strings.Contains(home.Body.String(), `action="/admin/invitation-owners/add"`) ||
		strings.Contains(home.Body.String(), `action="/admin/repositories/add"`) ||
		strings.Contains(home.Body.String(), `name="repository_id"`) {
		t.Fatalf("initial setup form status=%d body=%q", home.Code, home.Body.String())
	}
	ownerForm := url.Values{"version": {setupFormVersion(t, home.Body.String())}, "login": {"Acme"}, "id": {"91"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/invitation-owners/add", ownerForm, nil, ""); response.Code != http.StatusForbidden {
		t.Fatalf("anonymous owner approval status=%d", response.Code)
	}
	invalidOwner := ownerForm.Clone()
	invalidOwner.Set("id", "0")
	if response := adminResponse(handler, http.MethodPost, "/admin/invitation-owners/add", invalidOwner, cookie, ""); response.Code != http.StatusConflict || calls != 0 {
		t.Fatalf("invalid owner approval status=%d GitHub calls=%d", response.Code, calls)
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/invitation-owners/add", ownerForm, cookie, ""); response.Code != http.StatusSeeOther {
		t.Fatalf("offline owner approval status=%d body=%q", response.Code, response.Body.String())
	}
	if saved, err := config.Load(path); err != nil || len(saved.InvitationOwners) != 1 || saved.InvitationOwners[0] != "acme" || saved.InvitationOwnerIDs["acme"] != 91 || calls != 0 {
		t.Fatalf("offline owner approval was not pinned: owners=%v IDs=%v calls=%d error=%v", saved.InvitationOwners, saved.InvitationOwnerIDs, calls, err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if !strings.Contains(home.Body.String(), `action="/admin/invitation-owners/remove"`) || !strings.Contains(home.Body.String(), "(ID 91)") {
		t.Fatalf("approved owner not shown during setup: %s", home.Body.String())
	}
	removeOwner := url.Values{"version": {setupFormVersion(t, home.Body.String())}, "login": {"acme"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/invitation-owners/remove", removeOwner, cookie, ""); response.Code != http.StatusSeeOther {
		t.Fatalf("offline owner removal status=%d body=%q", response.Code, response.Body.String())
	}
	if saved, err := config.Load(path); err != nil || len(saved.InvitationOwners) != 0 || len(saved.InvitationOwnerIDs) != 0 || calls != 0 {
		t.Fatalf("offline owner removal failed: owners=%v IDs=%v calls=%d error=%v", saved.InvitationOwners, saved.InvitationOwnerIDs, calls, err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	beforeCredentials := url.Values{"version": {setupFormVersion(t, home.Body.String())}, "url": {"https://github.com/Example/Repo"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", beforeCredentials, cookie, ""); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "Save the GitHub bot credentials") || calls != 0 {
		t.Fatalf("repository add before credentials status=%d body=%q GitHub calls=%d", response.Code, response.Body.String(), calls)
	}
	credentials := url.Values{"version": {setupFormVersion(t, home.Body.String())},
		"bot_login": {"mona-cat_octo"}, "github_token": {"secret-token"}}
	missingToken := credentials.Clone()
	missingToken.Set("github_token", "")
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", missingToken, cookie, ""); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "Enter a GitHub bot token") {
		t.Fatalf("missing setup token status=%d body=%q", response.Code, response.Body.String())
	}
	invalidToken := credentials.Clone()
	invalidToken.Set("github_token", "bad secret token")
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", invalidToken, cookie, ""); response.Code != http.StatusConflict ||
		strings.Contains(response.Body.String(), "bad secret token") {
		t.Fatalf("invalid setup token status=%d echoed=%t", response.Code, strings.Contains(response.Body.String(), "bad secret token"))
	}
	if unchanged, err := config.Load(path); err != nil || unchanged.BotLogin != "" || unchanged.GitHub.Token != "" {
		t.Fatalf("invalid setup token changed credentials: bot=%q tokenSet=%t error=%v", unchanged.BotLogin, unchanged.GitHub.Token != "", err)
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", credentials, cookie, ""); response.Code != http.StatusSeeOther || calls != 0 {
		t.Fatalf("offline credential save status=%d GitHub calls=%d", response.Code, calls)
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.BotLogin != "mona-cat_octo" || cfg.GitHub.Token != "secret-token" || len(cfg.Repositories) != 0 {
		t.Fatalf("saved credentials: bot=%q tokenSet=%t repositories=%d error=%v", cfg.BotLogin, cfg.GitHub.Token != "", len(cfg.Repositories), err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	ownerForm.Set("version", setupFormVersion(t, home.Body.String()))
	if response := adminResponse(handler, http.MethodPost, "/admin/invitation-owners/add", ownerForm, cookie, ""); response.Code != http.StatusSeeOther {
		t.Fatalf("approve owner after credentials status=%d body=%q", response.Code, response.Body.String())
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if !strings.Contains(home.Body.String(), "Setup saved") || !strings.Contains(home.Body.String(), "Restart Minion to check invitations from approved owners") || calls != 0 {
		t.Fatalf("invitation-only setup status=%d body=%q GitHub calls=%d", home.Code, home.Body.String(), calls)
	}
	removeOwner.Set("version", setupFormVersion(t, home.Body.String()))
	if response := adminResponse(handler, http.MethodPost, "/admin/invitation-owners/remove", removeOwner, cookie, ""); response.Code != http.StatusSeeOther {
		t.Fatalf("remove owner after credentials status=%d body=%q", response.Code, response.Body.String())
	}
	if _, err := config.Update(path, func(cfg *config.Config) error {
		cfg.Commands = map[string]config.CommandMapping{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Add a command in Advanced config") ||
		strings.Contains(home.Body.String(), `action="/admin/repositories/add"`) {
		t.Fatalf("setup offered Add repository with no command: status=%d body=%q", home.Code, home.Body.String())
	}
	withoutCommand := url.Values{"version": {setupFormVersion(t, home.Body.String())}, "url": {"https://github.com/Example/Repo"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", withoutCommand, cookie, ""); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "Add an available command") || calls != 0 {
		t.Fatalf("repository add without command status=%d body=%q GitHub calls=%d", response.Code, response.Body.String(), calls)
	}
	if _, err := config.Update(path, func(cfg *config.Config) error {
		cfg.Commands = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), `action="/admin/repositories/add"`) ||
		!strings.Contains(home.Body.String(), `name="url" type="url"`) ||
		strings.Contains(home.Body.String(), "secret-token") || strings.Contains(home.Body.String(), `name="repository_id"`) {
		t.Fatalf("repository setup form status=%d body=%q", home.Code, home.Body.String())
	}
	blankToken := credentials.Clone()
	blankToken.Set("version", setupFormVersion(t, home.Body.String()))
	blankToken.Set("github_token", "")
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", blankToken, cookie, ""); response.Code != http.StatusSeeOther {
		t.Fatalf("blank token update status=%d body=%q", response.Code, response.Body.String())
	}
	if saved, err := config.Load(path); err != nil || saved.GitHub.Token != "secret-token" {
		t.Fatalf("blank token update changed saved token: set=%t error=%v", saved.GitHub.Token != "", err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	add := url.Values{"version": {setupFormVersion(t, home.Body.String())}, "url": {"https://GitHub.com/Example/Repo"}}
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
	staleCredentials := credentials.Clone()
	staleCredentials.Set("version", add.Get("version"))
	staleCredentials.Set("github_token", "replacement-token")
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", staleCredentials, cookie, ""); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "configuration changed; reload") {
		t.Fatalf("stale credentials status=%d body=%q", response.Code, response.Body.String())
	}
	if saved, err := config.Load(path); err != nil || saved.GitHub.Token != "secret-token" {
		t.Fatalf("stale credentials replaced saved token: set=%t error=%v", saved.GitHub.Token != "", err)
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", add, cookie, ""); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "configuration changed; reload") || calls != 0 {
		t.Fatalf("stale repository add status=%d GitHub calls=%d", response.Code, calls)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	add.Set("version", setupFormVersion(t, home.Body.String()))
	allowGitHub = true
	beforeReadOnly, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := add.Clone()
	readOnly.Set("url", "https://github.com/Example/Readonly")
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", readOnly, cookie, ""); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "Repository is not ready") {
		t.Fatalf("read-only repository add status=%d body=%q", response.Code, response.Body.String())
	}
	if afterReadOnly, err := os.ReadFile(path); err != nil || !bytes.Equal(afterReadOnly, beforeReadOnly) {
		t.Fatalf("read-only repository changed config: error=%v", err)
	}
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", add, cookie, ""); response.Code != http.StatusSeeOther {
		t.Fatalf("repository add status=%d body=%q", response.Code, response.Body.String())
	}
	cfg, err = config.Load(path)
	if err != nil || len(cfg.Repositories) != 1 || cfg.Repositories[0].Name != "example/repo" ||
		cfg.Repositories[0].ID != 42 || cfg.Repositories[0].OwnerID != 7 || !cfg.Repositories[0].DenyAllActors ||
		len(cfg.Repositories[0].Actions) != 1 || cfg.Repositories[0].Actions[0] != "review" {
		t.Fatalf("added repository: repositories=%+v error=%v", cfg.Repositories, err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if !strings.Contains(home.Body.String(), "Setup saved") || !strings.Contains(home.Body.String(), "example/repo") ||
		!strings.Contains(home.Body.String(), "For each selected AI, set its API key") ||
		!strings.Contains(home.Body.String(), "sign in to its CLI on the executor host") ||
		!strings.Contains(home.Body.String(), "install skills as its service account") ||
		!strings.Contains(home.Body.String(), `action="/admin/repositories/add"`) {
		t.Fatalf("completed setup no longer supports adding repositories: %s", home.Body.String())
	}
	add.Set("version", setupFormVersion(t, home.Body.String()))
	beforeDuplicate := calls
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", add, cookie, ""); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "Repository is already configured") || calls != beforeDuplicate {
		t.Fatalf("duplicate repository status=%d body=%q GitHub calls=%d", response.Code, response.Body.String(), calls-beforeDuplicate)
	}
	add.Set("url", "https://github.com/Example/Other")
	if response := adminResponse(handler, http.MethodPost, "/admin/repositories/add", add, cookie, ""); response.Code != http.StatusSeeOther {
		t.Fatalf("second repository add status=%d body=%q", response.Code, response.Body.String())
	}
	cfg, err = config.Load(path)
	if err != nil || len(cfg.Repositories) != 2 || cfg.Repositories[1].Name != "example/other" || cfg.Repositories[1].ID != 43 {
		t.Fatalf("second repository was not appended: repositories=%+v error=%v", cfg.Repositories, err)
	}
	if _, err := config.Update(path, func(cfg *config.Config) error {
		cfg.InvitationOwners = []string{"acme"}
		cfg.InvitationOwnerIDs = map[string]int64{"acme": 91}
		cfg.Commands = map[string]config.CommandMapping{}
		for i := range cfg.Repositories {
			cfg.Repositories[i].Actions = nil
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if !strings.Contains(home.Body.String(), "Setup is incomplete") || strings.Contains(home.Body.String(), "Setup saved") {
		t.Fatalf("repository without commands appeared ready: %s", home.Body.String())
	}
	if _, err := config.Update(path, func(cfg *config.Config) error { cfg.Repositories = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	home = adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if !strings.Contains(home.Body.String(), "Setup saved") || !strings.Contains(home.Body.String(), "Add an available command") {
		t.Fatalf("invitation-only setup omitted command guidance: %s", home.Body.String())
	}
}

func TestSetupRepositoryActions(t *testing.T) {
	for _, tc := range []struct {
		name, main string
		commands   map[string]config.CommandMapping
		want       []string
		wantError  bool
	}{
		{name: "no commands", commands: map[string]config.CommandMapping{}, wantError: true},
		{name: "review preferred", commands: map[string]config.CommandMapping{
			"review": {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
			"alpha":  {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
		}, want: []string{"review"}},
		{name: "custom commands", commands: map[string]config.CommandMapping{
			"zeta":  {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
			"alpha": {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
		}, want: []string{"alpha"}},
		{name: "unsupported command", main: "claude", commands: map[string]config.CommandMapping{
			"audit": {Action: "security-audit", Skills: []string{"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"}},
			"zeta":  {Action: "review", Skills: []string{"builtin://anti-prompt-injection"}},
		}, want: []string{"zeta"}},
		{name: "only unsupported command", main: "claude", commands: map[string]config.CommandMapping{
			"audit": {Action: "security-audit", Skills: []string{"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"}},
		}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actions, err := setupRepositoryActions(config.Config{Main: tc.main, Commands: tc.commands})
			if (err != nil) != tc.wantError || !slices.Equal(actions, tc.want) {
				t.Fatalf("actions=%v error=%v, want %v error=%t", actions, err, tc.want, tc.wantError)
			}
		})
	}
}
