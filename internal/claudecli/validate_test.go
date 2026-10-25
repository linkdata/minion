package claudecli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

const modelCatalogue = `{"type":"control_response","response":{"subtype":"success","request_id":"minion-initialize","response":{"models":[{"value":"unresolved"},{"value":"default","resolvedModel":"claude-pinned","supportedEffortLevels":["low","high"]},{"value":"quick","resolvedModel":"claude-small","supportedEffortLevels":["low","high"]}]}}}`
const claudeHelp = `Claude Code 1.0.0
Options:
  -p, --print
`

func TestValidate(t *testing.T) {
	fakeExecutable(t, "bwrap", "exit 99\n")
	fakeExecutable(t, "socat", "exit 99\n")
	binary := fakeExecutable(t, "claude", `if [ "$1" = '--help' ]; then
  cat <<'HELP'
`+claudeHelp+`HELP
elif [ "$1" = '--version' ]; then
  exit 97
elif [ "$3" = 'auth' ]; then
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
			cfg, err := Validate(context.Background(), config.ClaudeConfig{Model: tc.model, Effort: tc.effort})
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

func TestValidateRequiresInitialization(t *testing.T) {
	fakeExecutable(t, "bwrap", "exit 99\n")
	fakeExecutable(t, "socat", "exit 99\n")
	fakeExecutable(t, "claude", `if [ "$1" = '--help' ]; then
  cat <<'HELP'
`+claudeHelp+`HELP
elif [ "$3" = 'auth' ]; then
  printf '%s\n' '{"loggedIn":true}'
else
  exit 1
fi
`)
	if _, err := Validate(context.Background(), config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "model discovery") {
		t.Fatalf("Validate = %v", err)
	}
}

func TestValidateAcceptsSparseHelp(t *testing.T) {
	fakeExecutable(t, "bwrap", "exit 99\n")
	fakeExecutable(t, "socat", "exit 99\n")
	fakeExecutable(t, "claude", `if [ "$1" = '--help' ]; then
  cat <<'HELP'
`+claudeHelp+`HELP
elif [ "$3" = 'auth' ]; then
  printf '%s\n' '{"loggedIn":true}'
else
  cat >/dev/null
  printf '%s\n' '`+modelCatalogue+`'
fi
`)
	settings, err := Validate(t.Context(), config.ClaudeConfig{})
	if err != nil || settings.Model != "claude-pinned" {
		t.Fatalf("Validate with sparse help = %+v, %v", settings, err)
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
