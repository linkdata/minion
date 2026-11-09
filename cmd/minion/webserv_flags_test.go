package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		{[]string{"-mode=execute", "-user="}, "webserv flags require orchestrate mode"},
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

func TestWebservFlagsRejectUnusedWithoutListener(t *testing.T) {
	for _, shortcut := range []string{"-oneshot", "-install-skills"} {
		for _, option := range []string{"-address=", "-certdir=", "-datadir=", "-listenurl="} {
			code, output := runStartupFixture(t, config.Config{}, shortcut, option)
			if code != 2 || !strings.Contains(output, "cannot be combined with -oneshot or -install-skills") {
				t.Fatalf("%s accepted %s: exit=%d log=%q", shortcut, option, code, output)
			}
		}
	}
	code, output := runStartupFixture(t, config.Config{}, "-check", "-datadir=")
	if code != 2 || !strings.Contains(output, "-datadir requires a persistent admin listener") {
		t.Fatalf("-check accepted -datadir: exit=%d log=%q", code, output)
	}
}

func TestWebservUserSwitchWithoutAdminListener(t *testing.T) {
	cfg := config.Config{BotLogin: "bot", Main: "codex", Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
		StatePath:    filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir()}
	for _, args := range [][]string{{"-oneshot", "-user="}, {"-check", "-user="}, {"-user="}} {
		code, output := runStartupFixtureWithRun(t, cfg, func() int {
			return runWithGitCheck(func(context.Context, string) error { return errors.New("stop after config") })
		}, args...)
		if code != 1 || !strings.Contains(output, "Git preflight: stop after config") {
			t.Fatalf("-user without admin listener %v: exit=%d log=%q", args, code, output)
		}
	}
	code, output := runStartupFixture(t, cfg, "-install-skills", "-user=minion-no-such-user-!")
	if code != 2 || !strings.Contains(output, "switch webserv user:") {
		t.Fatalf("-install-skills rejected -user before switching: exit=%d log=%q", code, output)
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

func TestAdminServerUsesSpecificLoopbackURL(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 is unavailable: %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	server, err := startAdminServerWithSetup(t.Context(), config.AdminConfig{Listen: "127.0.0.2:0"}, nil, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close setup server: %v", err)
		}
	})
	if want := "http://" + server.listener.Addr().String(); server.entryURL != want {
		t.Fatalf("specific loopback URL = %q, want %q", server.entryURL, want)
	}
}

func TestAdminServerUsesHTTPOAuthCallbackOrigin(t *testing.T) {
	address := freeAdminListen(t)
	want := "http://" + address
	server, err := startAdminServerWithSetup(t.Context(), config.AdminConfig{
		Listen: address, GitHubOAuth: &config.OAuthClient{ClientID: "app", ClientSecret: "secret", RedirectURL: want + "/auth/github/callback"},
	}, nil, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if server.entryURL != want {
		t.Fatalf("HTTP OAuth entry URL = %q, want %q", server.entryURL, want)
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
	want := "https://" + leaf.IPAddresses[0].String()
	cfg := config.AdminConfig{Listen: "127.0.0.1:0", WebservCertDir: dir,
		GitHubOAuth: &config.OAuthClient{RedirectURL: want + "/auth/github/callback"}}
	server, err := startAdminServerWithSetup(t.Context(), cfg, nil, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if server.entryURL != want {
		t.Fatalf("Webserv entry URL = %q, want callback origin %q", server.entryURL, want)
	}
	cfg.WebservListenURL = want
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

func TestWebservCertificateSkipsMalformedDNSName(t *testing.T) {
	for _, badName := range []string{"bad..example", "bad%example"} {
		t.Run(badName, func(t *testing.T) {
			certDir := testAdminDNSCertificate(t, badName, "good.example")
			cfg := config.AdminConfig{Listen: "127.0.0.1:0", WebservCertDir: certDir}
			if err := checkAdminServerConfig(cfg); err != nil {
				t.Fatal(err)
			}
			server, err := startAdminServerWithSetup(t.Context(), cfg, nil, "", nil, true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			_, port, err := net.SplitHostPort(server.listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			if want := "https://good.example:" + port; server.entryURL != want {
				t.Fatalf("certified URL = %q, want %q", server.entryURL, want)
			}

			cfg.WebservListenURL = "https://" + badName
			if err := checkAdminServerConfig(cfg); err == nil {
				t.Fatal("malformed explicit -listenurl passed preflight")
			}
			cfg.WebservListenURL = ""
			cfg.WebservCertDir = testAdminDNSCertificate(t, badName)
			if server, err := startAdminServerWithSetup(t.Context(), cfg, nil, "", nil, true); err == nil {
				_ = server.Close()
				t.Fatal("malformed only certificate name was advertised")
			}
		})
	}
}

func testAdminDNSCertificate(t *testing.T, names ...string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: names,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fullchain.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "privkey.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWebservFlagsAcceptedForServiceAndCheck(t *testing.T) {
	cfg := config.Config{BotLogin: "bot", GitHub: config.GitHubConfig{Token: "test-token"}, Main: "codex", Admin: &config.AdminConfig{Listen: "0.0.0.0:8443", Administrators: []config.AdminAccount{{Username: "admin"}}},
		Commands: map[string]config.CommandMapping{}, AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}},
		StatePath:    filepath.Join(t.TempDir(), "state.json"), WorkRoot: t.TempDir()}
	certDir := t.TempDir() // Git preflight stops before certificate file checks.
	for _, args := range [][]string{
		{"-check", "-address", "0.0.0.0:8443", "-certdir", certDir, "-listenurl", "https://127.0.0.1:8443"},
		{"-address", "0.0.0.0:8443", "-certdir", certDir, "-datadir", t.TempDir(), "-listenurl", "https://127.0.0.1:8443"},
	} {
		code, output := runStartupFixtureWithRun(t, cfg, func() int {
			return runWithGitCheck(func(context.Context, string) error { return errors.New("stop after config") })
		}, args...)
		if code != 1 || !strings.Contains(output, "Git preflight: stop after config") || strings.Contains(output, "config:") {
			t.Fatalf("rejected valid webserv flags %v: exit=%d log=%q", args, code, output)
		}
	}
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error { return nil })
	}, "-check", "-address", "0.0.0.0:8443", "-certdir", certDir, "-listenurl", "https://127.0.0.1:8443")
	if code != 1 || !strings.Contains(output, "admin TLS preflight:") || !strings.Contains(output, "fullchain.pem") {
		t.Fatalf("-check skipped webserv TLS preflight: exit=%d log=%q", code, output)
	}
	code, output = runStartupFixture(t, cfg, "-check", "-address", "127.0.0.1:99999")
	if code != 2 || !strings.Contains(output, "-address port") {
		t.Fatalf("-check accepted invalid webserv port: exit=%d log=%q", code, output)
	}
	code, output = runStartupFixture(t, cfg, "-check", "-address", "bad/address", "-certdir", certDir)
	if code != 2 || !strings.Contains(output, "-address contains invalid host characters") {
		t.Fatalf("-check accepted invalid webserv host: exit=%d log=%q", code, output)
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
