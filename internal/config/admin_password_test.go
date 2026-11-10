package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func testAdminPasswordHash(password string) string {
	salt := []byte("0123456789abcdef")
	tag := argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(tag))
}

func TestAdminPasswordHash(t *testing.T) {
	hash := testAdminPasswordHash("correct horse battery staple")
	salt := base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef"))
	account := AdminAccount{Username: "admin", PasswordHash: hash}
	if !account.VerifyPassword("correct horse battery staple") || account.VerifyPassword("wrong") {
		t.Fatal("Argon2id password verification changed")
	}
	for i, value := range []string{
		"plaintext", strings.Replace(hash, "argon2id", "argon2i", 1),
		strings.Replace(hash, "m=19456", "m=1", 1),
		strings.Replace(hash, "m=19456", "m=1000000", 1),
		strings.Replace(hash, "t=2", "t=1", 1),
		strings.Replace(hash, "p=1", "p=255", 1),
		strings.Replace(hash, "v=19", "v=16", 1),
		strings.Replace(hash, salt, "?", 1),
	} {
		if _, ok := parseAdminPasswordHash(value); ok || (AdminAccount{PasswordHash: value}).VerifyPassword("correct horse battery staple") {
			t.Fatalf("accepted malformed or unsafe hash case %d", i)
		}
	}
}

func TestAdminPasswordLengthAndUTF8(t *testing.T) {
	for _, password := range []string{strings.Repeat("a", 64), strings.Repeat("😀", 64)} {
		account := AdminAccount{PasswordHash: testAdminPasswordHash(password)}
		if !account.VerifyPassword(password) {
			t.Fatalf("rejected valid %d-byte password", len(password))
		}
	}
	for _, password := range []string{"", strings.Repeat("a", 65), strings.Repeat("😀", 65), string([]byte{0xff})} {
		account := AdminAccount{PasswordHash: testAdminPasswordHash(password)}
		if account.VerifyPassword(password) {
			t.Fatalf("verified invalid %d-byte password", len(password))
		}
	}
}

func TestAdminAccountsValidation(t *testing.T) {
	if (AdminAccount{PasswordHash: AdminPasswordHashPlaceholder}).VerifyPassword("PASTE_GENERATED_HASH_HERE") {
		t.Fatal("sample placeholder verified as a password hash")
	}
	hash := testAdminPasswordHash("secret password")
	for _, test := range []struct {
		name     string
		accounts []AdminAccount
		invalid  bool
	}{
		{name: "no default account", invalid: true},
		{name: "unset password", accounts: []AdminAccount{{Username: "admin"}}},
		{name: "valid", accounts: []AdminAccount{{Username: "admin", PasswordHash: hash}}},
		{name: "sample placeholder", accounts: []AdminAccount{{Username: "admin", PasswordHash: AdminPasswordHashPlaceholder}}},
		{name: "invalid placeholder", accounts: []AdminAccount{{Username: "admin", PasswordHash: AdminPasswordHashPlaceholder + "!"}}, invalid: true},
		{name: "empty username", accounts: []AdminAccount{{PasswordHash: hash}}, invalid: true},
		{name: "other username", accounts: []AdminAccount{{Username: "operator", PasswordHash: hash}}, invalid: true},
		{name: "uppercase username", accounts: []AdminAccount{{Username: "Admin", PasswordHash: hash}}, invalid: true},
		{name: "multiple accounts", accounts: []AdminAccount{{Username: "admin", PasswordHash: hash}, {Username: "admin", PasswordHash: hash}}, invalid: true},
		{name: "plaintext", accounts: []AdminAccount{{Username: "admin", PasswordHash: "secret password"}}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{BotLogin: "bot", Admin: &AdminConfig{Administrators: test.accounts},
				Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
			err := cfg.Validate(t.TempDir())
			if test.invalid != (err != nil) {
				t.Fatalf("Validate gotError=%t, invalid=%t", err != nil, test.invalid)
			}
			if err != nil && (strings.Contains(err.Error(), "secret password") || strings.Contains(err.Error(), hash)) {
				t.Fatal("password leaked in validation error")
			}
		})
	}
}

func TestAdminAccountsDoNotRelaxPublicTLSRequirement(t *testing.T) {
	cfg := Config{BotLogin: "bot", Admin: &AdminConfig{
		Listen: "0.0.0.0:8377", Administrators: []AdminAccount{{Username: "admin", PasswordHash: testAdminPasswordHash("password")}},
	}, Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
	if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "outside literal loopback requires tls_cert_file and tls_key_file") {
		t.Fatal("public admin without TLS did not report the expected error")
	}
}

func TestLoadAdminAccount(t *testing.T) {
	hash := testAdminPasswordHash("correct horse battery staple")
	path := filepath.Join(t.TempDir(), "config.json")
	data := fmt.Sprintf(`{"bot_login":"bot","repositories":[{"name":"owner/repo","id":1,"owner_id":2,"actions":["review"]}],"admin":{"administrators":[{"username":"admin","password_hash":%q}]}}`, hash)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin == nil || len(cfg.Admin.Administrators) != 1 {
		t.Fatal("expected one loaded admin account")
	}
	account := cfg.Admin.Administrators[0]
	if account.Username != "admin" || account.PasswordHash != hash ||
		!account.VerifyPassword("correct horse battery staple") || account.VerifyPassword("wrong") {
		t.Fatal("loaded admin account does not verify its configured password")
	}
}
