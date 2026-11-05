package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAdminConfig(t *testing.T) {
	for _, test := range []struct {
		name, adminJSON, wantListen, wantCert, wantKey string
	}{
		{name: "omitted"},
		{name: "null", adminJSON: `,"admin":null`},
		{name: "defaults", adminJSON: `,"admin":{}`, wantListen: "127.0.0.1:8377"},
		{name: "relative TLS paths", adminJSON: `,"admin":{"listen":"localhost:8443","tls_cert_file":"certs/cert.pem","tls_key_file":"certs/key.pem"}`,
			wantListen: "localhost:8443", wantCert: "certs/cert.pem", wantKey: "certs/key.pem"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			data := `{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}]` + test.adminJSON + `}`
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantListen == "" {
				if cfg.Admin != nil {
					t.Fatalf("admin = %+v, want disabled", cfg.Admin)
				}
				return
			}
			if cfg.Admin == nil {
				t.Fatal("admin config missing")
			}
			wantCert, wantKey := test.wantCert, test.wantKey
			if wantCert != "" {
				wantCert = filepath.Join(dir, wantCert)
				wantKey = filepath.Join(dir, wantKey)
			}
			if cfg.Admin.Listen != test.wantListen || cfg.Admin.TLSCertFile != wantCert || cfg.Admin.TLSKeyFile != wantKey {
				t.Fatalf("admin = %+v, want listen=%q cert=%q key=%q", cfg.Admin, test.wantListen, wantCert, wantKey)
			}
		})
	}
}

func TestLoadAdminConfigPreservesRelativeSymlinkTLSPath(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip("symlink traversal semantics differ on Windows")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(filepath.Join(target, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "cert.pem"), []byte("target certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target, "sub"), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(root, "config.json")
	data := `{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}],"admin":{"listen":"localhost:8377","tls_cert_file":"link/../cert.pem","tls_key_file":"key.pem"}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := root + "/link/../cert.pem"
	if cfg.Admin == nil || cfg.Admin.TLSCertFile != want {
		t.Fatalf("TLS certificate path = %+v, want %q", cfg.Admin, want)
	}
	contents, err := os.ReadFile(cfg.Admin.TLSCertFile)
	if err != nil || string(contents) != "target certificate" {
		t.Fatalf("resolved TLS certificate = %q, %v", contents, err)
	}
}

func TestAdminConfigValidation(t *testing.T) {
	for _, test := range []struct {
		name, listen, cert, key, wantError string
	}{
		{name: "IPv4 loopback alias", listen: "127.0.0.2:8377", wantError: "without TLS must use 127.0.0.1"},
		{name: "IPv6 loopback", listen: "[::1]:8377", wantError: "without TLS must use 127.0.0.1"},
		{name: "mapped IPv4 loopback", listen: "[::ffff:127.0.0.1]:8377", wantError: "without TLS must use 127.0.0.1"},
		{name: "loopback with TLS", listen: "127.0.0.1:8377", cert: "cert.pem", key: "key.pem"},
		{name: "public IP with TLS", listen: "192.0.2.1:8377", cert: "cert.pem", key: "key.pem"},
		{name: "hostname with TLS", listen: "localhost:8377", cert: "cert.pem", key: "key.pem"},
		{name: "empty bracketed host", listen: "[]:8377", cert: "cert.pem", key: "key.pem", wantError: "empty bracketed host"},
		{name: "public IP without TLS", listen: "192.0.2.1:8377", wantError: "without TLS must use 127.0.0.1"},
		{name: "wildcard without TLS", listen: ":8377", wantError: "without TLS must use 127.0.0.1"},
		{name: "IPv4 wildcard without TLS", listen: "0.0.0.0:8377", wantError: "without TLS must use 127.0.0.1"},
		{name: "IPv6 wildcard without TLS", listen: "[::]:8377", wantError: "without TLS must use 127.0.0.1"},
		{name: "hostname without TLS", listen: "localhost:8377", wantError: "without TLS must use 127.0.0.1"},
		{name: "missing port", listen: "127.0.0.1", wantError: "admin.listen:"},
		{name: "empty port", listen: "127.0.0.1:", wantError: "numeric port"},
		{name: "non numeric port", listen: "127.0.0.1:http", wantError: "numeric port"},
		{name: "zero port", listen: "127.0.0.1:0", wantError: "numeric port"},
		{name: "port too large", listen: "127.0.0.1:65536", wantError: "numeric port"},
		{name: "signed port", listen: "127.0.0.1:+1", wantError: "numeric port"},
		{name: "certificate only", listen: "127.0.0.1:8377", cert: "cert.pem", wantError: "must both be set"},
		{name: "key only", listen: "127.0.0.1:8377", key: "key.pem", wantError: "must both be set"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{BotLogin: "bot", Admin: &AdminConfig{Listen: test.listen, TLSCertFile: test.cert, TLSKeyFile: test.key},
				Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
			err := cfg.Validate(t.TempDir())
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}
