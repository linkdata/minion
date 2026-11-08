package main

import (
	"encoding/json"
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
		"repository": {"Example/Repo"}, "repository_id": {"42"}, "owner_id": {"7"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", form, nil, ""); response.Code != http.StatusForbidden {
		t.Fatalf("anonymous setup save status=%d, want 403", response.Code)
	}
	loginPage := adminResponse(handler, http.MethodGet, "/admin/login", nil, nil, "")
	if loginPage.Code != http.StatusOK || len(loginPage.Result().Cookies()) != 1 {
		t.Fatalf("setup login page status=%d cookies=%v", loginPage.Code, loginPage.Result().Cookies())
	}
	login := adminResponse(handler, http.MethodPost, "/admin/login", url.Values{
		"username": {"admin"}, "password": {"correct-password"},
	}, loginPage.Result().Cookies()[0], "")
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("setup login status=%d cookies=%v", login.Code, login.Result().Cookies())
	}
	cookie := login.Result().Cookies()[0]
	home := adminResponse(handler, http.MethodGet, "/", nil, cookie, "")
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), "Save setup") ||
		strings.Contains(home.Body.String(), "secret-token") {
		t.Fatalf("setup form status=%d body=%q", home.Code, home.Body.String())
	}
	invalid := url.Values{"bot_login": {"example-bot"}, "github_token": {"bad secret token"},
		"repository": {"Example/Repo"}, "repository_id": {"42"}, "owner_id": {"7"}}
	if response := adminResponse(handler, http.MethodPost, "/admin/setup", invalid, cookie, ""); response.Code == http.StatusSeeOther ||
		strings.Contains(response.Body.String(), "bad secret token") {
		t.Fatalf("invalid setup save status=%d echoed token=%t", response.Code, strings.Contains(response.Body.String(), "bad secret token"))
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
}
