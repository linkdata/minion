package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
)

func TestHashAdminPasswordMatchesVerifier(t *testing.T) {
	password := []byte("correct horse battery staple")
	first, err := hashAdminPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashAdminPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	account := config.AdminAccount{PasswordHash: first}
	if !account.VerifyPassword(string(password)) || account.VerifyPassword("wrong password") {
		t.Fatal("generated hash failed the admin password verifier")
	}
	if strings.Split(first, "$")[4] == strings.Split(second, "$")[4] {
		t.Fatal("generated hashes reused a salt")
	}
	for _, invalid := range [][]byte{nil, bytes.Repeat([]byte("a"), 4097), {0xff}} {
		if hash, err := hashAdminPassword(invalid); err == nil || hash != "" {
			t.Fatalf("accepted invalid password length or UTF-8: hash=%q error=%v", hash, err)
		}
	}
	if _, err := hashAdminPassword(bytes.Repeat([]byte("a"), 4096)); err != nil {
		t.Fatalf("rejected maximum password size: %v", err)
	}
}

func TestHashModeRejectsArgumentAndBypassesConfig(t *testing.T) {
	for _, test := range []struct {
		name, want string
		args       []string
	}{
		{name: "plaintext argument", args: []string{"-mode", "hash", "secret-in-argv"}, want: "do not pass it as an argument"},
		{name: "loop flag", args: []string{"-mode=hash", "-loop"}, want: "cannot be combined"},
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
