package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
)

func TestAdminStatusSummaryKeepsSecretsOut(t *testing.T) {
	cfg := config.Config{
		BotLogin: "fixture-bot", Main: "codex", Adversarial: "claude",
		Codex:          config.CodexConfig{Model: "gpt-test", ReasoningEffort: "medium", APIKey: "secret-codex-key"},
		Claude:         config.ClaudeConfig{Model: "sonnet-test", Effort: "high", APIKey: "secret-claude-key"},
		Admin:          &config.AdminConfig{Administrators: []config.AdminAccount{{Username: "operator", PasswordHash: "secret-password-hash"}}},
		Repositories:   []config.Repository{{Name: "owner/repo", AllowedActors: []string{"alice"}}},
		AlwaysOnSkills: []string{"builtin://anti-prompt-injection", "https://github.com/a/b/tree/main/skills/caveman"},
		Commands:       map[string]config.CommandMapping{"review": {Skills: []string{"https://github.com/a/b/tree/main/skills/caveman", "https://github.com/a/b/tree/main/skills/review"}}},
	}
	view := statusView(&action.Orchestrator{BotLogin: cfg.BotLogin, BotID: 42}, cfg)
	if view.Requesters != 1 || view.OpenRepositories != 0 || view.Repositories != 1 || view.Skills != 3 || view.Main.Provider != "codex" || view.Critic.Provider != "claude" {
		t.Fatalf("status summary = %+v", view)
	}
	var page struct {
		Dot                adminStatusView
		HeadHTML, TailHTML template.HTML
	}
	page.Dot = view
	var body bytes.Buffer
	if err := adminStatusHTML.ExecuteTemplate(&body, "admin status", page); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"@fixture-bot", "gpt-test", "sonnet-test", "Named requesters: 1", "Monitored: 1", "Configured: 3", "Test GitHub auth"} {
		if !strings.Contains(body.String(), want) {
			t.Fatalf("status page missing %q: %s", want, body.String())
		}
	}
	for _, secret := range []string{"secret-codex-key", "secret-claude-key", "secret-password-hash"} {
		if strings.Contains(body.String(), secret) {
			t.Fatalf("status page exposed %q", secret)
		}
	}
}

func TestAdminStatusGitHubAuthCheck(t *testing.T) {
	var mu sync.Mutex
	status := http.StatusOK
	identity := `{"id":42,"login":"fixture-bot"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		responseStatus, responseIdentity := status, identity
		mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected GitHub request: %s %s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(responseStatus)
		_, _ = fmt.Fprint(w, responseIdentity)
	}))
	t.Cleanup(server.Close)
	client := github.New("fixture-token")
	client.BaseURL = server.URL
	owner := &action.Orchestrator{GitHub: client, BotLogin: "fixture-bot", BotID: 42}
	if got := checkGitHubAuth(context.Background(), owner); !got.Checked || !got.OK || !strings.Contains(got.Message, "@fixture-bot") {
		t.Fatalf("successful check = %+v", got)
	}
	mu.Lock()
	identity = `{"id":99,"login":"another-bot"}`
	mu.Unlock()
	if got := checkGitHubAuth(context.Background(), owner); got.OK || !strings.Contains(got.Message, "does not match") {
		t.Fatalf("wrong account = %+v", got)
	}
	mu.Lock()
	status = http.StatusUnauthorized
	identity = `{"message":"Bad credentials"}`
	mu.Unlock()
	if got := checkGitHubAuth(context.Background(), owner); got.OK || got.Message != "GitHub returned HTTP 401." {
		t.Fatalf("rejected credential = %+v", got)
	}
}
