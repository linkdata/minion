package config

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestWebservAddressBracketValidation(t *testing.T) {
	for _, test := range []struct {
		address string
		tls     bool
		valid   bool
	}{
		{"[::1", false, false},
		{"127.0.0.1:bad:port", true, false},
		{"[]", true, false},
		{"[]:9000", true, false},
		{"[localhost]", true, false},
		{"[::1]", false, true},
		{"::1", false, true},
		{"[::1]:9000", false, true},
		{"[localhost]:9000", true, true},
	} {
		t.Run(test.address, func(t *testing.T) {
			cfg := Config{BotLogin: "bot", Admin: &AdminConfig{WebservAddress: test.address}}
			if test.tls {
				cfg.Admin.WebservCertDir = "/certs"
			}
			err := cfg.Validate(t.TempDir())
			if (err == nil) != test.valid {
				t.Fatalf("-address %q validity=%t: %v", test.address, test.valid, err)
			}
		})
	}
}

func TestOAuthClientConfigValidation(t *testing.T) {
	valid := func() Config {
		return Config{BotLogin: "bot", Admin: &AdminConfig{TLSCertFile: "cert.pem", TLSKeyFile: "key.pem", GitHubOAuth: &OAuthClient{
			ClientID: "github-client", ClientSecret: "github-secret", RedirectURL: "https://minion.example/auth/github/callback"},
			GitLabOAuth: &OAuthClient{ClientID: "gitlab-client", ClientSecret: "gitlab-secret",
				RedirectURL: "https://minion.example/auth/gitlab/callback"}},
			Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
	}
	good := valid()
	if err := good.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if good.Admin.GitLabOAuth.BaseURL != "https://gitlab.com" {
		t.Fatalf("default GitLab origin = %q", good.Admin.GitLabOAuth.BaseURL)
	}
	caseFold := valid()
	caseFold.Admin.GitLabOAuth.RedirectURL = "https://MINION.EXAMPLE/auth/gitlab/callback"
	if err := caseFold.Validate(t.TempDir()); err != nil {
		t.Fatalf("same callback host with different case: %v", err)
	}
	localhost := valid()
	localhost.Admin.TLSCertFile, localhost.Admin.TLSKeyFile = "", ""
	localhost.Admin.GitHubOAuth.RedirectURL = "http://localhost:8377/auth/github/callback"
	localhost.Admin.GitLabOAuth.RedirectURL = "http://localhost:8377/auth/gitlab/callback"
	if err := localhost.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "redirect_url") {
		t.Fatalf("localhost HTTP OAuth: %v, want redirect_url error", err)
	}
	for _, tc := range []struct{ listen, host string }{
		{"127.0.0.1:8377", "127.0.0.1"},
		{"127.0.0.2:8377", "127.0.0.2"},
		{"[::1]:8377", "[::1]"},
		{"[::ffff:127.0.0.1]:8377", "127.0.0.1"},
	} {
		loopback := valid()
		loopback.Admin.Listen = tc.listen
		loopback.Admin.TLSCertFile, loopback.Admin.TLSKeyFile = "", ""
		loopback.Admin.GitHubOAuth.RedirectURL = "http://" + tc.host + ":8377/auth/github/callback"
		loopback.Admin.GitLabOAuth.RedirectURL = "http://" + tc.host + ":8377/auth/gitlab/callback"
		if err := loopback.Validate(t.TempDir()); err != nil {
			t.Errorf("loopback HTTP OAuth on %s: %v", tc.listen, err)
		}
	}
	equivalentIPv6 := valid()
	equivalentIPv6.Admin.Listen = "[::1]:8377"
	equivalentIPv6.Admin.TLSCertFile, equivalentIPv6.Admin.TLSKeyFile = "", ""
	equivalentIPv6.Admin.GitHubOAuth.RedirectURL = "http://[::1]:8377/auth/github/callback"
	equivalentIPv6.Admin.GitLabOAuth.RedirectURL = "http://[0:0::1]:8377/auth/gitlab/callback"
	if err := equivalentIPv6.Validate(t.TempDir()); err != nil {
		t.Fatalf("equivalent IPv6 callback hosts: %v", err)
	}
	equivalentIPv6Origin := valid()
	equivalentIPv6Origin.Admin.WebservListenURL = "https://[0:0::1]"
	equivalentIPv6Origin.Admin.GitHubOAuth.RedirectURL = "https://[::1]/auth/github/callback"
	equivalentIPv6Origin.Admin.GitLabOAuth.RedirectURL = "https://[::1]/auth/gitlab/callback"
	if err := equivalentIPv6Origin.Validate(t.TempDir()); err != nil {
		t.Fatalf("equivalent IPv6 entry and callback origins: %v", err)
	}
	equivalentIPv6Origin.Admin.WebservListenURL = "https://[::2]"
	if err := equivalentIPv6Origin.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "origin must match") {
		t.Fatalf("different IPv6 entry and callback origins: %v", err)
	}
	for _, tc := range []struct{ listen, host string }{
		{"127.0.0.1:8377", "127.0.0.2"},
		{"127.0.0.1:8377", "[::1]"},
		{"[::ffff:127.0.0.1]:8377", "127.0.0.2"},
	} {
		unbound := valid()
		unbound.Admin.Listen = tc.listen
		unbound.Admin.TLSCertFile, unbound.Admin.TLSKeyFile = "", ""
		unbound.Admin.GitHubOAuth.RedirectURL = "http://" + tc.host + ":8377/auth/github/callback"
		unbound.Admin.GitLabOAuth.RedirectURL = "http://" + tc.host + ":8377/auth/gitlab/callback"
		if err := unbound.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "HTTP host must match the admin listener") {
			t.Errorf("unbound HTTP callback %s for %s: %v", tc.host, tc.listen, err)
		}
	}
	defaultHTTPPort := valid()
	defaultHTTPPort.Admin.Listen = "127.0.0.1:80"
	defaultHTTPPort.Admin.TLSCertFile, defaultHTTPPort.Admin.TLSKeyFile = "", ""
	defaultHTTPPort.Admin.GitHubOAuth.RedirectURL = "http://127.0.0.1/auth/github/callback"
	defaultHTTPPort.Admin.GitLabOAuth.RedirectURL = "http://127.0.0.1/auth/gitlab/callback"
	if err := defaultHTTPPort.Validate(t.TempDir()); err != nil {
		t.Fatalf("implicit HTTP port 80: %v", err)
	}
	namedHTTPPort := valid()
	namedHTTPPort.Admin.TLSCertFile, namedHTTPPort.Admin.TLSKeyFile = "", ""
	namedHTTPPort.Admin.WebservAddress = "127.0.0.1:http"
	namedHTTPPort.Admin.GitHubOAuth.RedirectURL = "http://127.0.0.1/auth/github/callback"
	namedHTTPPort.Admin.GitLabOAuth.RedirectURL = "http://127.0.0.1/auth/gitlab/callback"
	if err := namedHTTPPort.Validate(t.TempDir()); err != nil {
		t.Fatalf("named Webserv port with matching callback: %v", err)
	}
	namedHTTPPort.Admin.GitHubOAuth.RedirectURL = "http://127.0.0.1:81/auth/github/callback"
	if err := namedHTTPPort.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "HTTP port must match") {
		t.Fatalf("named Webserv port with wrong callback: %v", err)
	}
	webservPort := 8080
	if os.Geteuid() == 0 {
		webservPort = 80
	}
	for _, test := range []struct{ address, host string }{
		{"127.0.0.1", "127.0.0.1"},
		{"[::1]", "[::1]"},
		{"::1", "[::1]"},
	} {
		bareHost := valid()
		bareHost.Admin.TLSCertFile, bareHost.Admin.TLSKeyFile = "", ""
		bareHost.Admin.WebservAddress = test.address
		bareHost.Admin.GitHubOAuth.RedirectURL = "http://" + test.host + ":" + strconv.Itoa(webservPort) + "/auth/github/callback"
		bareHost.Admin.GitLabOAuth.RedirectURL = "http://" + test.host + ":" + strconv.Itoa(webservPort) + "/auth/gitlab/callback"
		if err := bareHost.Validate(t.TempDir()); err != nil {
			t.Errorf("bare Webserv address %q with matching callback: %v", test.address, err)
		}
		bareHost.Admin.GitHubOAuth.RedirectURL = "http://" + test.host + ":" + strconv.Itoa(webservPort+1) + "/auth/github/callback"
		if err := bareHost.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "HTTP port must match") {
			t.Errorf("bare Webserv address %q with wrong callback port: %v", test.address, err)
		}
	}
	proxiedHTTPS := valid()
	proxiedHTTPS.Admin.TLSCertFile, proxiedHTTPS.Admin.TLSKeyFile = "", ""
	if err := proxiedHTTPS.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "requires TLS") {
		t.Fatalf("HTTPS callback without listener TLS: %v", err)
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
	punycode.Admin.GitHubOAuth.RedirectURL = "https://xn--bcher-kva.example/auth/github/callback"
	punycode.Admin.GitLabOAuth.RedirectURL = "https://xn--bcher-kva.example/auth/gitlab/callback"
	if err := punycode.Validate(t.TempDir()); err != nil || punycode.Admin.GitLabOAuth.BaseURL != "https://xn--bcher-kva.example" {
		t.Fatalf("ASCII IDN host was not normalized: base=%q error=%v", punycode.Admin.GitLabOAuth.BaseURL, err)
	}
	ipv6 := valid()
	ipv6.Admin.GitLabOAuth.BaseURL = "https://[::1]:8443"
	ipv6.Admin.GitHubOAuth.RedirectURL = "https://[::1]/auth/github/callback"
	ipv6.Admin.GitLabOAuth.RedirectURL = "https://[::1]/auth/gitlab/callback"
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
		{"shared provider callback", func(c *Config) { c.Admin.GitLabOAuth.RedirectURL = c.Admin.GitHubOAuth.RedirectURL }, "redirect_url"},
		{"callback query", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL += "?next=other" }, "redirect_url"},
		{"empty callback query", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL += "?" }, "redirect_url"},
		{"empty callback fragment", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL += "#" }, "redirect_url"},
		{"Unicode callback host", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL = "https://bücher.example/auth/github/callback" }, "redirect_url"},
		{"scoped IPv6 callback host", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL = "https://[fe80::1%25Eth0]/auth/github/callback" }, "redirect_url"},
		{"empty callback port", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL = "https://minion.example:/auth/github/callback" }, "redirect_url"},
		{"public HTTP callback", func(c *Config) { c.Admin.GitHubOAuth.RedirectURL = "http://minion.example/auth/github/callback" }, "redirect_url"},
		{"mixed HTTPS callback hosts", func(c *Config) {
			c.Admin.GitLabOAuth.RedirectURL = "https://other.example/auth/gitlab/callback"
		}, "hosts must match"},
		{"callback differs from entry URL", func(c *Config) {
			c.Admin.WebservListenURL = "https://other.example"
		}, "origin must match"},
		{"HTTP callback differs from effective address", func(c *Config) {
			c.Admin.TLSCertFile, c.Admin.TLSKeyFile = "", ""
			c.Admin.WebservAddress = "127.0.0.1:9000"
			c.Admin.GitHubOAuth.RedirectURL = "http://127.0.0.1:8377/auth/github/callback"
			c.Admin.GitLabOAuth.RedirectURL = "http://127.0.0.1:8377/auth/gitlab/callback"
		}, "port must match"},
		{"mixed HTTP callback hosts", func(c *Config) {
			c.Admin.TLSCertFile, c.Admin.TLSKeyFile = "", ""
			c.Admin.GitHubOAuth.RedirectURL = "http://127.0.0.1:8377/auth/github/callback"
			c.Admin.GitLabOAuth.RedirectURL = "http://[::ffff:127.0.0.1]:8377/auth/gitlab/callback"
		}, "hosts must match"},
		{"loopback IP callback wrong port", func(c *Config) {
			c.Admin.TLSCertFile, c.Admin.TLSKeyFile = "", ""
			c.Admin.GitHubOAuth.RedirectURL = "http://127.0.0.1:8378/auth/github/callback"
			c.Admin.GitLabOAuth.RedirectURL = "http://127.0.0.1:8377/auth/gitlab/callback"
		}, "port must match"},
		{"implicit HTTP callback port mismatch", func(c *Config) {
			c.Admin.TLSCertFile, c.Admin.TLSKeyFile = "", ""
			c.Admin.GitHubOAuth.RedirectURL = "http://127.0.0.1/auth/github/callback"
		}, "port must match"},
		{"localhost suffix HTTP callback", func(c *Config) {
			c.Admin.TLSCertFile, c.Admin.TLSKeyFile = "", ""
			c.Admin.GitHubOAuth.RedirectURL = "http://localhost.example/auth/github/callback"
		}, "redirect_url"},
		{"TLS listener with HTTP callback", func(c *Config) { c.Admin.GitLabOAuth.RedirectURL = "http://127.0.0.1:8377/auth/gitlab/callback" }, "must use HTTPS"},
		{"public listener without TLS", func(c *Config) {
			c.Admin.Listen = "0.0.0.0:8377"
			c.Admin.TLSCertFile, c.Admin.TLSKeyFile = "", ""
			c.Admin.GitHubOAuth.RedirectURL = "http://127.0.0.1:8377/auth/github/callback"
		}, "outside literal loopback"},
		{"GitHub endpoint override", func(c *Config) { c.Admin.GitHubOAuth.BaseURL = "https://github.example" }, "base_url"},
		{"GitLab HTTP origin", func(c *Config) { c.Admin.GitLabOAuth.BaseURL = "http://gitlab.example" }, "base_url"},
		{"GitLab path origin", func(c *Config) { c.Admin.GitLabOAuth.BaseURL = "https://gitlab.example/subpath" }, "base_url"},
		{"empty GitLab query", func(c *Config) { c.Admin.GitLabOAuth.BaseURL = "https://gitlab.example?" }, "base_url"},
		{"empty GitLab fragment", func(c *Config) { c.Admin.GitLabOAuth.BaseURL = "https://gitlab.example#" }, "base_url"},
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
