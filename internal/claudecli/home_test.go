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

func TestHomePathUsesNativeVariableAndDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	for _, override := range []string{"", filepath.Join(root, "explicit")} {
		t.Setenv("CLAUDE_CONFIG_DIR", override)
		want := override
		if want == "" {
			want = filepath.Join(root, ".claude")
		}
		if got, err := HomePath(); err != nil || got != want {
			t.Fatalf("native home=%q, %v; want %q", got, err, want)
		}
	}
}

func TestRunAndValidatePreserveNativeHomeSelection(t *testing.T) {
	for _, mode := range []string{"explicit relative", "implicit", "empty override"} {
		t.Run(mode, func(t *testing.T) {
			for _, validate := range []bool{false, true} {
				t.Run(map[bool]string{false: "run", true: "validate"}[validate], func(t *testing.T) {
					fakeExecutable(t, "bwrap", "exit 99\n")
					fakeExecutable(t, "socat", "exit 99\n")
					binary := fakeExecutable(t, "claude", `printf '%s,%s\n' "${CLAUDE_CONFIG_DIR+x}" "${CLAUDE_CONFIG_DIR-}" >> "$0.homes"
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
					base := t.TempDir()
					t.Setenv("HOME", base)
					home := filepath.Join(base, ".claude")
					wantEnv := "x," + home
					cwd, err := os.Getwd()
					if err != nil {
						t.Fatal(err)
					}
					relative, err := filepath.Rel(cwd, home)
					if err != nil {
						t.Fatal(err)
					}
					t.Setenv("CLAUDE_CONFIG_DIR", relative)
					if mode != "explicit relative" {
						t.Setenv("CLAUDE_CONFIG_DIR", "")
						if mode == "implicit" {
							if err := os.Unsetenv("CLAUDE_CONFIG_DIR"); err != nil {
								t.Fatal(err)
							}
						}
						wantEnv = ","
					}
					if validate {
						_, err = Validate(context.Background(), config.ClaudeConfig{})
					} else {
						_, err = Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
					}
					if err != nil {
						t.Fatal(err)
					}
					for _, got := range strings.Split(strings.TrimSpace(readFile(t, binary+".homes")), "\n") {
						if got != wantEnv {
							t.Fatalf("native home=%q; want %q", got, wantEnv)
						}
					}
					if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("home admission created a directory: %v", err)
					}
				})
			}
		})
	}
}
