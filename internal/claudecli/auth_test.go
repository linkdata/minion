package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	workspacepath "github.com/linkdata/minion/internal/workspace"
)

const dummyAPIKey = "minion-dummy-config-api-key"

func TestAuthentication(t *testing.T) {
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
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
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
			fakeExecutable(t, "bwrap", "exit 99\n")
			fakeExecutable(t, "socat", "exit 99\n")
			binary := fakeExecutable(t, "claude", `test -z "$ANTHROPIC_API_KEY$CLAUDE_CODE_OAUTH_TOKEN$ANTHROPIC_AUTH_TOKEN" || exit 2
test "$(cat "$CLAUDE_CONFIG_DIR/api-key")" = '`+dummyAPIKey+`' || exit 3
printf '%s\n' "$CLAUDE_CONFIG_DIR" >> "$0.authdirs"
if [ "$1" = '--version' ]; then
  printf '%s\n' '2.1.302 (Claude Code)'
  exit
fi
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
				_, err = Validate(context.Background(), settings)
			} else {
				_, err = Run(context.Background(), t.TempDir(), "task", true, "", settings)
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
						Filesystem struct {
							Read  []string `json:"allowRead"`
							Write []string `json:"allowWrite"`
						} `json:"filesystem"`
					} `json:"sandbox"`
				}
				if err := json.Unmarshal([]byte(lines[i+1]), &policy); err != nil {
					t.Fatal(err)
				}
				if policy.Helper != apiKeyHelper(dirs[0]) {
					t.Fatal("missing explicit configured-key helper")
				}
				for _, grant := range append(policy.Sandbox.Filesystem.Read, policy.Sandbox.Filesystem.Write...) {
					if workspacepath.Within(grant, dirs[0]) {
						t.Fatal("credential helper is exposed to sandbox tools")
					}
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
				_, err = Validate(context.Background(), settings)
			} else {
				_, err = Run(context.Background(), t.TempDir(), "task", false, "", settings)
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
				_, err = Validate(context.Background(), config.ClaudeConfig{APIKey: key})
			} else {
				_, err = Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{APIKey: key})
			}
			if err == nil || strings.Contains(err.Error(), "fixture") || !strings.Contains(err.Error(), "[redacted]") {
				t.Fatalf("encoded configured key escaped in CLI error: %v", err)
			}
		})
	}
}

func TestAPIKeyAnswerRedaction(t *testing.T) {
	fakeExecutable(t, "claude", "cat >/dev/null\nprintf '%s\\n' '"+strings.ReplaceAll(successfulResult, "final answer", dummyAPIKey)+"'\n")
	result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{APIKey: dummyAPIKey})
	if err != nil || result.Answer != "[redacted]" || !result.UsageComplete {
		t.Fatal("successful answer must redact configured key")
	}
}

func TestAPIKeyGrantOverlap(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "checkout")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(skill, []byte("dummy skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeExecutable(t, "claude", "exit 99\n")
	_, err := Run(context.Background(), workspace, "task", false, "", config.ClaudeConfig{APIKey: dummyAPIKey, Skills: []string{skill}})
	if err == nil || !strings.Contains(err.Error(), "API key directory overlaps") {
		t.Fatal("accepted skill read grant containing credential directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("failed validation retained temporary files: %v, %v", entries, err)
	}
}
