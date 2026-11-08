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

func TestAdvancedConfigMasksSecretsAndStagesOnlyKeys(t *testing.T) {
	cfg := config.Config{
		BotLogin: "bot", GitHub: config.GitHubConfig{Token: "github-secret"},
		Admin: &config.AdminConfig{Listen: "127.0.0.1:8377", Administrators: []config.AdminAccount{testAdminAccount()}},
		Codex: config.CodexConfig{APIKey: "codex-secret"}, Claude: config.ClaudeConfig{APIKey: "claude-secret"},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o640); err != nil {
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
	view, err := advancedConfigView(path, live, *loaded.Admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"github-secret", "codex-secret", "claude-secret", cfg.Admin.Administrators[0].PasswordHash} {
		if strings.Contains(view.JSON, secret) {
			t.Fatalf("advanced editor exposed %q", secret)
		}
	}
	var edited config.Config
	if err := json.Unmarshal([]byte(view.JSON), &edited); err != nil {
		t.Fatal(err)
	}
	edited.Admin.Administrators[0].PasswordHash = "changed-password"
	bad, _ := json.Marshal(edited)
	if err := saveAdvancedConfig(path, live, *loaded.Admin, view.Version, string(bad)); err == nil || !strings.Contains(err.Error(), "admin password") {
		t.Fatalf("password edit accepted: %v", err)
	}
	edited.Admin.Administrators[0].PasswordHash = advancedPasswordMarker
	edited.BotLogin = "new-bot"
	edited.PollIntervalSeconds = 60
	edited.Codex.APIKey = "new-codex-secret"
	edited.Claude.APIKey = ""
	good, _ := json.Marshal(edited)
	if err := saveAdvancedConfig(path, live, *loaded.Admin, view.Version, string(good)); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.BotLogin != "new-bot" || saved.PollIntervalSeconds != 60 || saved.GitHub.Token != "github-secret" ||
		saved.Codex.APIKey != "new-codex-secret" || saved.Claude.APIKey != "" ||
		saved.Admin.Administrators[0].PasswordHash != cfg.Admin.Administrators[0].PasswordHash {
		t.Fatalf("advanced save changed unexpected fields: %+v", saved)
	}
	if !live.restartRequired || !live.pending || live.desired.Codex.APIKey != "new-codex-secret" ||
		live.desired.Claude.APIKey != "" || live.active.Codex.APIKey != "codex-secret" || live.active.BotLogin != "bot" {
		t.Fatal("advanced save activated fixed settings or failed to stage new AI keys")
	}
	if err := live.save(func(*config.Config) error { return nil }); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("live edit accepted before restart: %v", err)
	}
	if err := saveAdvancedConfig(path, live, *loaded.Admin, view.Version, string(good)); err == nil || !strings.Contains(err.Error(), "reload") {
		t.Fatalf("stale editor submission accepted: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("config permissions changed: mode=%v", info.Mode())
		}
	}
}

func TestAdvancedConfigRoutesRequireAdminAndSaveMaskedForm(t *testing.T) {
	account := testAdminAccount()
	cfg := config.Config{BotLogin: "bot", GitHub: config.GitHubConfig{Token: "github-secret"},
		Admin: &config.AdminConfig{Administrators: []config.AdminAccount{account}},
		Codex: config.CodexConfig{APIKey: "codex-secret"}, Commands: map[string]config.CommandMapping{},
		AlwaysOnSkills: []string{}, Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
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
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, *loaded.Admin, nil, nil, live)
	if err != nil {
		jw.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })
	if denied := adminResponse(handler, http.MethodGet, "/admin/config", nil, nil, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("anonymous config page status=%d", denied.Code)
	}
	cookie := dashboardTestCookie(t, handler)
	page := adminResponse(handler, http.MethodGet, "/admin/config", nil, cookie, "")
	body := page.Body.String()
	if page.Code != http.StatusOK || page.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(body, `name="jawsKey"`) || !strings.Contains(body, advancedSecretMarker) ||
		!strings.Contains(body, advancedPasswordMarker) || strings.Contains(body, "github-secret") ||
		strings.Contains(body, "codex-secret") || strings.Contains(body, account.PasswordHash) {
		t.Fatalf("masked config page: status=%d body=%q", page.Code, body)
	}
	view, err := advancedConfigView(path, live, *loaded.Admin)
	if err != nil {
		t.Fatal(err)
	}
	var edited config.Config
	if err := json.Unmarshal([]byte(view.JSON), &edited); err != nil {
		t.Fatal(err)
	}
	edited.AIInstructions = "new instructions"
	data, err = json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"version": {view.Version}, "config": {string(data)}}
	if denied := adminResponse(handler, http.MethodPost, "/admin/config", form, nil, ""); denied.Code != http.StatusForbidden {
		t.Fatalf("anonymous config POST status=%d", denied.Code)
	}
	if denied := adminResponse(handler, http.MethodPost, "/admin/config", form, cookie, "https://other.example"); denied.Code != http.StatusForbidden {
		t.Fatalf("cross-origin config POST status=%d", denied.Code)
	}
	if saved := adminResponse(handler, http.MethodPost, "/admin/config", form, cookie, "https://admin.example"); saved.Code != http.StatusSeeOther || saved.Header().Get("Location") != "/admin/config" {
		t.Fatalf("valid config POST: status=%d location=%q body=%q", saved.Code, saved.Header().Get("Location"), saved.Body.String())
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AIInstructions != "new instructions" || saved.GitHub.Token != cfg.GitHub.Token ||
		saved.Codex.APIKey != cfg.Codex.APIKey || saved.Admin.Administrators[0].PasswordHash != account.PasswordHash {
		t.Fatal("config route changed the wrong fields or lost masked secrets")
	}
}

func TestAdvancedConfigHandlesAllCredentialFields(t *testing.T) {
	before := config.Config{
		GitHub: config.GitHubConfig{Token: "github-token-secret"}, Codex: config.CodexConfig{APIKey: "codex-key-secret"},
		Claude:         config.ClaudeConfig{APIKey: "claude-key-secret"},
		RemoteExecutor: &config.RemoteExecutorConfig{Token: "remote-token-secret"},
		Admin: &config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()},
			GitHubOAuth: &config.OAuthClient{ClientSecret: "github-oauth"}, GitLabOAuth: &config.OAuthClient{ClientSecret: "gitlab-oauth"}},
	}
	after, err := cloneConfig(before)
	if err != nil {
		t.Fatal(err)
	}
	maskAdvancedSecrets(&after)
	data, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"github-token-secret", "codex-key-secret", "claude-key-secret", "remote-token-secret", "github-oauth", "gitlab-oauth", before.Admin.Administrators[0].PasswordHash} {
		if strings.Contains(string(data), `"`+secret+`"`) {
			t.Fatalf("masked config contains %q", secret)
		}
	}
	if err := restoreAdvancedSecrets(&after, &before); err != nil {
		t.Fatal(err)
	}
	if after.GitHub.Token != before.GitHub.Token || after.Codex.APIKey != before.Codex.APIKey ||
		after.Claude.APIKey != before.Claude.APIKey || after.RemoteExecutor.Token != before.RemoteExecutor.Token ||
		after.Admin.GitHubOAuth.ClientSecret != before.Admin.GitHubOAuth.ClientSecret ||
		after.Admin.GitLabOAuth.ClientSecret != before.Admin.GitLabOAuth.ClientSecret ||
		after.Admin.Administrators[0].PasswordHash != before.Admin.Administrators[0].PasswordHash {
		t.Fatal("masked credentials did not survive a no-change edit")
	}
	newClient := after
	newClient.Admin = &config.AdminConfig{Administrators: []config.AdminAccount{{Username: "admin", PasswordHash: advancedPasswordMarker}},
		GitLabOAuth: &config.OAuthClient{ClientSecret: advancedSecretMarker}}
	withoutClient := before
	withoutClient.Admin = &config.AdminConfig{Administrators: before.Admin.Administrators}
	if err := restoreAdvancedSecrets(&newClient, &withoutClient); err == nil {
		t.Fatal("new OAuth client accepted the unchanged-secret marker")
	}
}

func TestAdvancedConfigKeyOnlyNeedsNoRestart(t *testing.T) {
	cfg := config.Config{BotLogin: "bot", Admin: &config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()}},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
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
	view, err := advancedConfigView(path, live, *loaded.Admin)
	if err != nil {
		t.Fatal(err)
	}
	var edited config.Config
	if err := json.Unmarshal([]byte(view.JSON), &edited); err != nil {
		t.Fatal(err)
	}
	edited.Codex.APIKey = "new-key"
	data, err = json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveAdvancedConfig(path, live, *loaded.Admin, view.Version, string(data)); err != nil {
		t.Fatal(err)
	}
	if live.restartRequired || !live.pending || live.desired.Codex.APIKey != "new-key" {
		t.Fatalf("key-only edit: restart=%t pending=%t key_staged=%t", live.restartRequired, live.pending, live.desired.Codex.APIKey == "new-key")
	}
	if err := live.save(func(cfg *config.Config) error { cfg.AIInstructions = "updated"; return nil }); err != nil {
		t.Fatalf("later live edit was blocked after key-only save: %v", err)
	}
	_, generation := live.next()
	live.applied(loaded, generation, errors.New("key preflight failed"))
	restored, err := config.Load(path)
	if err != nil || restored.Codex.APIKey != loaded.Codex.APIKey || restored.AIInstructions != "updated" ||
		live.desired.Codex.APIKey != loaded.Codex.APIKey {
		t.Fatalf("rejected Advanced key survived or lost another edit: saved=%+v error=%v", restored, err)
	}
}

func TestAdvancedConfigCanRemoveAdminBlock(t *testing.T) {
	cfg := config.Config{BotLogin: "bot", Admin: &config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()}},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}
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
	runtime := *loaded.Admin
	view, err := advancedConfigView(path, live, runtime)
	if err != nil {
		t.Fatal(err)
	}
	var edited config.Config
	if err := json.Unmarshal([]byte(view.JSON), &edited); err != nil {
		t.Fatal(err)
	}
	edited.Admin = nil
	data, err = json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveAdvancedConfig(path, live, runtime, view.Version, string(data)); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Admin != nil || !live.restartRequired || live.pending || live.active.Admin == nil {
		t.Fatal("admin removal did not save as a restart-required change")
	}
	view, err = advancedConfigView(path, live, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(view.JSON), &edited); err != nil {
		t.Fatal(err)
	}
	edited.BotLogin = "new-bot"
	data, err = json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveAdvancedConfig(path, live, runtime, view.Version, string(data)); err != nil {
		t.Fatalf("later edit after admin removal: %v", err)
	}
	edited.Admin = &config.AdminConfig{Administrators: []config.AdminAccount{{Username: "admin", PasswordHash: advancedPasswordMarker}}}
	data, err = json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	view, err = advancedConfigView(path, live, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveAdvancedConfig(path, live, runtime, view.Version, string(data)); err == nil || !strings.Contains(err.Error(), "admin password") {
		t.Fatalf("admin password was reintroduced through the editor: %v", err)
	}
}
