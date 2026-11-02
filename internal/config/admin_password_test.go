package config

import (
	"encoding/base64"
	"fmt"
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
	account := AdminAccount{Username: "operator", PasswordHash: hash}
	if !account.VerifyPassword("correct horse battery staple") || account.VerifyPassword("wrong") {
		t.Fatal("Argon2id password verification changed")
	}
	for _, value := range []string{
		"plaintext", strings.Replace(hash, "argon2id", "argon2i", 1),
		strings.Replace(hash, "m=19456", "m=1", 1),
		strings.Replace(hash, "m=19456", "m=1000000", 1),
		strings.Replace(hash, "t=2", "t=1", 1),
		strings.Replace(hash, "p=1", "p=255", 1),
		strings.Replace(hash, "v=19", "v=16", 1),
		strings.Replace(hash, salt, "?", 1),
	} {
		if _, ok := parseAdminPasswordHash(value); ok || (AdminAccount{PasswordHash: value}).VerifyPassword("correct horse battery staple") {
			t.Fatalf("accepted malformed or unsafe hash %q", value)
		}
	}
}

func TestAdminAccountsValidation(t *testing.T) {
	hash := testAdminPasswordHash("secret password")
	for _, test := range []struct {
		name     string
		accounts []AdminAccount
		invalid  bool
	}{
		{name: "no default account"},
		{name: "valid", accounts: []AdminAccount{{Username: "operator", PasswordHash: hash}}},
		{name: "empty username", accounts: []AdminAccount{{PasswordHash: hash}}, invalid: true},
		{name: "duplicate username", accounts: []AdminAccount{{Username: "Operator", PasswordHash: hash}, {Username: "operator", PasswordHash: hash}}, invalid: true},
		{name: "plaintext", accounts: []AdminAccount{{Username: "operator", PasswordHash: "secret password"}}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{BotLogin: "bot", Admin: &AdminConfig{Administrators: test.accounts},
				Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
			err := cfg.Validate(t.TempDir())
			if test.invalid != (err != nil) {
				t.Fatalf("Validate error = %v, invalid = %t", err, test.invalid)
			}
			if err != nil && (strings.Contains(err.Error(), "secret password") || strings.Contains(err.Error(), hash)) {
				t.Fatalf("password leaked in validation error: %v", err)
			}
		})
	}
}

func TestAdminAccountsDoNotRelaxPublicTLSRequirement(t *testing.T) {
	cfg := Config{BotLogin: "bot", Admin: &AdminConfig{
		Listen: "0.0.0.0:8377", Administrators: []AdminAccount{{Username: "operator", PasswordHash: testAdminPasswordHash("password")}},
	}, Repositories: []Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"review"}}}}
	if err := cfg.Validate(t.TempDir()); err == nil || !strings.Contains(err.Error(), "outside literal loopback") {
		t.Fatalf("public admin without TLS: error=%v", err)
	}
}
