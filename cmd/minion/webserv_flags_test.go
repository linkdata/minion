package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestAdminWebservConfigUsesCLIOverrides(t *testing.T) {
	cfg := config.AdminConfig{
		Listen: "127.0.0.1:8377", WebservAddress: "127.0.0.1:9000",
		WebservCertDir: "/certs", WebservDataDir: "/data",
		WebservListenURL: "https://minion.example",
	}
	service := adminWebservConfig(cfg)
	if service.Address != cfg.WebservAddress || service.CertDir != cfg.WebservCertDir ||
		service.User != "" || service.DataDir != cfg.WebservDataDir ||
		service.ListenURL != cfg.WebservListenURL {
		t.Fatalf("webserv options not passed through: %+v", service)
	}
	cfg = config.AdminConfig{Listen: "127.0.0.1:8377"}
	if service := adminWebservConfig(cfg); service.Address != cfg.Listen || service.CertDir != "" || service.User != "" || service.DataDir != "" || service.ListenURL != "" {
		t.Fatalf("webserv defaults changed: %+v", service)
	}
	port := "8443"
	if os.Geteuid() == 0 {
		port = "443"
	}
	for _, test := range []struct{ address, want string }{
		{"127.0.0.1", "127.0.0.1:" + port},
		{"[::1]", "[::1]:" + port},
		{"127.0.0.1:0", "127.0.0.1:0"},
	} {
		cfg = config.AdminConfig{WebservAddress: test.address, TLSCertFile: "cert.pem", TLSKeyFile: "key.pem"}
		if got := adminWebservConfig(cfg).Address; got != test.want {
			t.Errorf("legacy TLS address %q = %q, want %q", test.address, got, test.want)
		}
	}
}

func TestWebservFlagsRejectExplicitEmptyOutsideAdminService(t *testing.T) {
	cfg := config.Config{BotLogin: "bot", Main: "codex", Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
		StatePath:    filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir()}
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"-mode=execute", "-address="}, "webserv flags require orchestrate mode"},
		{[]string{"-certdir="}, "webserv flags require an admin block"},
	} {
		code, output := runStartupFixtureWithRun(t, cfg, func() int {
			return runWithGitCheck(func(context.Context, string) error {
				t.Fatal("webserv flag reached Git preflight")
				return nil
			})
		}, test.args...)
		if code != 2 || !strings.Contains(output, test.want) {
			t.Fatalf("explicit empty webserv flag %v: exit=%d log=%q", test.args, code, output)
		}
	}
}

func TestWebservOverrideRequiresTLSOutsideLoopback(t *testing.T) {
	for _, address := range []string{":9000", "0.0.0.0:9000", "example.test:9000", "localhost:9000"} {
		cfg := config.AdminConfig{WebservAddress: address}
		if err := validateWebservAdminAddress(cfg); err == nil || !strings.Contains(err.Error(), "requires a certificate") {
			t.Fatalf("public HTTP address %q accepted: %v", address, err)
		}
	}
	for _, address := range []string{"127.0.0.1:9000", "[::1]:9000", "127.0.0.1"} {
		if err := validateWebservAdminAddress(config.AdminConfig{WebservAddress: address}); err != nil {
			t.Fatalf("loopback address %q rejected: %v", address, err)
		}
	}
	for _, address := range []string{"127.0.0.1:99999", "127.0.0.1:-1"} {
		if err := validateWebservAdminAddress(config.AdminConfig{WebservAddress: address}); err == nil || !strings.Contains(err.Error(), "-address port") {
			t.Fatalf("invalid port in %q accepted: %v", address, err)
		}
	}
	if err := validateWebservAdminAddress(config.AdminConfig{WebservAddress: ":9000", WebservCertDir: "/certs"}); err != nil {
		t.Fatalf("TLS address rejected: %v", err)
	}
}

func TestSetupServerUsesWebservListenURL(t *testing.T) {
	const publicURL = "http://127.0.0.1:9000"
	server, err := startAdminServerWithSetup(t.Context(), config.AdminConfig{
		Listen: "127.0.0.1:8377", WebservAddress: "127.0.0.1:0", WebservListenURL: publicURL,
	}, nil, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close setup server: %v", err)
		}
	})
	if server.entryURL != publicURL {
		t.Fatalf("advertised URL = %q, want %q", server.entryURL, publicURL)
	}
}

func TestAdminListenerRejectsUnsafeWebservOptions(t *testing.T) {
	for _, cfg := range []config.AdminConfig{
		{Listen: "127.0.0.1:8377", WebservListenURL: "http://public.example"},
		{Listen: "127.0.0.1:8377", WebservListenURL: "https://public.example"},
		{Listen: "127.0.0.1:8377", WebservListenURL: "http://localhost/setup"},
		{Listen: "127.0.0.1:8377", TLSCertFile: "cert.pem", TLSKeyFile: "key.pem", WebservListenURL: "http://localhost"},
	} {
		if err := validateWebservAdminAddress(cfg); err == nil {
			t.Errorf("unsafe listener options accepted: %+v", cfg)
		}
	}
	t.Setenv("MINION_TEST_EMPTY_CERTDIR", "")
	cfg := config.AdminConfig{Listen: "127.0.0.1:8377", WebservAddress: ":9000", WebservCertDir: "$MINION_TEST_EMPTY_CERTDIR"}
	if err := checkAdminServerConfig(cfg); err == nil {
		t.Fatal("empty expanded certificate directory passed preflight")
	}
	if server, err := startAdminServerWithSetup(t.Context(), cfg, nil, "", nil, true); err == nil {
		_ = server.Close()
		t.Fatal("empty expanded certificate directory reached the listener")
	}
}

func TestWebservCertificateUsesOAuthCallbackURL(t *testing.T) {
	certFile, keyFile, leaf := remoteTestTLSFiles(t)
	dir := filepath.Dir(certFile)
	if err := os.Rename(certFile, filepath.Join(dir, "fullchain.pem")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(keyFile, filepath.Join(dir, "privkey.pem")); err != nil {
		t.Fatal(err)
	}
	cfg := config.AdminConfig{Listen: "127.0.0.1:0", WebservCertDir: dir,
		GitHubOAuth: &config.OAuthClient{RedirectURL: "https://" + leaf.DNSNames[0] + "/auth/github/callback"}}
	server, err := startAdminServerWithSetup(t.Context(), cfg, nil, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if want := "https://" + leaf.DNSNames[0]; server.entryURL != want {
		t.Fatalf("Webserv entry URL = %q, want callback origin %q", server.entryURL, want)
	}
	cfg.WebservListenURL = "https://" + leaf.DNSNames[0]
	if err := checkAdminServerConfig(cfg); err != nil {
		t.Fatalf("certificate rejected matching -listenurl: %v", err)
	}
	cfg.WebservListenURL = "https://other.example"
	if err := checkAdminServerConfig(cfg); err == nil {
		t.Fatal("certificate without -listenurl hostname passed preflight")
	}
	if server, err := startAdminServerWithSetup(t.Context(), cfg, nil, "", nil, true); err == nil {
		_ = server.Close()
		t.Fatal("certificate without -listenurl hostname reached the listener")
	}
	cfg.WebservListenURL = ""
	cfg.GitHubOAuth.RedirectURL = "https://other.example/auth/github/callback"
	if err := checkAdminServerConfig(cfg); err == nil || !strings.Contains(err.Error(), "OAuth callback host") {
		t.Fatalf("certificate without callback hostname passed preflight: %v", err)
	}
	if server, err := startAdminServerWithSetup(t.Context(), cfg, nil, "", nil, true); err == nil {
		_ = server.Close()
		t.Fatal("certificate without callback hostname was accepted")
	}
}

func TestWebservTLSOverrideValidatesWithoutListener(t *testing.T) {
	cfg := config.Config{BotLogin: "bot", Main: "codex", Admin: &config.AdminConfig{Listen: "0.0.0.0:8443", Administrators: []config.AdminAccount{{Username: "admin"}}},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
		StatePath:    filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir()}
	certDir := t.TempDir() // No certificate is needed without a listener.
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error { return errors.New("stop after config") })
	}, "-oneshot", "-certdir", certDir)
	if code != 1 || !strings.Contains(output, "Git preflight: stop after config") || strings.Contains(output, "config:") {
		t.Fatalf("oneshot runtime TLS validation: exit=%d log=%q", code, output)
	}
	code, output = runStartupFixture(t, cfg, "-install-skills", "-certdir", certDir)
	if code != 0 || strings.Contains(output, "config:") {
		t.Fatalf("install-skills runtime TLS validation: exit=%d log=%q", code, output)
	}
	code, output = runStartupFixture(t, cfg, "-check", "-address", "127.0.0.1:99999")
	if code != 2 || !strings.Contains(output, "-address port") {
		t.Fatalf("-check accepted invalid webserv port: exit=%d log=%q", code, output)
	}
}

func TestCheckRejectsUncoveredOAuthCallbackHost(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	certFile, keyFile, _ := remoteTestTLSFiles(t)
	cfg := config.Config{BotLogin: "bot", Main: "codex", Admin: &config.AdminConfig{
		TLSCertFile: certFile, TLSKeyFile: keyFile, Administrators: []config.AdminAccount{{Username: "admin"}}, GitHubOAuth: &config.OAuthClient{
			ClientID: "id", ClientSecret: "secret", RedirectURL: "https://other.example/auth/github/callback"}},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
		StatePath:    filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir()}
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error { return nil })
	}, "-check")
	if code != 1 || !strings.Contains(output, "admin TLS preflight:") || !strings.Contains(output, "OAuth callback host") {
		t.Fatalf("-check accepted uncovered callback: code=%d output=%q", code, output)
	}
}
