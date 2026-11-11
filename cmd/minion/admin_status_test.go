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
	"sync/atomic"
	"testing"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
)

func TestAdminStatusSummaryKeepsSecretsOut(t *testing.T) {
	cfg := config.Config{
		BotLogin: "fixture-bot", Main: "codex", Adversarial: "claude",
		GitHub:         config.GitHubConfig{Token: "secret-github-token"},
		Codex:          config.CodexConfig{Model: "gpt-test", ReasoningEffort: "medium", APIKey: "secret-codex-key"},
		Claude:         config.ClaudeConfig{Model: "sonnet-test", Effort: "high", APIKey: "secret-claude-key"},
		Admin:          &config.AdminConfig{Administrators: []config.AdminAccount{{Username: "admin", PasswordHash: "secret-password-hash"}}},
		Repositories:   []config.Repository{{Name: "owner/repo", AllowedActors: []string{"alice"}, AllowedActorIDs: map[string]int64{"alice": 7}}},
		AlwaysOnSkills: []string{"builtin://anti-prompt-injection", "https://github.com/a/b/tree/main/skills/caveman"},
		Commands:       map[string]config.CommandMapping{"review": {Skills: []string{"https://github.com/a/b/tree/main/skills/caveman", "https://github.com/a/b/tree/main/skills/review"}}},
	}
	view := statusView(&action.Orchestrator{BotLogin: cfg.BotLogin, BotID: 42}, cfg, false)
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
	for _, want := range []string{"@fixture-bot", "gpt-test", "sonnet-test", "Named requesters: 1", "Monitored: 1", "Configured: 3", `method="post" action="/admin/status/check"`, "Test GitHub auth"} {
		if !strings.Contains(body.String(), want) {
			t.Fatalf("status page missing %q: %s", want, body.String())
		}
	}
	for _, secret := range []string{"secret-github-token", "secret-codex-key", "secret-claude-key", "secret-password-hash"} {
		if strings.Contains(body.String(), secret) {
			t.Fatalf("status page exposed %q", secret)
		}
	}
}

func TestAdminStatusShowsInvitationAndFirstRepositoryState(t *testing.T) {
	cfg := config.Config{BotLogin: "bot", GitHub: config.GitHubConfig{Token: "token"},
		InvitationOwners: []string{"acme", "alice"}}
	for _, tc := range []struct {
		name     string
		saved    bool
		want     string
		unwanted string
	}{
		{name: "waiting for invitation", want: "Waiting for repository invitations", unwanted: "restart Minion"},
		{name: "first repository saved", saved: true, want: "First repository saved; restart Minion to begin monitoring.", unwanted: "Waiting for repository invitations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := statusView(&action.Orchestrator{BotLogin: "bot"}, cfg, tc.saved)
			var page struct {
				Dot                adminStatusView
				HeadHTML, TailHTML template.HTML
			}
			page.Dot = view
			var body bytes.Buffer
			if err := adminStatusHTML.ExecuteTemplate(&body, "admin status", page); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Monitored: 0", "Approved owners: 2", tc.want} {
				if !strings.Contains(body.String(), want) {
					t.Fatalf("status page missing %q: %s", want, body.String())
				}
			}
			if strings.Contains(body.String(), tc.unwanted) {
				t.Fatalf("status page incorrectly shows %q: %s", tc.unwanted, body.String())
			}
		})
	}
}

func TestAdminStatusCheckRequiresPost(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"id":42,"login":"fixture-bot"}`)
	}))
	t.Cleanup(server.Close)
	client := github.New("fixture-token")
	client.BaseURL = server.URL
	owner := &action.Orchestrator{GitHub: client, BotLogin: "fixture-bot", BotID: 42}
	live, _ := liveConfigFixture(t, false)
	jw, err := jaws.New()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAdminHandler(jw, config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()}}, owner, nil, live)
	if err != nil {
		jw.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { jw.Serve(); close(done) }()
	t.Cleanup(func() { jw.Close(); <-done })
	cookie := dashboardTestCookie(t, handler)
	if page := adminResponse(handler, http.MethodGet, "/admin/status", nil, cookie, ""); page.Code != http.StatusOK {
		t.Fatalf("status page: %d", page.Code)
	}
	if check := adminResponse(handler, http.MethodGet, "/admin/status/check", nil, cookie, ""); check.Code != http.StatusNotFound {
		t.Fatalf("GET check: %d, want 404", check.Code)
	}
	if check := adminResponse(handler, http.MethodPost, "/admin/status/check", nil, cookie, "https://other.example"); check.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST check: %d", check.Code)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("unrequested check contacted GitHub %d times", got)
	}
	check := adminResponse(handler, http.MethodPost, "/admin/status/check", nil, cookie, "")
	if check.Code != http.StatusOK || !strings.Contains(check.Body.String(), "Authenticated as @fixture-bot") || calls.Load() != 1 {
		t.Fatalf("POST check: status=%d calls=%d body=%q", check.Code, calls.Load(), check.Body.String())
	}
}

func TestAdminStatusCountsOpenRepositories(t *testing.T) {
	cfg := config.Config{Repositories: []config.Repository{
		{Name: "owner/default"},
		{Name: "owner/empty", AllowedActors: []string{}},
		{Name: "owner/closed", DenyAllActors: true},
	}}
	view := statusView(&action.Orchestrator{}, cfg, false)
	if view.OpenRepositories != 2 || view.Repositories != 3 {
		t.Fatalf("open repository count = %+v", view)
	}
}

func TestAdminStatusCountsRequestersByGitHubID(t *testing.T) {
	cfg := config.Config{Repositories: []config.Repository{
		{Name: "owner/first", AllowedActors: []string{"old-login"}, AllowedActorIDs: map[string]int64{"old-login": 7}},
		{Name: "owner/second", AllowedActors: []string{"new-login", "other"}, AllowedActorIDs: map[string]int64{"new-login": 7, "other": 8}},
	}}
	view := statusView(&action.Orchestrator{}, cfg, false)
	if view.Requesters != 2 || view.OpenRepositories != 0 {
		t.Fatalf("named requester count = %+v", view)
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
