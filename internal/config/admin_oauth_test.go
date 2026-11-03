package config

import (
	"strings"
	"testing"
)

func TestOAuthClientConfigValidation(t *testing.T) {
	valid := func() Config {
		return Config{BotLogin: "bot", Admin: &AdminConfig{GitHubOAuth: &OAuthClient{
			ClientID: "github-client", ClientSecret: "github-secret", RedirectURL: "https://minion.example/auth/callback"},
			GitLabOAuth: &OAuthClient{ClientID: "gitlab-client", ClientSecret: "gitlab-secret",
				RedirectURL: "http://127.0.0.1:8377/auth/callback"}},
			Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
	}
	good := valid()
	if err := good.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if good.Admin.GitLabOAuth.BaseURL != "https://gitlab.com" {
		t.Fatalf("default GitLab origin = %q", good.Admin.GitLabOAuth.BaseURL)
	}
	for _, test := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"missing ID", func(c *Config) { c.Admin.GitHubOAuth.ClientID = "" }, "client_id"},
		{"missing secret", func(c *Config) { c.Admin.GitHubOAuth.ClientSecret = "" }, "client_secret"},
		{"callback on another path", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL = "https://minion.example/other" }, "redirect_url"},
		{"callback query", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL += "?next=other" }, "redirect_url"},
		{"public HTTP callback", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL = "http://minion.example/auth/callback" }, "redirect_url"},
		{"GitHub endpoint override", func(c *Config) { c.Admin.GitHubOAuth.BaseURL = "https://github.example" }, "base_url"},
		{"GitLab HTTP origin", func(c *Config) { c.Admin.GitLabOAuth.BaseURL = "http://gitlab.example" }, "base_url"},
		{"GitLab path origin", func(c *Config) { c.Admin.GitLabOAuth.BaseURL = "https://gitlab.example/subpath" }, "base_url"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid()
			test.edit(&cfg)
			if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}
