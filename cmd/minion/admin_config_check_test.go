package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/jaws"
	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
)

func TestSavedGitHubAuthButtonWorksInSetupAndAdmin(t *testing.T) {
	for _, setup := range []bool{true, false} {
		t.Run(fmt.Sprint("setup=", setup), func(t *testing.T) {
			cfg := config.Config{Admin: &config.AdminConfig{Administrators: []config.AdminAccount{testAdminAccount()}}}
			if !setup {
				cfg.BotLogin = "saved-bot"
				cfg.Repositories = []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			jw, err := jaws.New()
			if err != nil {
				t.Fatal(err)
			}
			defer jw.Close()
			var handler http.Handler
			if setup {
				handler, err = newAdminHandlerWithSetupPath(jw, *cfg.Admin, nil, nil, true, path)
			} else {
				loaded, loadErr := config.Load(path)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				live, liveErr := newAdminLiveConfig(path, loaded)
				if liveErr != nil {
					t.Fatal(liveErr)
				}
				handler, err = newAdminHandler(jw, *cfg.Admin, nil, nil, live)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { jw.Serve(); close(done) }()
			t.Cleanup(func() { <-done })
			if denied := adminResponse(handler, http.MethodPost, "/admin/config/check", nil, nil, ""); denied.Code != http.StatusForbidden {
				t.Fatalf("anonymous check status=%d", denied.Code)
			}
			cookie := dashboardTestCookie(t, handler)
			page := adminResponse(handler, http.MethodGet, "/admin/config", nil, cookie, "")
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `method="post" action="/admin/config/check"`) ||
				!strings.Contains(page.Body.String(), "Test saved GitHub auth") {
				t.Fatalf("config page lacks test button: status=%d body=%q", page.Code, page.Body.String())
			}
			wantGET := http.StatusNotFound
			if setup {
				wantGET = http.StatusForbidden
			}
			if got := adminResponse(handler, http.MethodGet, "/admin/config/check", nil, cookie, ""); got.Code != wantGET {
				t.Fatalf("GET check status=%d, want %d", got.Code, wantGET)
			}
			check := adminResponse(handler, http.MethodPost, "/admin/config/check", nil, cookie, "")
			if check.Code != http.StatusOK || check.Header().Get("Cache-Control") != "no-store" ||
				!strings.Contains(check.Body.String(), "Save a GitHub bot login and token before testing.") {
				t.Fatalf("saved config check: status=%d body=%q", check.Code, check.Body.String())
			}
			cfg.BotLogin = "saved-bot"
			cfg.GitHub.Token = "saved-token"
			data, err = json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			previousTransport := http.DefaultTransport
			calls := 0
			http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host != "api.github.com" || request.URL.Path != "/user" || request.Header.Get("Authorization") != "Bearer saved-token" {
					t.Errorf("unexpected GitHub request: %s, authorization=%q", request.URL, request.Header.Get("Authorization"))
				}
				response := httptest.NewRecorder()
				_, _ = response.WriteString(`{"id":42,"login":"saved-bot"}`)
				return response.Result(), nil
			})
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			check = adminResponse(handler, http.MethodPost, "/admin/config/check", nil, cookie, "")
			if calls != 1 || check.Code != http.StatusOK || !strings.Contains(check.Body.String(), "Authenticated as @saved-bot (ID 42).") {
				t.Fatalf("saved token check: calls=%d status=%d body=%q", calls, check.Code, check.Body.String())
			}
		})
	}
}

func TestGitHubAuthCheckAcceptsSavedBotIdentityWithoutActiveID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer saved-token" {
			t.Errorf("unexpected GitHub request: %s, authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":42,"login":"saved-bot"}`)
	}))
	t.Cleanup(server.Close)
	client := github.New("saved-token")
	client.BaseURL = server.URL
	bot := &action.Orchestrator{GitHub: client, BotLogin: "saved-bot"}
	if got := checkGitHubAuth(t.Context(), bot); !got.OK || !strings.Contains(got.Message, "@saved-bot") {
		t.Fatalf("saved bot check = %+v", got)
	}
	bot.BotLogin = "other-bot"
	if got := checkGitHubAuth(t.Context(), bot); got.OK || !strings.Contains(got.Message, "does not match") {
		t.Fatalf("mismatched saved bot check = %+v", got)
	}
}
