package action

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
)

func selectedPreflightRunner(t *testing.T, commands map[string]config.CommandMapping) *Runner {
	t.Helper()
	runner, err := NewSelectedRunner(Runner{WorkRoot: t.TempDir()}, WorkerSelection{
		Commands: commands,
		Main:     RoleSettings{Provider: "codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestPreflightSelectionRequiresFrozenSelectionAndWorkRoot(t *testing.T) {
	var missing *Runner
	if err := missing.PreflightSelection(t.Context()); err == nil || !strings.Contains(err.Error(), "runner is missing") {
		t.Fatalf("nil runner preflight = %v", err)
	}
	if err := (&Runner{WorkRoot: t.TempDir()}).PreflightSelection(t.Context()); err == nil || !strings.Contains(err.Error(), "no frozen selection") {
		t.Fatalf("unselected runner preflight = %v", err)
	}
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
	})
	runner.WorkRoot = ""
	if err := runner.PreflightSelection(t.Context()); err == nil || !strings.Contains(err.Error(), "work root is required") {
		t.Fatalf("empty work root preflight = %v", err)
	}
}

func TestPreflightSelectionChecksEverySelectedRequiredSkill(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
		"fix":    {Action: string(command.Fix), Skills: []string{testFixSkillURL}},
	})
	err := runner.PreflightSelection(t.Context())
	if err == nil || !strings.Contains(err.Error(), "fix") || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing selected skill preflight = %v", err)
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
	if err == nil || !strings.Contains(err.Error(), "audit prerequisites") || !strings.Contains(err.Error(), "validate-findings.cjs") {
		t.Fatalf("selected audit toolchain preflight = %v", err)
	}
	assertEmptyDir(t, runner.WorkRoot)

	reviewOnly := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
	})
	t.Setenv("PATH", t.TempDir())
	err = reviewOnly.PreflightSelection(t.Context())
	if err == nil || !strings.Contains(err.Error(), "git readiness") || strings.Contains(err.Error(), "audit prerequisites") {
		t.Fatalf("review-only preflight reached unselected audit: %v", err)
	}
}

func TestPreflightSelectionRequiresAuditInCommandMapping(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL, testAuditSkillURL)
	runner, err := NewSelectedRunner(Runner{WorkRoot: t.TempDir()}, WorkerSelection{
		Commands: map[string]config.CommandMapping{
			"audit": {Action: string(command.SecurityAudit), Skills: []string{testReviewSkillURL}},
		},
		Main: RoleSettings{Provider: "codex", AlwaysOnSkills: []string{testAuditSkillURL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.PreflightSelection(t.Context()); err == nil || !strings.Contains(err.Error(), "no selected security-audit skill") {
		t.Fatalf("audit borrowed optional skill: %v", err)
	}
}

func TestPreflightSelectionRedactsAndBoundsErrors(t *testing.T) {
	const key = "preflight-secret-key"
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{"https://github.com/o/r/tree/main/skills/" + key}},
	})
	runner.Codex.APIKey = key
	err := runner.PreflightSelection(t.Context())
	if err == nil || strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("unredacted skill error: %v", err)
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
	if err == nil {
		t.Fatal("invalid work root passed preflight")
	}
	if strings.Contains(err.Error(), key) || len(err.Error()) > httpFailureMessageLimit || !utf8.ValidString(err.Error()) {
		t.Fatalf("unsafe preflight error: bytes=%d error=%v", len(err.Error()), err)
	}
	if !strings.Contains(err.Error(), "[redacted]") || !strings.HasSuffix(err.Error(), " [truncated]") {
		t.Fatalf("preflight error was not redacted and bounded: %q", err)
	}
	if !strings.Contains(err.Error(), "worker work root") {
		t.Fatalf("preflight lost work root context: %v", err)
	}
}
