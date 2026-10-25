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
			if !strings.Contains(fresh, "--no-session-persistence\n") || !strings.Contains(fresh, "--session-id\n") || !strings.Contains(fresh, "--add-dir\n") || !strings.Contains(fresh, "--model\n"+tc.wantModel+"\n") || !strings.Contains(fresh, "--json-schema\n{\"type\":\"object\"}\n") {
				t.Fatalf("fresh capability probe used wrong arguments: %q", fresh)
			}
			if tc.effort != "" && !strings.Contains(fresh, "--effort\n"+tc.effort+"\n") {
				t.Fatalf("fresh capability probe omitted selected effort: %q", fresh)
			}
		})
	}
}

func TestValidateRejectsPaidDiscoveryOutput(t *testing.T) {
	fakeExecutable(t, "claude", `if [ "$3" = auth ]; then
  printf '%s\n' '{"loggedIn":true}'
else
  cat >/dev/null
  printf '%s\n' '`+modelCatalogue+`' '{"type":"result","modelUsage":{"main":{"inputTokens":1}}}'
fi
`)
	if _, err := Validate(t.Context(), config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "model discovery emitted model activity") {
		t.Fatalf("accepted paid event after catalogue: %v", err)
	}
}

func TestValidateRejectsMissingInvocationOptionBeforeModelCall(t *testing.T) {
	for _, option := range []string{"--session-id", "--resume", "--add-dir", "--model", "--effort"} {
		t.Run(option, func(t *testing.T) {
			binary := fakeValidationCLI(t, validationCLIBehavior{rejected: option})
			settings := config.ClaudeConfig{}
			if option == "--effort" {
				settings.Effort = "high"
			}
			_, err := Validate(t.Context(), settings)
			if err == nil || !strings.Contains(err.Error(), option) {
				t.Fatalf("accepted CLI without %s: %v", option, err)
			}
			if _, err := os.Stat(binary + ".model-called"); !os.IsNotExist(err) {
				t.Fatalf("capability check started a model call: %v", err)
			}
		})
	}
}

func TestValidateRejectsCLIThatIgnoresUnknownOption(t *testing.T) {
	fakeValidationCLI(t, validationCLIBehavior{ignoreUnknown: true})
	if _, err := Validate(t.Context(), config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "did not reject an unknown option") {
		t.Fatalf("accepted CLI that ignores unknown options: %v", err)
	}
}

func TestValidateRejectsUnrelatedUnknownOptionFailure(t *testing.T) {
	fakeValidationCLI(t, validationCLIBehavior{unrelatedUnknown: true})
	if _, err := Validate(t.Context(), config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "did not reject an unknown option") {
		t.Fatalf("accepted unrelated startup failure as parser rejection: %v", err)
	}
}

func TestValidateRejectsUnknownOptionAfterModelOutput(t *testing.T) {
	fakeValidationCLI(t, validationCLIBehavior{modelOutputOnUnknown: true})
	if _, err := Validate(t.Context(), config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "did not reject an unknown option") {
		t.Fatalf("accepted model output during parser rejection: %v", err)
	}
}

func TestValidateRejectsUnsupportedResumeDespiteEmptyResult(t *testing.T) {
	fakeValidationCLI(t, validationCLIBehavior{unsupportedResume: true})
	if _, err := Validate(t.Context(), config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "did not reach empty-session lookup") {
		t.Fatalf("accepted unsupported resume as missing-session lookup: %v", err)
	}
}

func TestValidateHonorsCallerCancellation(t *testing.T) {
	fakeValidationCLI(t, validationCLIBehavior{})
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
		`{"type":"user"}` + "\n" + empty,
		`{"type":"stream_event"}` + "\n" + empty,
		`{"type":"system","usage":{"input_tokens":1}}` + "\n" + empty,
		`{"type":"system","modelUsage":{"main":{"inputTokens":0}}}` + "\n" + empty,
		`{"type":"result","is_error":true,"result":"generated answer"}`,
		`{"type":"result","is_error":true,"structured_output":{"answer":"generated answer"}}`,
		`{"type":"result","is_error":true,"num_turns":0,"usage":{"input_tokens":1}}`,
		`{"type":"result","is_error":true,"num_turns":0,"modelUsage":{"main":{"inputTokens":1}}}`,
		`{"type":"result","is_error":false,"num_turns":0}`,
	} {
		if isEmptySessionLookup([]byte(output)) {
			t.Fatalf("accepted model activity or success: %q", output)
		}
	}
}

func TestFreshInitializationRejectsPaidOrMalformedTrailingEvent(t *testing.T) {
	if !cleanFreshInitialization([]byte(modelCatalogue)) {
		t.Fatal("rejected a control-only initialization")
	}
	for _, extra := range []string{
		`{"type":"result","subtype":"success"}`,
		`{"type":"result","num_turns":1}`,
		`{"type":"result","usage":{"input_tokens":1}}`,
		`{"type":"result","total_cost_usd":0.01}`,
		`{"type":"result","modelUsage":{"main":{"inputTokens":3}}}`,
		`{"type":"result","modelUsage":{"main":{"inputTokens":0}}}`,
		`{"type":"result","subtype":"success","result":"generated answer"}`,
		`{"type":"result","subtype":"success","structured_output":{"answer":"generated answer"}}`,
		`{"type":"system","usage":{"input_tokens":1}}`,
		`{"type":"assistant"}`,
		`{"type":"user"}`,
		`{"type":"stream_event"}`,
		`{`,
		`null`,
	} {
		if cleanFreshInitialization([]byte(modelCatalogue + "\n" + extra)) {
			t.Fatalf("accepted trailing model activity or malformed event: %s", extra)
		}
	}
}

type validationCLIBehavior struct {
	rejected             string
	ignoreUnknown        bool
	unrelatedUnknown     bool
	modelOutputOnUnknown bool
	unsupportedResume    bool
}

func fakeValidationCLI(t *testing.T, behavior validationCLIBehavior) string {
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
	if behavior.unrelatedUnknown {
		unknown = `case " $* " in *' --minion-unsupported-capability-probe '*) printf '%s\n' 'unrelated startup failure' >&2; exit 2 ;; esac`
	} else if behavior.modelOutputOnUnknown {
		unknown = `case " $* " in *' --minion-unsupported-capability-probe '*) printf '%s\n' '{"type":"assistant"}'; printf '%s\n' 'unknown option --minion-unsupported-capability-probe' >&2; exit 2 ;; esac`
	} else if behavior.ignoreUnknown {
		unknown = ""
	}
	reject := ""
	if behavior.rejected != "" {
		reject = `case " $* " in *' ` + behavior.rejected + ` '*) printf '%s\n' 'unsupported ` + behavior.rejected + `' >&2; exit 2 ;; esac`
	}
	resumeError := `printf 'No conversation found with session ID: %s\n' "$id" >&2`
	if behavior.unsupportedResume {
		resumeError = `printf 'unsupported --resume for session %s\n' "$id" >&2`
	}
	dir := testsupport.FakeOnPath(t, "claude", `#!/bin/sh
if [ "$3" = auth ]; then
  printf '%s\n' '{"loggedIn":true}'
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
      `+resumeError+`
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
