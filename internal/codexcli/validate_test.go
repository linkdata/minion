package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/config"
)

const catalogueScript = `#!/bin/sh
printf '%s\n' "$@" > @ARGS@
printf '%s,%s\n' "${GH_TOKEN-absent}" "${GITHUB_TOKEN-absent}" > @ENV@
IFS= read -r request
printf '%s\n' "$request" >> @REQUESTS@
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r request
printf '%s\n' "$request" >> @REQUESTS@
IFS= read -r request
printf '%s\n' "$request" >> @REQUESTS@
printf '%s\n' '{"method":"test/notification","params":{}}' @PAGE1@
if [ -n @PAGE2@ ]; then
  IFS= read -r request
  printf '%s\n' "$request" >> @REQUESTS@
  printf '%s\n' @PAGE2@
fi
cat >/dev/null
`

func catalogueScriptFor(dir, page1, page2 string) string {
	return strings.NewReplacer(
		"@ARGS@", aicli.ShellQuote(filepath.Join(dir, "args")),
		"@ENV@", aicli.ShellQuote(filepath.Join(dir, "env")),
		"@REQUESTS@", aicli.ShellQuote(filepath.Join(dir, "requests")),
		"@PAGE1@", aicli.ShellQuote(page1),
		"@PAGE2@", aicli.ShellQuote(page2),
	).Replace(catalogueScript)
}

func TestValidateModelAndEffortAgainstInstalledCatalogue(t *testing.T) {
	for _, tt := range []struct {
		name, model, effort, wantModel, wantEffort, wantError string
	}{
		{name: "explicit model on second page", model: "gpt-6-sol", effort: "ultra", wantModel: "gpt-6-sol", wantEffort: "ultra"},
		{name: "defaults pinned", wantModel: "gpt-6-luna", wantEffort: "medium"},
		{name: "model default effort", model: "gpt-6-sol", wantModel: "gpt-6-sol", wantEffort: "medium"},
		{name: "effort on default model", effort: "high", wantModel: "gpt-6-luna", wantEffort: "high"},
		{name: "removed model", model: "removed-model", effort: "ultra", wantError: `codex.model "removed-model" is not recognized`},
		{name: "removed effort", model: "gpt-6-sol", effort: "removed-effort", wantError: `supported efforts: medium, ultra`},
		{name: "effort belongs to another model", model: "gpt-6-luna", effort: "ultra", wantError: `codex.reasoning_effort "ultra" is not supported for model "gpt-6-luna"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			page1 := `{"id":2,"result":{"data":[{"model":"gpt-6-luna","isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium"},{"reasoningEffort":"high"}]}],"nextCursor":"second"}}`
			page2 := `{"id":3,"result":{"data":[{"model":"gpt-6-sol","hidden":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium"},{"reasoningEffort":"ultra"}]}],"nextCursor":null}}`
			installFakeCodex(t, catalogueScriptFor(dir, page1, page2))
			t.Setenv("GH_TOKEN", "secret")
			t.Setenv("GITHUB_TOKEN", "secret")
			settings, err := Validate(context.Background(), config.CodexConfig{Model: tt.model, ReasoningEffort: tt.effort, Skills: []string{"/example/SKILL.md"}})
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Validate() error = %v, want %q", err, tt.wantError)
				}
			} else if err != nil || settings.Model != tt.wantModel || settings.ReasoningEffort != tt.wantEffort || len(settings.Skills) != 1 {
				t.Fatalf("Validate() = %+v, %v", settings, err)
			}
			args, err := os.ReadFile(filepath.Join(dir, "args"))
			if err != nil || !strings.HasPrefix(string(args), "app-server\n") || !strings.Contains(string(args), `model_provider="openai"`) {
				t.Fatalf("wrong discovery command: %s, %v", args, err)
			}
			env, err := os.ReadFile(filepath.Join(dir, "env"))
			if err != nil || strings.TrimSpace(string(env)) != "absent,absent" {
				t.Fatalf("GitHub credentials reached model discovery: %s, %v", env, err)
			}
			requests, err := os.ReadFile(filepath.Join(dir, "requests"))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(requests)), "\n")
			if len(lines) != 4 {
				t.Fatalf("expected handshake and two catalogue pages, got %s", requests)
			}
			var last struct {
				Method string `json:"method"`
				Params struct {
					Cursor        string `json:"cursor"`
					IncludeHidden bool   `json:"includeHidden"`
				} `json:"params"`
			}
			if err := json.Unmarshal([]byte(lines[3]), &last); err != nil || last.Method != "model/list" || last.Params.Cursor != "second" || !last.Params.IncludeHidden {
				t.Fatalf("wrong catalogue pagination: %+v, %v", last, err)
			}
		})
	}
}

func TestValidateFailsWhenCodexCannotSupplyCatalogue(t *testing.T) {
	for _, response := range []string{
		`{"id":2,"error":{"message":"Unknown method model/list"}}`,
		`{"id":2,"result":{"data":[]}}`,
		`{"id":2,"result":null}`,
		`{"id":2,"result":{"data":[{"model":"gpt-6-sol"}]}}`,
		`{"id":2,"result":{"data":[{"unexpectedNewField":"gpt-6-sol"}]}}`,
		`not-json`,
	} {
		t.Run(response, func(t *testing.T) {
			dir := t.TempDir()
			installFakeCodex(t, catalogueScriptFor(dir, response, ""))
			if _, err := Validate(context.Background(), config.CodexConfig{Model: "gpt-6-sol", ReasoningEffort: "ultra"}); err == nil {
				t.Fatalf("accepted unvalidated settings after response %s", response)
			}
		})
	}
}

func TestValidateUsesConfiguredAPIKeyWithoutPersistingLogin(t *testing.T) {
	dir := t.TempDir()
	page := `{"id":2,"result":{"data":[{"model":"test","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}`
	script := strings.Replace(catalogueScriptFor(dir, page, ""), "printf '%s\\n' '{\"method\":\"test/notification\",\"params\":{}}'", `printf '%s,%s\n' "${CODEX_API_KEY-absent}" "${CODEX_ACCESS_TOKEN-absent}" > "$0.auth"
printf '%s\n' '{"id":0,"result":{"type":"apiKey"}}'
IFS= read -r request
printf '%s\n' "$request" >> "$0.model-request"
printf '%s\n' '{"method":"test/notification","params":{}}'`, 1)
	fake := installFakeCodex(t, script)
	t.Setenv("CODEX_API_KEY", "dummy-environment-key")
	t.Setenv("CODEX_ACCESS_TOKEN", "dummy-environment-access-token")
	settings, err := Validate(context.Background(), config.CodexConfig{APIKey: "dummy-configured-key"})
	if err != nil || settings.Model != "test" || settings.APIKey != "dummy-configured-key" {
		t.Fatalf("Validate() failed to preserve configured authentication: %v", err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil || !strings.Contains(string(args), `cli_auth_credentials_store="ephemeral"`) || !strings.Contains(string(args), `forced_login_method="api"`) || strings.Contains(string(args), settings.APIKey) {
		t.Fatalf("wrong configured authentication arguments: %v", err)
	}
	auth, err := os.ReadFile(fake + ".auth")
	if err != nil || string(auth) != "absent,absent\n" {
		t.Fatalf("ambient credentials reached discovery: %q, %v", auth, err)
	}
	requests, err := os.ReadFile(filepath.Join(dir, "requests"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(requests)), "\n")
	var login struct {
		Method string `json:"method"`
		Params struct {
			Type   string `json:"type"`
			APIKey string `json:"apiKey"`
		} `json:"params"`
	}
	if len(lines) != 3 {
		t.Fatalf("expected handshake and login; got %d requests", len(lines))
	}
	if err := json.Unmarshal([]byte(lines[2]), &login); err != nil || login.Method != "account/login/start" || login.Params.Type != "apiKey" || login.Params.APIKey != settings.APIKey {
		t.Fatalf("wrong configured login request: %v", err)
	}
}

func TestValidateRejectsFailedConfiguredLogin(t *testing.T) {
	for _, response := range []string{
		`{"id":0,"error":{"message":"dummy-configured-key"}}`,
		`{"id":0,"result":{"type":"chatgpt"}}`,
	} {
		t.Run(response, func(t *testing.T) {
			dir := t.TempDir()
			installFakeCodex(t, catalogueScriptFor(dir, response, ""))
			_, err := Validate(context.Background(), config.CodexConfig{APIKey: "dummy-configured-key"})
			if err == nil || strings.Contains(err.Error(), "dummy-configured-key") {
				t.Fatalf("configured login must fail without exposing the key: %v", err)
			}
		})
	}
}

func TestValidateReportsCLIExitAndHonorsTimeout(t *testing.T) {
	t.Run("unsupported CLI flag", func(t *testing.T) {
		installFakeCodex(t, "#!/bin/sh\nprintf '%s\\n' 'unsupported flag' >&2\nexit 2\n")
		if _, err := Validate(context.Background(), config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "unsupported flag") {
			t.Fatalf("missing CLI failure: %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		installFakeCodex(t, "#!/bin/sh\nexec sleep 10\n")
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		started := time.Now()
		if _, err := Validate(ctx, config.CodexConfig{}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected deadline exceeded, got %v", err)
		}
		if time.Since(started) > 3*time.Second {
			t.Fatal("model discovery did not stop after timeout")
		}
	})
}

func TestValidateReportsCleanupFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	page1 := `{"id":2,"result":{"data":[{"model":"test","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}`
	script := catalogueScriptFor(dir, page1, "")
	installFakeCodex(t, strings.Replace(script, "IFS= read -r request", "mkdir locked\ntouch locked/file\nchmod 000 locked\nIFS= read -r request", 1))
	t.Cleanup(func() {
		paths, err := filepath.Glob(filepath.Join(dir, "minion-codex-check-*", "locked"))
		if err != nil {
			t.Error(err)
		}
		for _, path := range paths {
			if err := os.Chmod(path, 0o700); err != nil {
				t.Error(err)
			}
		}
	})
	// Go 1.23 predates testing.T.Context.
	if _, err := Validate(context.Background(), config.CodexConfig{}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Validate() = %v; want cleanup permission error", err)
	}
}

func TestModelPreflightQuotesStderr(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\nprintf 'private\\nforged journal line' >&2\nprintf '%s\\n' '{not json'\n")
	_, err := listModels(context.Background(), config.CodexConfig{})
	if err == nil || strings.Contains(err.Error(), "\nforged journal line") || !strings.Contains(err.Error(), `private\nforged journal line`) {
		t.Fatalf("unsafe preflight stderr: %v", err)
	}
}

func TestCatalogueProviderErrorIsBoundedAndQuoted(t *testing.T) {
	dir := t.TempDir()
	message := "first\nforged journal line " + strings.Repeat("x", 10000)
	response, err := json.Marshal(map[string]any{"id": 2, "error": map[string]string{"message": message}})
	if err != nil {
		t.Fatal(err)
	}
	installFakeCodex(t, catalogueScriptFor(dir, string(response), ""))
	_, err = Validate(context.Background(), config.CodexConfig{})
	if err == nil || strings.Contains(err.Error(), "\nforged journal line") || !strings.Contains(err.Error(), `first\nforged journal line`) || len(err.Error()) > bounded.DiagnosticBytes+200 || !strings.Contains(err.Error(), "...") {
		t.Fatalf("unsafe/unbounded provider error: %v", err)
	}
}

func TestCatalogueEventAcceptsExactLimitAndRejectsNextByte(t *testing.T) {
	const prefix = `{"id":2,"result":{"data":[{"model":"fixture","isDefault":true}]},"padding":"`
	const suffix = `"}`
	for _, size := range []int{aicli.MaxEventBytes, aicli.MaxEventBytes + 1, aicli.MaxEventBytes + 10} {
		for _, ending := range []string{"\n", "\r\n", ""} {
			t.Run(fmt.Sprintf("bytes%d/ending%q", size, ending), func(t *testing.T) {
				binary := installFakeCodex(t, "#!/bin/sh\nIFS= read -r request\nprintf '%s\\n' '{\"id\":1,\"result\":{}}'\nIFS= read -r request\nIFS= read -r request\ncat \"$0.response\"\n")
				response := prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix + ending
				if err := os.WriteFile(binary+".response", []byte(response), 0600); err != nil {
					t.Fatal(err)
				}
				models, err := listModels(context.Background(), config.CodexConfig{})
				if size == aicli.MaxEventBytes {
					if err != nil || len(models) != 1 || models[0].Model != "fixture" {
						t.Fatalf("exact-cap catalogue=%+v err=%v", models, err)
					}
				} else if !errors.Is(err, aicli.ErrOutputLimit) {
					t.Fatalf("oversized catalogue accepted or unclassified: models=%+v err=%v", models, err)
				}
			})
		}
	}
}

func TestValidateKeepsImplicitNativeHome(t *testing.T) {
	for _, key := range []string{"", "fixture-key"} {
		t.Run(fmt.Sprint(key != ""), func(t *testing.T) {
			dir := t.TempDir()
			page := `{"id":2,"result":{"data":[{"model":"fixture","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}`
			script := catalogueScriptFor(dir, page, "")
			if key != "" {
				script = strings.Replace(script, "printf '%s\\n' '{\"method\":\"test/notification\",\"params\":{}}'", "printf '%s\\n' '{\"id\":0,\"result\":{\"type\":\"apiKey\"}}'\nIFS= read -r request\nprintf '%s\\n'", 1)
			}
			script = strings.Replace(script, "#!/bin/sh\n", "#!/bin/sh\n[ \"${CODEX_HOME+x}\" != x ] || exit 3\n[ ! -e \"$HOME/.codex\" ] || exit 4\n", 1)
			installFakeCodex(t, script)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CODEX_HOME", "")
			if _, err := Validate(context.Background(), config.CodexConfig{APIKey: key}); err != nil {
				t.Fatalf("model discovery injected a missing implicit home: %v", err)
			}
		})
	}
}

func TestValidateRedactsKeyBeforeClippingCatalogueError(t *testing.T) {
	key := "KEYPREFIX__LEAKSUFFIX"
	message := strings.Repeat("x", bounded.DiagnosticBytes-len(key)+2) + key + strings.Repeat("y", 300)
	response, err := json.Marshal(map[string]any{"id": 0, "error": map[string]string{"message": message}})
	if err != nil {
		t.Fatal(err)
	}
	installFakeCodex(t, catalogueScriptFor(t.TempDir(), string(response), ""))
	_, err = Validate(context.Background(), config.CodexConfig{APIKey: key})
	if err == nil || strings.Contains(err.Error(), "KEYPREFIX__LEAK") || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "y...") || len(err.Error()) > bounded.DiagnosticBytes+200 {
		t.Fatalf("clipped catalogue error exposed key fragment or lost bound: %v", err)
	}
}

func TestValidateRedactsQuotedConfiguredKeyFromProviderError(t *testing.T) {
	for _, key := range []string{`fixture"secret`, `fixture\secret`} {
		for _, escaped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/escaped=%t", key, escaped), func(t *testing.T) {
				dir := t.TempDir()
				message := "provider rejected " + key
				if escaped {
					encoded, err := json.Marshal(key)
					if err != nil {
						t.Fatal(err)
					}
					message = "provider rejected " + string(encoded[1:len(encoded)-1])
				}
				response, err := json.Marshal(map[string]any{"id": 0, "error": map[string]string{"message": message}})
				if err != nil {
					t.Fatal(err)
				}
				installFakeCodex(t, catalogueScriptFor(dir, string(response), ""))
				_, err = Validate(context.Background(), config.CodexConfig{APIKey: key})
				if err == nil || err.Error() != `cannot validate Codex settings: Codex account/login/start: "provider rejected [redacted]"` {
					t.Fatalf("provider error did not redact the configured key: %v", err)
				}
			})
		}
	}
}
