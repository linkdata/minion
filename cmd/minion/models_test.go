package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/testsupport"
)

func TestCheckModelsRequiresOnlySelectedProvider(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell provider fixture is unavailable on native Windows")
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		t.Setenv(key, "")
	}
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			if provider == "claude" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
				t.Skip("Claude sandbox support requires Linux or macOS")
			}
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			for _, name := range []string{"codex", "claude"} {
				body := "#!/bin/sh\nprintf 'called' > \"$0.called\"\nprintf 'fixture preflight failure' >&2\nexit 1\n"
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.Config{Main: provider}
			var output bytes.Buffer
			if err := checkModels(context.Background(), &cfg, log.New(&output, "", 0)); err == nil {
				t.Fatal("preflight accepted a broken CLI")
			}
			for _, name := range []string{"codex", "claude"} {
				_, err := os.Stat(filepath.Join(dir, name+".called"))
				if (err == nil) != (name == provider) {
					t.Fatalf("%s called = %t; selected %s", name, err == nil, provider)
				}
			}
		})
	}
}

func TestLogCommandSkillsReportsUnsupportedWorkflow(t *testing.T) {
	var output bytes.Buffer
	cfg := config.Config{Main: "claude", Commands: map[string]config.CommandMapping{
		"security": {Action: "security-audit", Skills: []string{"unused"}},
	}}
	logCommandSkills(cfg, log.New(&output, "", 0))
	if !strings.Contains(output.String(), "command security disabled: action security-audit is unsupported with claude as main") {
		t.Fatalf("missing disabled action diagnostic: %s", output.String())
	}
}

func TestCheckModelsBoundsResolvedCatalogueModel(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Claude sandbox support requires Linux or macOS")
	}
	for _, selection := range []string{"default", "alias"} {
		for _, size := range []int{128, 129} {
			t.Run(fmt.Sprintf("%s/bytes%d", selection, size), func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				for _, key := range []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
					t.Setenv(key, "")
				}
				for _, name := range []string{"bwrap", "socat"} {
					testsupport.FakeOnPath(t, name, "#!/bin/sh\nexit 99\n")
				}
				model := strings.Repeat("m", size)
				value := "default"
				cfg := config.Config{Main: "claude"}
				if selection == "alias" {
					value, cfg.Claude.Model = "quick", "quick"
				}
				catalogue := fmt.Sprintf(`{"type":"control_response","response":{"subtype":"success","request_id":"minion-initialize","response":{"models":[{"value":%q,"resolvedModel":%q}]}}}`, value, model)
				testsupport.FakeOnPath(t, "claude", `#!/bin/sh
if [ "$1" = '--help' ]; then
  printf '%s\n' '--session-id --resume --add-dir --model --effort --json-schema --no-session-persistence --input-format -p --verbose --output-format --permission-mode --permission-prompts --safe-mode --setting-sources --disable-slash-commands --strict-mcp-config --mcp-config --no-chrome --tools --allowedTools --settings'
elif [ "$3" = 'auth' ]; then
  printf '%s\n' '{"loggedIn":true}'
else
  input=$(/bin/cat)
  case "$input" in
    *minion-initialize*) printf '%s\n' '`+catalogue+`' ;;
    *) exit 90 ;;
  esac
fi
`)
				var output bytes.Buffer
				err := checkModels(context.Background(), &cfg, log.New(&output, "", 0))
				if cfg.Claude.Model != model {
					t.Fatalf("preflight did not resolve catalogue model: model=%q error=%v", cfg.Claude.Model, err)
				}
				if size == 128 {
					if err != nil {
						t.Fatalf("exact-limit resolved model rejected: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "claude settings: invalid action model or skills") {
					t.Fatalf("over-limit resolved model reached jobs: %v", err)
				}
			})
		}
	}
}

func TestCheckModelsBoundsResolvedCodexCatalogueModel(t *testing.T) {
	for _, size := range []int{128, 129} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			for _, key := range []string{"CODEX_HOME", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
				t.Setenv(key, "")
			}
			model := strings.Repeat("m", size)
			catalogue := fmt.Sprintf(`{"id":2,"result":{"data":[{"model":%q,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium"}]}]}}`, model)
			testsupport.FakeOnPath(t, "codex", `#!/bin/sh
case "$1" in
login) exit 0 ;;
debug)
  text='MINION_TRUSTED_PROMPT_78'
  if [ ! -f "$PWD/.minion-codex-root" ]; then
    text="$text MINION_UNTRUSTED_AGENTS_78 MINION_UNTRUSTED_SKILL_78"
  fi
  printf '[{"role":"user","content":[{"text":"%s"}]}]\n' "$text"
  ;;
exec)
  case " $* " in *' --minion-unsupported-capability-probe '*) exit 2 ;; esac
  if [ "$2" = resume ]; then
    printf '%s\n' 'Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]'
  else
    printf '%s\n' 'Usage: codex exec [OPTIONS] [PROMPT]'
  fi
  ;;
app-server)
  IFS= read -r request
  printf '%s\n' '{"id":1,"result":{}}'
  IFS= read -r request
  IFS= read -r request
  printf '%s\n' '`+catalogue+`'
  /bin/cat >/dev/null
  ;;
*) exit 90 ;;
esac
`)
			cfg := config.Config{Main: "codex"}
			var output bytes.Buffer
			err := checkModels(context.Background(), &cfg, log.New(&output, "", 0))
			if cfg.Codex.Model != model {
				t.Fatalf("preflight did not resolve Codex catalogue model: model=%q error=%v", cfg.Codex.Model, err)
			}
			if size == 128 {
				if err != nil {
					t.Fatalf("exact-limit resolved model rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "codex settings: invalid action model or skills") {
				t.Fatalf("over-limit resolved model reached jobs: %v", err)
			}
		})
	}
}

func TestGitHubTokenSelectionAndEmptyCLIResult(t *testing.T) {
	t.Run("missing CLI", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "")
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("PATH", t.TempDir())
		if token, err := githubToken(context.Background()); token != "" || err == nil || !strings.Contains(err.Error(), "executable file not found") {
			t.Fatalf("missing gh returned token=%q error=%v", token, err)
		}
	})
	for _, test := range []struct {
		name, ghToken, githubToken, output, want, wantError string
		callCLI                                             bool
	}{
		{name: "GH_TOKEN wins", ghToken: "  preferred \n", githubToken: " secondary ", want: "preferred"},
		{name: "GITHUB_TOKEN fallback", githubToken: " \tsecondary\r\n", want: "secondary"},
		{name: "CLI fallback", output: "  cli-token \r\n", want: "cli-token", callCLI: true},
		{name: "empty CLI result", output: " \r\n", wantError: "gh auth token returned an empty token", callCLI: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && test.callCLI {
				t.Skip("POSIX shell gh fixture is unavailable on native Windows")
			}
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			t.Setenv("GH_TOKEN", test.ghToken)
			t.Setenv("GITHUB_TOKEN", test.githubToken)
			marker := filepath.Join(dir, "gh.called")
			script := "#!/bin/sh\n"
			script += "printf called > \"$0.called\"\n"
			script += "if [ \"$1\" != auth ] || [ \"$2\" != token ] || [ \"$3\" != --hostname ] || [ \"$4\" != github.com ]; then exit 91; fi\n"
			script += "printf '%s' '" + test.output + "'\n"
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			got, err := githubToken(context.Background())
			if got != test.want || (test.wantError == "" && err != nil) || (test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError))) {
				t.Fatalf("token=%q error=%v; want %q, %q", got, err, test.want, test.wantError)
			}
			_, markerErr := os.Stat(marker)
			if test.callCLI != (markerErr == nil) {
				t.Fatalf("CLI called=%t, marker error=%v", markerErr == nil, markerErr)
			}
		})
	}
}
