package main

import (
	"bytes"
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
			if err := checkModels(t.Context(), &cfg, log.New(&output, "", 0)); err == nil {
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

func TestCheckModelsRejectsOversizedAmbientWorkerCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, provider, variable string
		size                     int
	}{
		{"Codex API key", "codex", "CODEX_API_KEY", config.MaxProviderAPIKeyBytes + 1},
		{"Codex access token", "codex", "CODEX_ACCESS_TOKEN", 4<<10 + 1},
		{"Claude API key", "claude", "ANTHROPIC_API_KEY", config.MaxProviderAPIKeyBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, variable := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "ANTHROPIC_API_KEY"} {
				t.Setenv(variable, "")
			}
			t.Setenv(tc.variable, strings.Repeat("x", tc.size))
			cfg := config.Config{Main: tc.provider}
			var output bytes.Buffer
			err := checkModels(t.Context(), &cfg, log.New(&output, "", 0))
			if err == nil || !strings.Contains(err.Error(), "worker credentials exceed limit") || strings.Contains(err.Error(), strings.Repeat("x", tc.size)) {
				t.Fatalf("oversized ambient credential was not rejected safely: %v", err)
			}
		})
	}
}

func TestLogCommandSkillsReportsUnsupportedWorkflow(t *testing.T) {
	cfg := config.Config{Main: "claude", Commands: map[string]config.CommandMapping{
		"security": {Action: "security-audit", Skills: []string{"unused"}},
	}}
	for _, remote := range []bool{false, true} {
		if remote {
			cfg.RemoteExecutor = &config.RemoteExecutorConfig{}
		}
		var output bytes.Buffer
		logCommandSkills(cfg, log.New(&output, "", 0))
		if !strings.Contains(output.String(), "command security disabled: action security-audit is unsupported with claude as main") {
			t.Fatalf("remote=%t missing disabled action diagnostic: %s", remote, output.String())
		}
	}
}

func TestLogCommandSkillsRequiresPublishingHost(t *testing.T) {
	cfg := config.Config{Main: "codex", RemoteExecutor: &config.RemoteExecutorConfig{}, Commands: map[string]config.CommandMapping{
		"security": {Action: "security-audit", Skills: []string{"unused"}},
	}}
	var output bytes.Buffer
	logCommandSkills(cfg, log.New(&output, "", 0))
	if disabled := strings.Contains(output.String(), "command security disabled"); disabled != (runtime.GOOS == "windows") {
		t.Fatalf("publishing host audit diagnostic: %s", output.String())
	}
}

func TestCheckModelsBoundsResolvedCatalogueModel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell provider fixture is unavailable on native Windows")
	}
	for _, selection := range []string{"default", "alias"} {
		for _, size := range []int{128, 129} {
			t.Run(fmt.Sprintf("%s/bytes%d", selection, size), func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				for _, key := range []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
					t.Setenv(key, "")
				}
				model := strings.Repeat("m", size)
				value := "default"
				cfg := config.Config{Main: "claude"}
				if selection == "alias" {
					value, cfg.Claude.Model = "quick", "quick"
				}
				catalogue := fmt.Sprintf(`{"type":"control_response","response":{"subtype":"success","request_id":"minion-initialize","response":{"models":[{"value":%q,"resolvedModel":%q}]}}}`, value, model)
				testsupport.FakeOnPath(t, "claude", `#!/bin/sh
if [ "$3" = 'auth' ]; then
  printf '%s\n' '{"loggedIn":true}'
else
  case " $* " in *' --minion-unsupported-capability-probe '*) printf '%s\n' 'unknown option --minion-unsupported-capability-probe' >&2; exit 2 ;; esac
  case " $* " in
    *' --resume '*)
      next=false
      for arg do
        if $next; then id=$arg; break; fi
        [ "$arg" != --resume ] || next=true
      done
      printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0}}'
      printf 'No conversation found with session ID: %s\n' "$id" >&2
      exit 1 ;;
  esac
  input=$(/bin/cat)
  case "$input" in
    *minion-initialize*) printf '%s\n' '`+catalogue+`' ;;
    *) exit 90 ;;
  esac
fi
`)
				var output bytes.Buffer
				err := checkModels(t.Context(), &cfg, log.New(&output, "", 0))
				if cfg.Claude.Model != model {
					t.Fatalf("preflight did not resolve catalogue model: model=%q error=%v", cfg.Claude.Model, err)
				}
				if size == 128 {
					if err != nil {
						t.Fatalf("exact-limit resolved model rejected: %v", err)
					}
					cfg.Admin = &config.AdminConfig{UserModelChoices: &config.UserModelChoices{
						Main: []config.RoleModelConfig{{Model: model, Effort: "high"}},
					}}
					if err := checkModels(t.Context(), &cfg, log.New(&output, "", 0)); err == nil ||
						!strings.Contains(err.Error(), "approved Claude choice") || !strings.Contains(err.Error(), "unsupported") {
						t.Fatalf("unsupported approved Claude effort passed -check: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "claude settings: invalid action model or skills") {
					t.Fatalf("over-limit resolved model reached jobs: %v", err)
				}
			})
		}
	}
}

func TestCheckModelsBoundsResolvedCodexCatalogueModel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell provider fixture is unavailable on native Windows")
	}
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
  case " $* " in *' --minion-unsupported-capability-probe '*) printf '%s\n' 'unexpected argument --minion-unsupported-capability-probe' >&2; exit 2 ;; esac
  if [ "$2" = resume ]; then
    printf '%s\n' 'Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]'
  else
    printf '%s\n' 'Usage: codex exec [OPTIONS] [PROMPT]'
  fi
  ;;
app-server)
  case " $* " in *' minion_unsupported_capability_probe=true '*) printf '%s\n' 'unknown config key: minion_unsupported_capability_probe' >&2; exit 2 ;; esac
  IFS= read -r request
  printf '%s\n' '{"id":1,"result":{}}'
  IFS= read -r request
  IFS= read -r request
  printf '%s\n' '`+catalogue+`'
  exec /bin/cat >/dev/null
  ;;
*) exit 90 ;;
esac
`)
			cfg := config.Config{Main: "codex"}
			var output bytes.Buffer
			err := checkModels(t.Context(), &cfg, log.New(&output, "", 0))
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
