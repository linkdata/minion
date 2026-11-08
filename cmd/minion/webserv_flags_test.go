package main

import (
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
		service.User != cfg.WebservUser || service.DataDir != cfg.WebservDataDir ||
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
	const publicURL = "http://minion.example/setup"
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
