package claudecli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestHomePathUsesNativeVariableAndDefault(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", root)
	}
	for _, override := range []string{"", filepath.Join(root, "explicit")} {
		t.Setenv("CLAUDE_CONFIG_DIR", override)
		want := filepath.Join(realRoot, "explicit")
		if override == "" {
			want = filepath.Join(realRoot, ".claude")
		}
		if got, err := HomePath(); err != nil || got != want {
			t.Fatalf("native home=%q, %v; want %q", got, err, want)
		}
	}
}

func TestUnavailableImplicitHomeStopsBeforeCLI(t *testing.T) {
	for _, validate := range []bool{false, true} {
		t.Run(map[bool]string{false: "run", true: "validate"}[validate], func(t *testing.T) {
			binary := fakeExecutable(t, "claude", `touch "$0.called"; exit 99`)
			t.Setenv("HOME", "")
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			var err error
			if validate {
				_, err = Validate(context.Background(), config.ClaudeConfig{})
			} else {
				_, err = Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
			}
			if err == nil || !strings.Contains(err.Error(), "CLAUDE_CONFIG_DIR") {
				t.Fatalf("unavailable implicit home accepted: %v", err)
			}
			if _, err := os.Stat(binary + ".called"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unknown-home refusal launched Claude: %v", err)
			}
		})
	}
}

func TestConfiguredKeyUsesPrivateHomeWithoutUserHome(t *testing.T) {
	binary := fakeExecutable(t, "claude", `printf '%s' "$CLAUDE_CONFIG_DIR" > "$0.home"
cat >/dev/null
printf '%s\n' '`+successfulResult+`'
`)
	t.Setenv("HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{APIKey: "fixture-key"})
	if err != nil || result.Answer != "final answer" {
		t.Fatalf("configured private home was refused: %+v, %v", result, err)
	}
	home := readFile(t, binary+".home")
	if !strings.HasPrefix(filepath.Base(home), ".minion-claude-auth-") {
		t.Fatalf("configured key did not select its private home: %q", home)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configured private home was not removed: %v", err)
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
if [ "$1" = '--help' ]; then
  cat <<'HELP'
`+claudeHelp+`HELP
elif [ "$3" = 'auth' ]; then
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
					realBase, err := filepath.EvalSymlinks(base)
					if err != nil {
						t.Fatal(err)
					}
					wantEnv := "x," + filepath.Join(realBase, ".claude")
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
