package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/config"
)

const catalogueScript = `#!/bin/sh
if [ "$1" = exec ]; then
  case " $* " in *' --help '*) ;; *) exit 9 ;; esac
  printf '%s\n' '--invocation--' "$@" >> @HELP_ARGS@
  printf '%s,%s,%s,%s\n' "${GH_TOKEN-absent}" "${GITHUB_TOKEN-absent}" "${CODEX_API_KEY-absent}" "${CODEX_ACCESS_TOKEN-absent}" >> @HELP_ENV@
  case " $* " in *' --minion-unsupported-capability-probe '*) exit 2 ;; esac
  if [ "$2" = resume ]; then
    printf '%s\n' 'Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]'
  else
    printf '%s\n' 'Usage: codex exec [OPTIONS] [PROMPT]'
  fi
  exit 0
fi
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
		"@HELP_ARGS@", aicli.ShellQuote(filepath.Join(dir, "help-args")),
		"@HELP_ENV@", aicli.ShellQuote(filepath.Join(dir, "help-env")),
		"@REQUESTS@", aicli.ShellQuote(filepath.Join(dir, "requests")),
		"@PAGE1@", aicli.ShellQuote(page1),
		"@PAGE2@", aicli.ShellQuote(page2),
	).Replace(catalogueScript)
}

func TestValidateRejectsUnsupportedDisabledFeature(t *testing.T) {
	dir := t.TempDir()
	script := catalogueScriptFor(dir, "", "")
	script = strings.Replace(script, "#!/bin/sh\n", "#!/bin/sh\ncase \" $* \" in *' --disable apps '*) printf '%s\\n' 'Unknown feature flag: apps' >&2; exit 2 ;; esac\n", 1)
	installFakeCodex(t, script)
	if _, err := Validate(t.Context(), config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "Unknown feature flag: apps") {
		t.Fatalf("accepted unsupported disabled feature: %v", err)
	}
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
			settings, err := Validate(t.Context(), config.CodexConfig{Model: tt.model, ReasoningEffort: tt.effort, Skills: []string{"/example/SKILL.md"}})
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Validate() error = %v, want %q", err, tt.wantError)
				}
			} else if err != nil || settings.Model != tt.wantModel || settings.ReasoningEffort != tt.wantEffort || len(settings.Skills) != 1 {
				t.Fatalf("Validate() = %+v, %v", settings, err)
			}
			args, err := os.ReadFile(filepath.Join(dir, "args"))
			if err != nil || !strings.HasPrefix(string(args), "app-server\n") || !strings.Contains(string(args), `model_provider="openai"`) || !strings.Contains(string(args), `sandbox_mode="workspace-write"`) || !strings.Contains(string(args), "sandbox_workspace_write.exclude_slash_tmp=true") || !strings.Contains(string(args), "sandbox_workspace_write.exclude_tmpdir_env_var=true") || !strings.Contains(string(args), "sandbox_workspace_write.writable_roots=[") || !strings.Contains(string(args), "--disable\napps\n") {
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

func TestValidateRejectsUnsupportedWorkspaceWriteConfig(t *testing.T) {
	dir := t.TempDir()
	page := `{"id":2,"result":{"data":[{"model":"fixture","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}`
	script := strings.Replace(catalogueScriptFor(dir, page, ""), "#!/bin/sh\n", `#!/bin/sh
if [ "$1" = app-server ]; then
  case " $* " in *' sandbox_workspace_write.writable_roots='*) printf '%s\n' 'unknown config key: sandbox_workspace_write.writable_roots' >&2; exit 2 ;; esac
fi
`, 1)
	installFakeCodex(t, script)
	if _, err := Validate(t.Context(), config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "unknown config key: sandbox_workspace_write.writable_roots") {
		t.Fatalf("accepted unsupported workspace-write config: %v", err)
	}
}

func TestValidateChecksExecAndResumeCapabilitiesWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	page := `{"id":2,"result":{"data":[{"model":"fixture","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}`
	installFakeCodex(t, catalogueScriptFor(dir, page, ""))
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "host-secret")
	}
	if _, err := Validate(t.Context(), config.CodexConfig{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "help-args"))
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimPrefix(string(data), "--invocation--\n"), "--invocation--\n")
	if len(calls) != 4 {
		t.Fatalf("expected fresh/resume help and rejection probes, got %q", data)
	}
	flags := []string{"--json", "--ignore-user-config", "--ignore-rules", "--strict-config", "--skip-git-repo-check"}
	for _, feature := range []string{
		"apps", "browser_use", "browser_use_external", "browser_use_full_cdp_access",
		"computer_use", "image_generation", "plugins", "remote_plugin",
		"shell_snapshot", "skill_mcp_dependency_install", "memories", "hooks", "skill_search",
	} {
		flags = append(flags, "--disable", feature)
	}
	flags = append(flags, "-c", "skills.include_instructions=false", "-c", "approval_policy=never", "--model", "fixture", "--output-schema")
	for i, command := range [][]string{{"exec"}, {"exec", "resume"}} {
		got := strings.Split(strings.TrimSpace(calls[2*i]), "\n")
		if len(got) < 2 || !filepath.IsAbs(got[len(got)-2]) || filepath.Base(got[len(got)-2]) != "output-schema.json" {
			t.Fatalf("probe %d omitted a temporary output schema: %q", i, got)
		}
		want := append(append(slices.Clone(command), flags[:5]...), "-c", `sandbox_mode="read-only"`)
		want = append(want, flags[5:]...)
		want = append(want, got[len(got)-2], "--help")
		if !slices.Equal(got, want) {
			t.Fatalf("probe %d args = %q; want %q", i, got, want)
		}
		rejected := strings.Split(strings.TrimSpace(calls[2*i+1]), "\n")
		wantRejected := append(append([]string(nil), command...), "--minion-unsupported-capability-probe", "--help")
		if !slices.Equal(rejected, wantRejected) {
			t.Fatalf("negative probe %d args = %q; want %q", i, rejected, wantRejected)
		}
	}
	env, err := os.ReadFile(filepath.Join(dir, "help-env"))
	if err != nil || string(env) != strings.Repeat("absent,absent,absent,absent\n", 4) {
		t.Fatalf("credentials reached capability probe: %q, %v", env, err)
	}
}

func TestValidateRejectsUnsupportedExecResumeFlag(t *testing.T) {
	dir := t.TempDir()
	page := `{"id":2,"result":{"data":[{"model":"fixture","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}`
	script := catalogueScriptFor(dir, page, "")
	script = strings.Replace(script, "if [ \"$1\" = exec ]; then", "if [ \"$1\" = exec ] && [ \"$2\" = resume ]; then\n  case \" $* \" in *' --ignore-rules '*) printf '%s\\n' \"unexpected argument '--ignore-rules'\" >&2; exit 2 ;; esac\nfi\nif [ \"$1\" = exec ]; then", 1)
	installFakeCodex(t, script)
	if _, err := Validate(t.Context(), config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "Codex exec resume capability check") || !strings.Contains(err.Error(), "--ignore-rules") {
		t.Fatalf("accepted unsupported resume flag: %v", err)
	}
}

func TestValidateRejectsMissingOutputSchemaSupport(t *testing.T) {
	dir := t.TempDir()
	page := `{"id":2,"result":{"data":[{"model":"fixture","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}`
	script := catalogueScriptFor(dir, page, "")
	script = strings.Replace(script, "if [ \"$1\" = exec ]; then", "if [ \"$1\" = exec ]; then\n  case \" $* \" in *' --output-schema '*) printf '%s\\n' \"unexpected argument '--output-schema'\" >&2; exit 2 ;; esac\nfi\nif [ \"$1\" = exec ]; then", 1)
	installFakeCodex(t, script)
	if _, err := Validate(t.Context(), config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "Codex exec capability check") || !strings.Contains(err.Error(), "--output-schema") {
		t.Fatalf("accepted CLI without output schema support: %v", err)
	}
}

func TestValidateRejectsHelpThatIgnoresUnknownOptions(t *testing.T) {
	dir := t.TempDir()
	page := `{"id":2,"result":{"data":[{"model":"fixture","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}`
	script := strings.Replace(catalogueScriptFor(dir, page, ""), "case \" $* \" in *' --minion-unsupported-capability-probe '*) exit 2 ;; esac", "", 1)
	installFakeCodex(t, script)
	if _, err := Validate(t.Context(), config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "Codex exec capability check did not reject an unknown option") {
		t.Fatalf("accepted help that ignored an unknown option: %v", err)
	}
}

func TestValidateAcceptsCompatibleExecHelpWithDifferentLabels(t *testing.T) {
	dir := t.TempDir()
	page := `{"id":2,"result":{"data":[{"model":"fixture","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}`
	script := strings.NewReplacer(
		"Usage: codex exec [OPTIONS] [PROMPT]", "Run an action",
		"Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]", "Continue an action",
	).Replace(catalogueScriptFor(dir, page, ""))
	installFakeCodex(t, script)
	if _, err := Validate(t.Context(), config.CodexConfig{}); err != nil {
		t.Fatalf("rejected compatible help labels: %v", err)
	}
}

func TestExecCapabilityProbeHonorsCallerDeadline(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\nexec sleep 10\n")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := validateExecCapabilities(ctx, config.CodexConfig{Model: "fixture"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("capability probe lost deadline: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("capability probe did not stop after deadline")
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
			if _, err := Validate(t.Context(), config.CodexConfig{Model: "gpt-6-sol", ReasoningEffort: "ultra"}); err == nil {
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
	settings, err := Validate(t.Context(), config.CodexConfig{APIKey: "dummy-configured-key"})
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
			_, err := Validate(t.Context(), config.CodexConfig{APIKey: "dummy-configured-key"})
			if err == nil || strings.Contains(err.Error(), "dummy-configured-key") {
				t.Fatalf("configured login must fail without exposing the key: %v", err)
			}
		})
	}
}

func TestValidateReportsCLIExitAndHonorsTimeout(t *testing.T) {
	t.Run("unsupported CLI flag", func(t *testing.T) {
		installFakeCodex(t, "#!/bin/sh\nprintf '%s\\n' 'unsupported flag' >&2\nexit 2\n")
		if _, err := Validate(t.Context(), config.CodexConfig{}); err == nil || !strings.Contains(err.Error(), "unsupported flag") {
			t.Fatalf("missing CLI failure: %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		installFakeCodex(t, "#!/bin/sh\nexec sleep 10\n")
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
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

func TestModelDiscoveryCancellationUnblocksScanner(t *testing.T) {
	home := canonicalTempDir(t)
	marker := filepath.Join(home, "started")
	bin := t.TempDir()
	source := filepath.Join(bin, "fake.go")
	if err := os.WriteFile(source, []byte(`package main
import (
	"os"
	"path/filepath"
	"time"
)
func main() {
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "started"), []byte("x"), 0600); err != nil {
		os.Exit(1)
	}
	time.Sleep(8 * time.Second)
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if output, err := exec.Command("go", "build", "-o", filepath.Join(bin, name), source).CombinedOutput(); err != nil {
		t.Fatalf("build fake Codex: %v: %s", err, output)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", home)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := listModels(ctx, config.CodexConfig{})
		done <- err
	}()
	startDeadline := time.NewTimer(2 * time.Second)
	defer startDeadline.Stop()
	checkStarted := time.NewTicker(time.Millisecond)
	defer checkStarted.Stop()
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("discovery exited before scanner wait: %v", err)
		case <-startDeadline.C:
			cancel()
			t.Fatal("discovery process did not start")
		case <-checkStarted.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation cause lost: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("canceled discovery scanner remained blocked")
	}
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
	if _, err := Validate(t.Context(), config.CodexConfig{}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Validate() = %v; want cleanup permission error", err)
	}
}

func TestModelPreflightQuotesStderr(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\nprintf 'private\\nforged journal line' >&2\nprintf '%s\\n' '{not json'\n")
	_, err := listModels(t.Context(), config.CodexConfig{})
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
	_, err = Validate(t.Context(), config.CodexConfig{})
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
				models, err := listModels(t.Context(), config.CodexConfig{})
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
			if _, err := Validate(t.Context(), config.CodexConfig{APIKey: key}); err != nil {
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
	_, err = Validate(t.Context(), config.CodexConfig{APIKey: key})
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
				_, err = Validate(t.Context(), config.CodexConfig{APIKey: key})
				if err == nil || err.Error() != `cannot validate Codex settings: Codex account/login/start: "provider rejected [redacted]"` {
					t.Fatalf("provider error did not redact the configured key: %v", err)
				}
			})
		}
	}
}
