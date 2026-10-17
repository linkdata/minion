package claudecli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

const modelCatalogue = `{"type":"control_response","response":{"subtype":"success","request_id":"minion-initialize","response":{"models":[{"value":"unresolved"},{"value":"default","resolvedModel":"claude-pinned","supportedEffortLevels":["low","high"]},{"value":"quick","resolvedModel":"claude-small","supportedEffortLevels":["low","high"]}]}}}`

func TestValidate(t *testing.T) {
	fakeExecutable(t, "bwrap", "exit 99\n")
	fakeExecutable(t, "socat", "exit 99\n")
	fakeExecutable(t, "claude", `if [ "$1" = '--version' ]; then
  printf '%s\n' '2.1.302 (Claude Code)'
elif [ "$4" = 'auth' ]; then
  printf '%s\n' '{"loggedIn":true}'
else
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
			cfg, err := Validate(context.Background(), config.ClaudeConfig{Model: tc.model, Effort: tc.effort})
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
	fakeExecutable(t, "claude", `if [ "$1" = '--version' ]; then
  printf '%s\n' '2.1.302 (Claude Code)'
elif [ "$4" = 'auth' ]; then
  printf '%s\n' '{"loggedIn":true}'
else
  exit 1
fi
`)
	if _, err := Validate(context.Background(), config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "model discovery") {
		t.Fatalf("Validate = %v", err)
	}
}

func TestSupportedVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"2.1.302 (Claude Code)", true},
		{"2.1.303", true},
		{"3.0.0", true},
		{"2.1.301", false},
		{"2.1.295", false},
		{"2.0.999", false},
		{"", false},
		{"3.x.y", false},
		{"3.0", false},
		{"2.-1.295", false},
	} {
		if got := supportedVersion(tc.version); got != tc.want {
			t.Errorf("supportedVersion(%q) = %v, want %v", tc.version, got, tc.want)
		}
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

func TestProbeRetainsOutputLimitAndQuotesDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		limited      bool
	}{
		{"output cap", "head -c 4194305 /dev/zero\nsleep 30\n", true},
		{"diagnostic", "printf 'first\\nforged journal line' >&2\nexit 7\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := fakeExecutable(t, "probe", tc.script)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := probe(ctx, binary, nil, t.TempDir(), os.Environ(), "")
			if tc.limited {
				if !errors.Is(err, aicli.ErrOutputLimit) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("limit cause lost: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), `first\nforged journal line`) || strings.Contains(err.Error(), "\nforged journal line") {
				t.Fatalf("unquoted diagnostic: %v", err)
			}
		})
	}
}

func TestProbeExactBoundAndCallerCancellation(t *testing.T) {
	binary := fakeExecutable(t, "probe", "head -c 4194304 /dev/zero\n")
	output, err := probe(context.Background(), binary, nil, t.TempDir(), os.Environ(), "")
	if err != nil || len(output) != maxEventBytes {
		t.Fatalf("exact bound bytes=%d error=%v", len(output), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = probe(ctx, binary, nil, t.TempDir(), os.Environ(), ""); !errors.Is(err, context.Canceled) || errors.Is(err, aicli.ErrOutputLimit) {
		t.Fatalf("caller cancellation cause changed: %v", err)
	}
}
