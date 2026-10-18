package claudecli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestHomePathKeepsNativeSelectionAndMissingDirectories(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got, err := HomePath(); err != nil || got != filepath.Join(root, ".claude") {
		t.Fatalf("default native home=%q, %v", got, err)
	}
	t.Setenv("HOME", "")
	if got, err := HomePath(); err != nil || got != "" {
		t.Fatalf("guessed an unavailable native home=%q, %v", got, err)
	}
	target := filepath.Join(root, "real", "child")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", alias+"/../not-created")
	want := filepath.Join(root, "real", "not-created")
	if got, err := HomePath(); err != nil || got != want {
		t.Fatalf("native symlink/.. home=%q, %v; want %q", got, err, want)
	}
	if _, err := os.Stat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("home resolution created a directory: %v", err)
	}
}

func TestRunAndValidatePinRelativeNativeHome(t *testing.T) {
	for _, validate := range []bool{false, true} {
		t.Run(map[bool]string{false: "run", true: "validate"}[validate], func(t *testing.T) {
			fakeExecutable(t, "bwrap", "exit 99\n")
			fakeExecutable(t, "socat", "exit 99\n")
			binary := fakeExecutable(t, "claude", `printf '%s\n' "$CLAUDE_CONFIG_DIR" >> "$0.homes"
if [ "$1" = '--version' ]; then
  printf '%s\n' '2.1.302 (Claude Code)'
elif [ "$4" = 'auth' ]; then
  printf '%s\n' '{"loggedIn":true}'
else
  input=$(cat)
  case "$input" in
    *minion-initialize*) printf '%s\n' '`+modelCatalogue+`' ;;
    *) printf '%s\n' '`+successfulResult+`' ;;
  esac
fi
`)
			home := filepath.Join(t.TempDir(), "not-created")
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			relative, err := filepath.Rel(cwd, home)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("CLAUDE_CONFIG_DIR", relative)
			if validate {
				_, err = Validate(context.Background(), config.ClaudeConfig{})
			} else {
				_, err = Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, got := range strings.Split(strings.TrimSpace(readFile(t, binary+".homes")), "\n") {
				if got != home {
					t.Fatalf("native home=%q; want %q", got, home)
				}
			}
			if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("home admission created a directory: %v", err)
			}
		})
	}
}
