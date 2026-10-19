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
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(key, "")
	}
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			if provider == "claude" && runtime.GOOS != "linux" {
				t.Skip("Claude sandbox support requires Linux")
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
	if runtime.GOOS != "linux" {
		t.Skip("Claude sandbox support requires Linux")
	}
	for _, selection := range []string{"default", "alias"} {
		for _, size := range []int{128, 129} {
			t.Run(fmt.Sprintf("%s/bytes%d", selection, size), func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				for _, key := range []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
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
if [ "$1" = '--version' ]; then
  printf '%s\n' '2.1.302 (Claude Code)'
elif [ "$4" = 'auth' ]; then
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
			for _, key := range []string{"CODEX_HOME", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
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
