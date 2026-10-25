package claudecli

import (
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
		})
	}
}

const invocationHelp = "Options:\n  --session-id <uuid>\n  -r, --resume [sessionId]\n  --add-dir <directories...>\n  --model <name>\n  --effort <level>\n"

func TestValidateRejectsMissingInvocationOptionBeforeModelCall(t *testing.T) {
	for _, test := range []struct{ option, declaration string }{
		{"--session-id", "  --session-id <uuid>\n"},
		{"--resume", "  -r, --resume [sessionId]\n"},
		{"--add-dir", "  --add-dir <directories...>\n"},
		{"--model", "  --model <name>\n"},
		{"--effort", "  --effort <level>\n"},
	} {
		t.Run(test.option, func(t *testing.T) {
			binary := fakeValidationHelp(t, strings.Replace(invocationHelp, test.declaration, "", 1))
			_, err := Validate(t.Context(), config.ClaudeConfig{Effort: "high"})
			if err == nil || !strings.Contains(err.Error(), "invocation capability check") || !strings.Contains(err.Error(), test.option) {
				t.Fatalf("accepted help without %s: %v", test.option, err)
			}
			if _, err := os.Stat(binary + ".model-called"); !os.IsNotExist(err) {
				t.Fatalf("capability check started a model call: %v", err)
			}
		})
	}
}

func TestValidateDoesNotRequireUnusedEffortOption(t *testing.T) {
	fakeValidationHelp(t, strings.Replace(invocationHelp, "  --effort <level>\n", "", 1))
	if _, err := Validate(t.Context(), config.ClaudeConfig{}); err != nil {
		t.Fatalf("rejected CLI without unused effort option: %v", err)
	}
}

func TestHelpOptionDeclaredRequiresDeclaration(t *testing.T) {
	if helpOptionDeclared([]byte("  --other <value>  See --resume for details\n"), "--resume") || helpOptionDeclared([]byte("  --resumed <id>\n"), "--resume") {
		t.Fatal("accepted a description or prefix as an option declaration")
	}
	if !helpOptionDeclared([]byte("  -r, --resume [sessionId]\n"), "--resume") {
		t.Fatal("missed an option declaration after a short alias")
	}
}

func fakeValidationHelp(t *testing.T, help string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Claude CLI is not supported on native Windows")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
	}
	dir := testsupport.FakeOnPath(t, "claude", `#!/bin/sh
if [ "$3" = auth ]; then
  printf '%s\n' '{"loggedIn":true}'
elif [ "$1" = --help ]; then
  cat <<'MINION_HELP'
`+help+`MINION_HELP
else
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
