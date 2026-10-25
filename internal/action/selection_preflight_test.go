package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
)

func requirePreflightMessage(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want || !validPreflightMessage(err.Error()) {
		t.Fatalf("preflight error = %v, want exact safe message %q", err, want)
	}
}

func selectedPreflightRunner(t *testing.T, commands map[string]config.CommandMapping) *Runner {
	t.Helper()
	runner, err := NewSelectedRunner(Runner{WorkRoot: t.TempDir()}, WorkerSelection{
		Commands: commands,
		Main:     RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestPreflightSelectionRequiresFrozenSelectionAndWorkRoot(t *testing.T) {
	var missing *Runner
	requirePreflightMessage(t, missing.PreflightSelection(t.Context()), preflightRunnerUnavailable)
	requirePreflightMessage(t, (&Runner{WorkRoot: t.TempDir()}).PreflightSelection(t.Context()), preflightSelectionUnavailable)
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
	})
	runner.WorkRoot = ""
	requirePreflightMessage(t, runner.PreflightSelection(t.Context()), preflightWorkRootUnavailable)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := runner.PreflightSelection(ctx)
	// The missing work root takes precedence over cancellation.
	requirePreflightMessage(t, err, preflightWorkRootUnavailable)
	runner.WorkRoot = t.TempDir()
	err = runner.PreflightSelection(ctx)
	requirePreflightMessage(t, err, preflightCanceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled preflight lost cause: %v", err)
	}
}

func TestPreflightSelectionRejectsUnsupportedEvidenceDirectoriesBeforePull(t *testing.T) {
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
	})
	for _, provider := range []string{"codex", "claude"} {
		if provider == "codex" {
			runner.Codex.ReadOnlyDirs = []string{"/"}
		} else {
			runner.Codex.ReadOnlyDirs = nil
			runner.Claude.ReadOnlyDirs = []string{"/"}
		}
		err := runner.PreflightSelection(t.Context())
		requirePreflightMessage(t, err, preflightEvidenceDirsUnsupported)
		if !errors.Is(err, errUnsupportedEvidenceDirs) {
			t.Fatalf("%s evidence directory cause was lost: %v", provider, err)
		}
	}
}

func TestPreflightSelectionRejectsInvalidWorkRootBeforeToolChecks(t *testing.T) {
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
	})
	file, err := os.CreateTemp(t.TempDir(), "work-root-")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	runner.WorkRoot = file.Name()
	requirePreflightMessage(t, runner.PreflightSelection(t.Context()), preflightWorkRootInvalid)
	if runtime.GOOS != "windows" {
		runner.WorkRoot = "/"
		requirePreflightMessage(t, runner.PreflightSelection(t.Context()), preflightWorkRootInvalid)
	}
}

func TestPreflightSelectionChecksEverySelectedRequiredSkill(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
		"fix":    {Action: string(command.Fix), Skills: []string{testFixSkillURL}},
	})
	err := runner.PreflightSelection(t.Context())
	requirePreflightMessage(t, err, preflightCommandSkillUnavailable)
	if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), "not installed") {
		t.Fatalf("missing selected skill cause = %v", cause)
	}
	if len(runner.Codex.Skills) != 0 || runner.selection.Commands["fix"].Skills[0] != testFixSkillURL {
		t.Fatal("preflight changed runner or frozen selection")
	}
}

func TestPreflightSelectionChecksOnlySelectedAuditTools(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL, testAuditSkillURL)
	skillDir := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "security-audit")
	if err := os.Remove(filepath.Join(skillDir, "validate-findings.cjs")); err != nil {
		t.Fatal(err)
	}
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
		"audit":  {Action: string(command.SecurityAudit), Skills: []string{testAuditSkillURL}},
	})
	err := runner.PreflightSelection(t.Context())
	requirePreflightMessage(t, err, preflightAuditToolchainUnavailable)
	if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), "validate-findings.cjs") {
		t.Fatalf("selected audit toolchain cause = %v", cause)
	}
	assertEmptyDir(t, runner.WorkRoot)

	reviewOnly := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
	})
	t.Setenv("PATH", t.TempDir())
	err = reviewOnly.PreflightSelection(t.Context())
	requirePreflightMessage(t, err, preflightGitReadinessUnavailable)
}

func TestPreflightSelectionRequiresAuditInCommandMapping(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL, testAuditSkillURL)
	runner, err := NewSelectedRunner(Runner{WorkRoot: t.TempDir()}, WorkerSelection{
		Commands: map[string]config.CommandMapping{
			"audit": {Action: string(command.SecurityAudit), Skills: []string{testReviewSkillURL}},
		},
		Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high", AlwaysOnSkills: []string{testAuditSkillURL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	requirePreflightMessage(t, runner.PreflightSelection(t.Context()), preflightAuditSkillUnavailable)
}

func TestPreflightSelectionNeverDisplaysRawCauses(t *testing.T) {
	const key = "preflight-secret-key"
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{"https://github.com/o/r/tree/main/skills/" + key}},
	})
	runner.Codex.APIKey = key
	err := runner.PreflightSelection(t.Context())
	requirePreflightMessage(t, err, preflightCommandSkillUnavailable)
	if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), key) {
		t.Fatalf("preflight lost raw skill cause: %v", cause)
	}
	if runtime.GOOS == "windows" {
		t.Skip("oversized path errors differ on Windows")
	}
	runner = selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
	})
	runner.Codex.APIKey = key
	runner.WorkRoot = filepath.Join(t.TempDir(), strings.Repeat("a", 100)+key+strings.Repeat("b", 12000))
	err = runner.PreflightSelection(t.Context())
	requirePreflightMessage(t, err, preflightWorkRootInvalid)
	if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), key) {
		t.Fatalf("preflight lost raw work-root cause: %v", cause)
	}
}

func TestPreflightSelectionHidesNativeCredentialEcho(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Codex CLI uses a POSIX shell")
	}
	installActionTestSkills(t, testReviewSkillURL)
	t.Setenv("CODEX_HOME", t.TempDir())
	const token = "native-preflight-token"
	t.Setenv("CODEX_API_KEY", token)
	fakeOnPath(t, "codex", `#!/bin/sh
case "$1 $2" in
  'debug prompt-input')
    case " $* " in
      *' skills.include_instructions=true '*) printf '%s\n' '[{"role":"user","content":[{"text":"MINION_TRUSTED_PROMPT_78 MINION_UNTRUSTED_AGENTS_78 MINION_UNTRUSTED_SKILL_78"}]}]' ;;
      *) printf '%s\n' '[{"role":"user","content":[{"text":"MINION_TRUSTED_PROMPT_78"}]}]' ;;
    esac
    exit 0 ;;
esac
case "$1" in
  app-server) printf '%s\n' "$CODEX_API_KEY" >&2; exit 7 ;;
esac
exit 99
`)
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
	})
	err := runner.PreflightSelection(t.Context())
	requirePreflightMessage(t, err, preflightCodexInvocationUnsupported)
	if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), token) {
		t.Fatalf("fake CLI did not echo the native credential into the raw cause: %v", cause)
	}
}

func TestPreflightSelectionRejectsClaudeModelAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Claude CLI uses a POSIX shell")
	}
	installActionTestSkills(t, testReviewSkillURL)
	writeActionTestSkills(t, os.Getenv("HOME"), ".claude", testReviewSkillURL)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "")
	}
	fakeOnPath(t, "claude", `#!/bin/sh
if [ "$3" = auth ]; then
  printf '%s\n' '{"loggedIn":true}'
else
  case " $* " in
    *' --minion-unsupported-capability-probe '*) printf '%s\n' 'unknown option --minion-unsupported-capability-probe' >&2; exit 2 ;;
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
    *minion-initialize*) printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"minion-initialize","response":{"models":[{"value":"default","resolvedModel":"claude-pinned","supportedEffortLevels":["low"]}]}}}' ;;
    *) exit 99 ;;
  esac
fi
`)
	selected := WorkerSelection{
		Commands: map[string]config.CommandMapping{
			"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
		},
		Main: RoleSettings{Provider: "claude", Model: "default", Effort: "low"},
	}
	runner, err := NewSelectedRunner(Runner{WorkRoot: t.TempDir()}, selected)
	if err != nil {
		t.Fatal(err)
	}
	err = runner.PreflightSelection(t.Context())
	requirePreflightMessage(t, err, preflightClaudeSettingsUnavailable)
	if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), "was not resolved at startup") {
		t.Fatalf("unresolved Claude alias cause = %v", cause)
	}
	selected.Main.Model = "claude-pinned"
	runner, err = NewSelectedRunner(Runner{WorkRoot: t.TempDir()}, selected)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.PreflightSelection(t.Context()); err != nil {
		t.Fatalf("resolved Claude model failed preflight: %v", err)
	}
}

func TestValidPreflightMessageRejectsInjectedOutput(t *testing.T) {
	for _, message := range []string{
		"", "Codex login unavailable\nsecret", "selected command skill unavailable: secret",
		"worker runner unavailable ", "preflight: worker runner unavailable",
	} {
		if validPreflightMessage(message) {
			t.Fatalf("accepted nonconstant preflight message %q", message)
		}
	}
}
