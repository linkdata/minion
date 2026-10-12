package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveSmoke(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "live-tests.local.json"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("live-tests.local.json is absent")
	}
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		OwnerLogin    string `json:"owner_login"`
		BotLogin      string `json:"bot_login"`
		RepositoryURL string `json:"repository_url"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.OwnerLogin == "" || cfg.BotLogin == "" || strings.EqualFold(cfg.OwnerLogin, cfg.BotLogin) {
		t.Fatal("live test requires two distinct GitHub accounts")
	}
	repoURL, err := url.Parse(cfg.RepositoryURL)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.Trim(repoURL.Path, "/"), "/")
	if repoURL.Scheme != "https" || repoURL.Host != "github.com" || repoURL.User != nil || repoURL.RawQuery != "" || repoURL.Fragment != "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" || !strings.EqualFold(parts[0], cfg.OwnerLogin) {
		t.Fatalf("invalid repository URL %q", cfg.RepositoryURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, login := range []string{cfg.OwnerLogin, cfg.BotLogin} {
		t.Run(login, func(t *testing.T) {
			got := liveAPIField(t, ctx, "users/"+url.PathEscape(login), ".login")
			if !strings.EqualFold(got, login) {
				t.Fatalf("GitHub user = %q, want %q", got, login)
			}
		})
	}
	t.Run("repository", func(t *testing.T) {
		want := strings.Join(parts, "/")
		got := liveAPIField(t, ctx, "repos/"+want, ".full_name")
		if !strings.EqualFold(got, want) {
			t.Fatalf("GitHub repository = %q, want %q", got, want)
		}
	})
}

func liveAPIField(t *testing.T, ctx context.Context, endpoint, field string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "gh", "api", "--hostname", "github.com", "--method", "GET", endpoint, "--jq", field)
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh api %s: %v: %s", endpoint, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output))
}
