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
		WebservCertDir: "/certs", WebservUser: "minion", WebservDataDir: "/data",
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
	if _, err := startAdminServerWithSetup(t.Context(), cfg, nil, "", nil, true); err == nil {
		t.Fatal("certificate without -listenurl hostname reached the listener")
	}
	cfg.WebservListenURL = ""
	cfg.GitHubOAuth.RedirectURL = "https://other.example/auth/github/callback"
	if _, err := startAdminServerWithSetup(t.Context(), cfg, nil, "", nil, true); err == nil {
		t.Fatal("certificate without callback hostname was accepted")
	}
}

func TestWebservTLSOverrideValidatesWithoutListener(t *testing.T) {
	cfg := config.Config{BotLogin: "bot", Main: "codex", Admin: &config.AdminConfig{Listen: "0.0.0.0:8443"},
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
}
