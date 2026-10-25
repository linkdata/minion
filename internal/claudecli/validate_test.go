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
	"github.com/linkdata/minion/internal/testsupport"
)

const modelCatalogue = `{"type":"control_response","response":{"subtype":"success","request_id":"minion-initialize","response":{"models":[{"value":"unresolved"},{"value":"default","resolvedModel":"claude-pinned","supportedEffortLevels":["low","high"]},{"value":"quick","resolvedModel":"claude-small","supportedEffortLevels":["low","high"]}]}}}`

func TestValidate(t *testing.T) {
	binary := fakeExecutable(t, "claude", `if [ "$3" = 'auth' ]; then
  printf '%s\n' '{"loggedIn":true}'
else
  printf '%s\n' "$@" > "$0.args"
  cat > "$0.input"
  printf '%s\n' '`+modelCatalogue+`'
fi
`)
	for _, tc := range []struct {
		name, model, effort, wantModel, wantEffort string
		fails                                      bool
	}{
		{name: "defaults", wantModel: "claude-pinned"},
		{name: "explicit default", model: "default", wantModel: "claude-pinned"},
		{name: "alias", model: "quick", effort: "low", wantModel: "claude-small", wantEffort: "low"},
		{name: "pinned", model: "claude-small", effort: "high", wantModel: "claude-small", wantEffort: "high"},
		{name: "unknown model", model: "missing", fails: true},
		{name: "unresolved model", model: "unresolved", fails: true},
		{name: "unsupported effort", effort: "max", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Remove(binary + ".args"); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			cfg, err := Validate(t.Context(), config.ClaudeConfig{Model: tc.model, Effort: tc.effort})
			if args := readFile(t, binary+".args"); !strings.Contains(args, "--no-session-persistence\n") || !strings.Contains(args, "--input-format\nstream-json\n") || !strings.Contains(args, "--json-schema\n{\"type\":\"object\"}\n") {
				t.Fatalf("model discovery retained a session or lost initialization framing: %q", args)
			}
			if tc.fails {
				if err == nil {
					t.Fatal("expected invalid model configuration to fail")
				}
				return
			}
			if err != nil || cfg.Model != tc.wantModel || cfg.Effort != tc.wantEffort {
				t.Fatalf("Validate = %+v, %v", cfg, err)
			}
			fresh := readFile(t, binary+".fresh-args")
			if !strings.Contains(fresh, "--no-session-persistence\n") || !strings.Contains(fresh, "--session-id\n") || !strings.Contains(fresh, "--add-dir\n") || !strings.Contains(fresh, "--model\n"+tc.wantModel+"\n") {
				t.Fatalf("fresh capability probe used wrong arguments: %q", fresh)
			}
		})
	}
}

const invocationHelp = "Options:\n  --session-id <uuid>\n  -r, --resume [sessionId]\n  --add-dir <directories...>\n  --model <name>\n  --effort <level>\n"

func TestValidateRejectsMissingInvocationOptionBeforeModelCall(t *testing.T) {
	for _, option := range []string{"--session-id", "--resume", "--add-dir", "--model"} {
		t.Run(option, func(t *testing.T) {
			binary := fakeValidationCLI(t, invocationHelp, option, false)
			_, err := Validate(t.Context(), config.ClaudeConfig{})
			if err == nil || !strings.Contains(err.Error(), option) {
				t.Fatalf("accepted CLI without %s: %v", option, err)
			}
			if _, err := os.Stat(binary + ".model-called"); !os.IsNotExist(err) {
				t.Fatalf("capability check started a model call: %v", err)
			}
		})
	}
}

func TestValidateRejectsMissingConfiguredEffortOption(t *testing.T) {
	binary := fakeValidationCLI(t, strings.Replace(invocationHelp, "  --effort <level>\n", "", 1), "", false)
	_, err := Validate(t.Context(), config.ClaudeConfig{Effort: "high"})
	if err == nil || !strings.Contains(err.Error(), "--effort") {
		t.Fatalf("accepted CLI without effort option: %v", err)
	}
	if _, err := os.Stat(binary + ".model-called"); !os.IsNotExist(err) {
		t.Fatalf("capability check started a model call: %v", err)
	}
}

func TestValidateDoesNotRequireUnusedEffortOption(t *testing.T) {
	fakeValidationCLI(t, strings.Replace(invocationHelp, "  --effort <level>\n", "", 1), "", false)
	if _, err := Validate(t.Context(), config.ClaudeConfig{}); err != nil {
		t.Fatalf("rejected CLI without unused effort option: %v", err)
	}
}

func TestValidateRejectsCLIThatIgnoresUnknownOption(t *testing.T) {
	fakeValidationCLI(t, invocationHelp, "", true)
	if _, err := Validate(t.Context(), config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "did not reject an unknown option") {
		t.Fatalf("accepted CLI that ignores unknown options: %v", err)
	}
}

func TestValidateHonorsCallerCancellation(t *testing.T) {
	fakeValidationCLI(t, invocationHelp, "", false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Validate(ctx, config.ClaudeConfig{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost caller cancellation: %v", err)
	}
}

func TestEmptySessionLookupRejectsModelActivity(t *testing.T) {
	empty := `{"type":"result","is_error":true,"num_turns":0,"total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0}}`
	if !isEmptySessionLookup([]byte(empty)) {
		t.Fatal("rejected an empty-session error result")
	}
	for _, output := range []string{
		`{"type":"assistant"}` + "\n" + empty,
		`{"type":"result","is_error":true,"num_turns":0,"usage":{"input_tokens":1}}`,
		`{"type":"result","is_error":false,"num_turns":0}`,
	} {
		if isEmptySessionLookup([]byte(output)) {
			t.Fatalf("accepted model activity or success: %q", output)
		}
	}
}

func TestHelpOptionDeclaredRequiresDeclaration(t *testing.T) {
	if helpOptionDeclared([]byte("  --other <value>  See --effort for details\n"), "--effort") || helpOptionDeclared([]byte("  --effortful <id>\n"), "--effort") {
		t.Fatal("accepted a description or prefix as an option declaration")
	}
	if !helpOptionDeclared([]byte("  --effort <level>\n"), "--effort") {
		t.Fatal("missed an effort option declaration")
	}
}

func fakeValidationCLI(t *testing.T, help, rejected string, ignoreUnknown bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Claude CLI is not supported on native Windows")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
	}
	unknown := `case " $* " in *' --minion-unsupported-capability-probe '*) printf '%s\n' 'unknown option --minion-unsupported-capability-probe' >&2; exit 2 ;; esac`
	if ignoreUnknown {
		unknown = ""
	}
	reject := ""
	if rejected != "" {
		reject = `case " $* " in *' ` + rejected + ` '*) printf '%s\n' 'unsupported ` + rejected + `' >&2; exit 2 ;; esac`
	}
	dir := testsupport.FakeOnPath(t, "claude", `#!/bin/sh
if [ "$3" = auth ]; then
  printf '%s\n' '{"loggedIn":true}'
elif [ "$1" = --help ]; then
  cat <<'MINION_HELP'
`+help+`MINION_HELP
else
  `+unknown+`
  `+reject+`
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
  input=$(cat)
  case "$input" in
    *minion-initialize*) printf '%s\n' '`+modelCatalogue+`' ;;
    *) printf 'called' > "$0.model-called"; exit 99 ;;
  esac
fi
`)
	return filepath.Join(dir, "claude")
}

func TestValidateRequiresInitialization(t *testing.T) {
	fakeExecutable(t, "claude", `if [ "$3" = 'auth' ]; then
  printf '%s\n' '{"loggedIn":true}'
else
  exit 1
fi
`)
	if _, err := Validate(t.Context(), config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "model discovery") {
		t.Fatalf("Validate = %v", err)
	}
}

func TestInitializationModels(t *testing.T) {
	for _, input := range []string{"", "{", `{"type":"control_response","response":{"request_id":"other"}}`, `{"type":"control_response","response":{"request_id":"minion-initialize","subtype":"error"}}`} {
		if _, err := initializationModels([]byte(input)); err == nil {
			t.Fatalf("accepted malformed initialize response %q", input)
		}
	}
	if models, err := initializationModels([]byte(modelCatalogue)); err != nil || len(models) != 3 {
		t.Fatalf("initializationModels = %+v, %v", models, err)
	}
}
