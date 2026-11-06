package main

import (
	"context"
	"encoding/base64"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
)

func TestHashAdminPasswordMatchesVerifier(t *testing.T) {
	password := "correct horse battery staple"
	first, err := hashAdminPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashAdminPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	account := config.AdminAccount{PasswordHash: first}
	if !account.VerifyPassword(password) || account.VerifyPassword("wrong password") {
		t.Fatal("generated hash failed the admin password verifier")
	}
	parts := strings.Split(first, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=19456,t=2,p=1" {
		t.Fatalf("unexpected Argon2id PHC parameters: %q", first)
	}
	salt, saltErr := base64.RawStdEncoding.DecodeString(parts[4])
	tag, tagErr := base64.RawStdEncoding.DecodeString(parts[5])
	if saltErr != nil || tagErr != nil || len(salt) != 16 || len(tag) != 32 {
		t.Fatalf("unexpected Argon2id salt or tag: salt=%d tag=%d errors=%v, %v", len(salt), len(tag), saltErr, tagErr)
	}
	if parts[4] == strings.Split(second, "$")[4] {
		t.Fatal("generated hashes reused a salt")
	}
	for _, invalid := range []string{"", strings.Repeat("a", 65), strings.Repeat("界", 65), string([]byte{0xff})} {
		if hash, err := hashAdminPassword(invalid); err == nil || hash != "" {
			t.Fatalf("accepted invalid password length or UTF-8: hash=%q error=%v", hash, err)
		}
	}
	for _, valid := range []string{strings.Repeat("a", 64), strings.Repeat("界", 64)} {
		if _, err := hashAdminPassword(valid); err != nil {
			t.Fatalf("rejected 64-character password: %v", err)
		}
	}
}

func TestHashModeRejectsArgumentAndBypassesConfig(t *testing.T) {
	for _, test := range []struct {
		name, want string
		args       []string
	}{
		{name: "plaintext argument", args: []string{"-mode", "hash", "secret-in-argv"}, want: "do not pass it as an argument"},
		{name: "oneshot flag", args: []string{"-mode=hash", "-oneshot"}, want: "cannot be combined"},
		{name: "check flag", args: []string{"-mode=hash", "-check"}, want: "cannot be combined"},
		{name: "install-skills flag", args: []string{"-mode=hash", "-install-skills"}, want: "cannot be combined"},
		{name: "no terminal before config", args: []string{"-mode=hash", "-config=" + filepath.Join(t.TempDir(), "missing.json")}, want: "requires an interactive terminal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldArgs, oldFlags, oldStdin, oldStdout, oldStderr := os.Args, flag.CommandLine, os.Stdin, os.Stdout, os.Stderr
			defer func() {
				os.Args, flag.CommandLine, os.Stdin, os.Stdout, os.Stderr = oldArgs, oldFlags, oldStdin, oldStdout, oldStderr
			}()
			stdin, err := os.CreateTemp(t.TempDir(), "stdin")
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := os.CreateTemp(t.TempDir(), "stdout")
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			stderr, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			os.Args = append([]string{"minion"}, test.args...)
			flag.CommandLine = flag.NewFlagSet("minion", flag.ContinueOnError)
			os.Stdin, os.Stdout, os.Stderr = stdin, stdout, stderr
			code := runWithGitCheckAt(func(context.Context, string) error { t.Error("hash mode checked Git"); return nil }, time.Now)
			output, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal(err)
			}
			printed, err := os.ReadFile(stdout.Name())
			if err != nil {
				t.Fatal(err)
			}
			if code != 2 || !strings.Contains(string(output), test.want) || strings.Contains(string(output), "secret-in-argv") || len(printed) != 0 {
				t.Fatalf("hash mode code=%d stderr=%q stdout=%q", code, output, printed)
			}
		})
	}
}
