package config

import (
	"strings"
	"testing"
)

func TestOAuthClientConfigValidation(t *testing.T) {
	valid := func() Config {
		return Config{BotLogin: "bot", Admin: &AdminConfig{TLSCertFile: "cert.pem", TLSKeyFile: "key.pem", GitHubOAuth: &OAuthClient{
			ClientID: "github-client", ClientSecret: "github-secret", RedirectURL: "https://minion.example/auth/callback"},
			GitLabOAuth: &OAuthClient{ClientID: "gitlab-client", ClientSecret: "gitlab-secret",
				RedirectURL: "https://minion.example/auth/callback"}},
			Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
	}
	good := valid()
	if err := good.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if good.Admin.GitLabOAuth.BaseURL != "https://gitlab.com" {
		t.Fatalf("default GitLab origin = %q", good.Admin.GitLabOAuth.BaseURL)
	}
	loopback := valid()
	loopback.Admin.TLSCertFile, loopback.Admin.TLSKeyFile = "", ""
	loopback.Admin.GitHubOAuth.RedirectURL = "http://127.0.0.1:8377/auth/callback"
	loopback.Admin.GitLabOAuth.RedirectURL = "http://127.0.0.1:8377/auth/callback"
	if err := loopback.Validate(t.TempDir()); err != nil {
		t.Fatalf("loopback HTTP OAuth: %v", err)
	}
	defaultPort := valid()
	defaultPort.Admin.GitLabOAuth.BaseURL = "https://GITLAB.example:443"
	if err := defaultPort.Validate(t.TempDir()); err != nil || defaultPort.Admin.GitLabOAuth.BaseURL != "https://gitlab.example" {
		t.Fatalf("default GitLab port was not normalized: base=%q error=%v", defaultPort.Admin.GitLabOAuth.BaseURL, err)
	}
	paddedPort := valid()
	paddedPort.Admin.GitLabOAuth.BaseURL = "https://GITLAB.example:08443"
	if err := paddedPort.Validate(t.TempDir()); err != nil || paddedPort.Admin.GitLabOAuth.BaseURL != "https://gitlab.example:8443" {
		t.Fatalf("padded GitLab port was not normalized: base=%q error=%v", paddedPort.Admin.GitLabOAuth.BaseURL, err)
	}
	punycode := valid()
	punycode.Admin.GitLabOAuth.BaseURL = "https://XN--BCHER-KVA.EXAMPLE:443"
	punycode.Admin.GitHubOAuth.RedirectURL = "https://xn--bcher-kva.example/auth/callback"
	if err := punycode.Validate(t.TempDir()); err != nil || punycode.Admin.GitLabOAuth.BaseURL != "https://xn--bcher-kva.example" {
		t.Fatalf("ASCII IDN host was not normalized: base=%q error=%v", punycode.Admin.GitLabOAuth.BaseURL, err)
	}
	ipv6 := valid()
	ipv6.Admin.GitLabOAuth.BaseURL = "https://[::1]:8443"
	ipv6.Admin.GitHubOAuth.RedirectURL = "https://[::1]/auth/callback"
	if err := ipv6.Validate(t.TempDir()); err != nil || ipv6.Admin.GitLabOAuth.BaseURL != "https://[::1]:8443" {
		t.Fatalf("plain IPv6 GitLab origin was rejected: base=%q error=%v", ipv6.Admin.GitLabOAuth.BaseURL, err)
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
		{"Unicode callback host", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL = "https://bücher.example/auth/callback" }, "redirect_url"},
		{"scoped IPv6 callback host", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL = "https://[fe80::1%25Eth0]/auth/callback" }, "redirect_url"},
		{"empty callback port", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL = "https://minion.example:/auth/callback" }, "redirect_url"},
		{"public HTTP callback", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL = "http://minion.example/auth/callback" }, "redirect_url"},
		{"TLS listener with HTTP callback", func(c *Config) { c.Admin.GitLabOAuth.RedirectURL = "http://127.0.0.1:8377/auth/callback" }, "admin TLS"},
		{"plain listener with HTTPS callback", func(c *Config) { c.Admin.TLSCertFile, c.Admin.TLSKeyFile = "", "" }, "admin TLS"},
		{"GitHub endpoint override", func(c *Config) { c.Admin.GitHubOAuth.BaseURL = "https://github.example" }, "base_url"},
		{"GitLab HTTP origin", func(c *Config) { c.Admin.GitLabOAuth.BaseURL = "http://gitlab.example" }, "base_url"},
		{"GitLab path origin", func(c *Config) { c.Admin.GitLabOAuth.BaseURL = "https://gitlab.example/subpath" }, "base_url"},
		{"Unicode GitLab host", func(c *Config) { c.Admin.GitLabOAuth.BaseURL = "https://bücher.example" }, "base_url"},
		{"scoped IPv6 GitLab host", func(c *Config) { c.Admin.GitLabOAuth.BaseURL = "https://[fe80::1%25Eth0]" }, "base_url"},
		{"empty GitLab port", func(c *Config) { c.Admin.GitLabOAuth.BaseURL = "https://gitlab.example:" }, "base_url"},
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
