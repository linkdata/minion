package claudecli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

const dummyAPIKey = "minion-dummy-config-api-key"

func TestAuthentication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Claude CLI is not supported on native Windows")
	}
	parent := filepath.Join(t.TempDir(), "space ' $ `\n")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := authentication(parent, dummyAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "api-key"): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("credential permissions = %v, %v", info, err)
		}
	}
	data, err := exec.Command("/bin/sh", "-c", apiKeyHelper(dir)).Output()
	if err != nil || string(data) != dummyAPIKey {
		t.Fatal("quoted helper did not return the configured key")
	}
	if dir, err := authentication(parent, ""); dir != "" || err != nil {
		t.Fatalf("empty key creates authentication: %q, %v", dir, err)
	}
	if dir, err := authentication(filepath.Join(parent, "absent"), dummyAPIKey); dir != "" || err == nil {
		t.Fatal("accepted missing authentication parent")
	}
}

func TestAuthenticationCanonicalParent(t *testing.T) {
	parent := t.TempDir()
	link := filepath.Join(t.TempDir(), "auth-parent")
	symlinkOrSkip(t, parent, link)
	dir, err := authentication(link, dummyAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	expectedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != expectedParent {
		t.Fatal("credential path is not canonical for sandbox grant checks")
	}
}

func TestAPIKeyConfiguration(t *testing.T) {
	for _, validate := range []bool{false, true} {
		t.Run(map[bool]string{false: "run", true: "validate"}[validate], func(t *testing.T) {
			binary := fakeExecutable(t, "claude", `test -z "$ANTHROPIC_API_KEY$CLAUDE_CODE_OAUTH_TOKEN$ANTHROPIC_AUTH_TOKEN" || exit 2
test "$(cat "$CLAUDE_CONFIG_DIR/api-key")" = '`+dummyAPIKey+`' || exit 3
printf '%s\n' "$CLAUDE_CONFIG_DIR" >> "$0.authdirs"
printf '%s\n' "$@" >> "$0.args"
for arg in "$@"; do
  if [ "$arg" = auth ]; then
    printf '%s\n' '{"loggedIn":true}'
    exit
  fi
done
input=$(cat)
case "$input" in
  *minion-initialize*) printf '%s\n' '`+modelCatalogue+`' ;;
  *) printf '%s\n' '`+successfulResult+`' ;;
esac
`)
			for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CONFIG_DIR"} {
				t.Setenv(key, "ignored-environment-value")
			}
			settings := config.ClaudeConfig{APIKey: dummyAPIKey}
			var err error
			if validate {
				_, err = Validate(t.Context(), settings)
			} else {
				_, err = Run(t.Context(), t.TempDir(), "task", true, "", settings)
			}
			if err != nil {
				t.Fatal(err)
			}
			dirs := strings.Fields(readFile(t, binary+".authdirs"))
			for _, dir := range dirs {
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("credential directory retained: %v", err)
				}
			}
			args := readFile(t, binary+".args")
			if strings.Contains(args, dummyAPIKey) {
				t.Fatal("configured key exposed in CLI arguments")
			}
			policies := 0
			lines := strings.Split(args, "\n")
			for i, arg := range lines[:len(lines)-1] {
				if arg != "--settings" {
					continue
				}
				var policy struct {
					Helper  string `json:"apiKeyHelper"`
					Sandbox struct {
						Credentials struct {
							Files []struct{ Path, Mode string } `json:"files"`
						} `json:"credentials"`
					} `json:"sandbox"`
				}
				if err := json.Unmarshal([]byte(lines[i+1]), &policy); err != nil {
					t.Fatal(err)
				}
				if policy.Helper != apiKeyHelper(dirs[0]) {
					t.Fatal("missing explicit configured-key helper")
				}
				if len(policy.Sandbox.Credentials.Files) != 1 || policy.Sandbox.Credentials.Files[0].Path != filepath.Join(dirs[0], "api-key") || policy.Sandbox.Credentials.Files[0].Mode != "deny" {
					t.Fatalf("credential file is not denied to sandbox tools: %+v", policy.Sandbox.Credentials.Files)
				}
				policies++
			}
			want := 1
			if validate {
				want = 2 // Authentication and model discovery both use the helper.
			}
			if policies != want {
				t.Fatalf("explicit helper settings = %d, want %d", policies, want)
			}
		})
	}
}

func TestAPIKeyErrors(t *testing.T) {
	for _, validate := range []bool{false, true} {
		t.Run(map[bool]string{false: "run", true: "validate"}[validate], func(t *testing.T) {
			binary := fakeExecutable(t, "claude", `printf '%s\n' "$CLAUDE_CONFIG_DIR" > "$0.authdir"
cat "$CLAUDE_CONFIG_DIR/api-key" >&2
exit 1
`)
			settings := config.ClaudeConfig{APIKey: dummyAPIKey}
			var err error
			if validate {
				_, err = Validate(t.Context(), settings)
			} else {
				_, err = Run(t.Context(), t.TempDir(), "task", false, "", settings)
			}
			if err == nil || strings.Contains(err.Error(), dummyAPIKey) || !strings.Contains(err.Error(), "[redacted]") {
				t.Fatal("CLI error must redact configured key")
			}
			dir := strings.TrimSpace(readFile(t, binary+".authdir"))
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed CLI retained credentials: %v", err)
			}
		})
	}
}

func TestAPIKeyEncodedStderrRedaction(t *testing.T) {
	key := `fixture"secret`
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, validate := range []bool{false, true} {
		t.Run(map[bool]string{false: "run", true: "validate"}[validate], func(t *testing.T) {
			fakeExecutable(t, "claude", "printf '%s\\n' "+aicli.ShellQuote(string(encoded[1:len(encoded)-1]))+" >&2\nexit 1\n")
			var err error
			if validate {
				_, err = Validate(t.Context(), config.ClaudeConfig{APIKey: key})
			} else {
				_, err = Run(t.Context(), t.TempDir(), "task", false, "", config.ClaudeConfig{APIKey: key})
			}
			if err == nil || strings.Contains(err.Error(), "fixture") || !strings.Contains(err.Error(), "[redacted]") {
				t.Fatalf("encoded configured key escaped in CLI error: %v", err)
			}
		})
	}
}

func TestAPIKeyAnswerRedaction(t *testing.T) {
	fakeExecutable(t, "claude", "cat >/dev/null\nprintf '%s\\n' '"+strings.ReplaceAll(successfulResult, "final answer", dummyAPIKey)+"'\n")
	result, err := Run(t.Context(), t.TempDir(), "task", false, "", config.ClaudeConfig{APIKey: dummyAPIKey})
	if err != nil || result.Answer != "[redacted]" || !result.UsageComplete {
		t.Fatal("successful answer must redact configured key")
	}
}

func TestAPIKeyGrantOverlap(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct{ workspace, output string }{
		{workspace: root},
		{workspace: filepath.Join(root, "source"), output: root},
	} {
		if _, err := sandboxSettings(test.workspace, test.output, filepath.Join(root, "auth")); err == nil || !strings.Contains(err.Error(), "API key directory overlaps") {
			t.Fatalf("accepted writable root containing credential directory: %+v: %v", test, err)
		}
	}
}
