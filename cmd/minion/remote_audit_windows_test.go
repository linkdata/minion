//go:build windows

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestWindowsSetupDoesNotSelectRemoteAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Config{
		Admin: &config.AdminConfig{Listen: "127.0.0.1:8377", Administrators: []config.AdminAccount{testAdminAccount()}},
		Commands: map[string]config.CommandMapping{
			"audit": {Action: "security-audit", Skills: []string{"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"}},
		},
		MainSettings: config.RoleModelConfig{Model: "main-model", Effort: "high"},
		RemoteExecutor: &config.RemoteExecutorConfig{ExecutorID: "worker", Listen: "127.0.0.1:9999", TLSCertFile: "cert", TLSKeyFile: "key",
			Token: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	post := func() *httptest.ResponseRecorder {
		t.Helper()
		loaded, version, err := loadSetupConfig(path, *cfg.Admin)
		if err != nil {
			t.Fatal(err)
		}
		form := url.Values{"version": {version}, "bot_login": {"example-bot"}, "github_token": {"secret-token"},
			"repository": {"example/repo"}, "repository_id": {"42"}, "owner_id": {"7"}}
		request := httptest.NewRequest(http.MethodPost, "/admin/setup", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		(&adminAccess{setupPath: path, setupAdmin: *loaded.Admin}).setupPost().ServeHTTP(response, request)
		return response
	}
	if response := post(); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "No configured command can run") {
		t.Fatalf("audit-only setup status=%d body=%q", response.Code, response.Body.String())
	}
	if _, err := config.Update(path, func(cfg *config.Config) error {
		cfg.Commands["review"] = config.CommandMapping{Action: "review", Skills: []string{"builtin://anti-prompt-injection"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if response := post(); response.Code != http.StatusSeeOther {
		t.Fatalf("review setup status=%d body=%q", response.Code, response.Body.String())
	}
	saved, err := config.Load(path)
	if err != nil || len(saved.Repositories) != 1 || len(saved.Repositories[0].Actions) != 1 || saved.Repositories[0].Actions[0] != "review" {
		t.Fatalf("setup selected unsupported action: repositories=%v error=%v", saved.Repositories, err)
	}
}

func TestWindowsAdminDoesNotOfferRemoteAudit(t *testing.T) {
	cfg := config.Config{Main: "codex", RemoteExecutor: &config.RemoteExecutorConfig{}, Commands: map[string]config.CommandMapping{
		"audit": {Action: "security-audit", Skills: []string{"https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"}},
	}}
	if err := validateAdminActions(cfg, []string{"audit"}, nil); err == nil {
		t.Fatal("admin form accepted a remote audit without a publishing validator")
	}
	view := configurationView(adminConfigSnapshot{Config: cfg, Remote: true, RemoteCommands: map[string]bool{"audit": true}})
	if len(view.Commands) != 0 {
		t.Fatalf("admin form offered audit on Windows publishing host: %v", view.Commands)
	}
}
