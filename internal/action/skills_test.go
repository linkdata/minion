package action

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
)

func TestConfigureRoleReportsRequiredAndOptionalResolutionErrors(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			var journal bytes.Buffer
			runner := Runner{Logger: log.New(&journal, "", 0)}
			if err := runner.ConfigureRole(RoleSettings{Provider: provider, Skills: []string{"invalid skill URL"}}); err == nil || !strings.Contains(err.Error(), provider+" skill:") {
				t.Fatalf("required skill error was lost: %v", err)
			}
			err := runner.ConfigureRole(RoleSettings{Provider: provider, AlwaysOnSkills: []string{"invalid skill URL", skills.BuiltinAntiPromptInjection}})
			selected := runner.Codex.Skills
			if provider == "claude" {
				selected = runner.Claude.Skills
			}
			if err != nil || !slices.Equal(selected, []string{skills.BuiltinAntiPromptInjection}) || !strings.Contains(journal.String(), "skip unavailable "+provider+" always-on skill invalid skill URL:") {
				t.Fatalf("optional skill error was not logged and skipped: skills=%v error=%v journal=%s", selected, err, &journal)
			}
		})
	}
}

const (
	testReviewSkillURL = "https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail-review"
	testFixSkillURL    = "https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail"
	testAuditSkillURL  = "https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"
)

func TestValidationDoesNotDuplicateOptionalSkillWarning(t *testing.T) {
	var journal bytes.Buffer
	orchestrator := &Orchestrator{
		Runner:         Runner{Logger: log.New(&journal, "", 0)},
		Main:           "codex",
		AlwaysOnSkills: []string{"invalid skill URL"},
		Commands: map[string]config.CommandMapping{
			"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}},
		},
	}
	job := monitor.Job{
		Repository: config.Repository{Name: "owner/repo", Actions: []string{"inspect"}},
		Command:    command.Command{Name: "inspect", Action: command.Comment},
	}
	if err := orchestrator.validateCommand(job); err != nil || journal.Len() != 0 {
		t.Fatalf("validation logged optional skill skip: err=%v log=%q", err, journal.String())
	}
	if err := orchestrator.Runner.ConfigureRole(orchestrator.roleSettings("codex", orchestrator.Commands["inspect"].Skills)); err != nil || strings.Count(journal.String(), "skip unavailable codex always-on skill invalid skill URL:") != 1 {
		t.Fatalf("execution warning count changed: err=%v log=%q", err, journal.String())
	}
}

func TestRunnerSeparatesMainAndCriticSkills(t *testing.T) {
	const optional = "https://github.com/example/skills/tree/main/skills/optional"
	const missing = "https://github.com/example/skills/tree/main/skills/missing"
	for _, main := range []string{"codex", "claude"} {
		t.Run(main, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			mainRoot, criticRoot, critic := ".agents", ".claude", "claude"
			if main == "claude" {
				mainRoot, criticRoot, critic = ".claude", ".agents", "codex"
			}
			writeActionTestSkills(t, home, mainRoot, testReviewSkillURL, optional)
			writeActionTestSkills(t, home, criticRoot, optional)
			runner := &Runner{
				Codex:  config.CodexConfig{Model: "host-codex", ReasoningEffort: "low", Skills: []string{"stale codex skill"}},
				Claude: config.ClaudeConfig{Model: "host-claude", Effort: "low", Skills: []string{"stale claude skill"}},
			}
			mainRole := RoleSettings{Provider: main, Model: "main-model", Effort: "high", Skills: []string{testReviewSkillURL},
				AlwaysOnSkills: []string{optional, skills.BuiltinAntiPromptInjection, missing}}
			criticRole := RoleSettings{Provider: critic, Model: "critic-model", Effort: "medium",
				AlwaysOnSkills: mainRole.AlwaysOnSkills}
			configured := *runner
			if err := configured.ConfigureRole(mainRole); err != nil {
				t.Fatal(err)
			}
			if err := configured.ConfigureRole(criticRole); err != nil {
				t.Fatal(err)
			}
			mainSkills, criticSkills := configured.Codex.Skills, configured.Claude.Skills
			if main == "claude" {
				mainSkills, criticSkills = configured.Claude.Skills, configured.Codex.Skills
			}
			wantMain := []string{
				filepath.Join(home, mainRoot, "skills", "ponytail-review", "SKILL.md"),
				filepath.Join(home, mainRoot, "skills", "optional", "SKILL.md"),
				skills.BuiltinAntiPromptInjection,
			}
			wantCritic := []string{filepath.Join(home, criticRoot, "skills", "optional", "SKILL.md"), skills.BuiltinAntiPromptInjection}
			if !slices.Equal(mainSkills, wantMain) || !slices.Equal(criticSkills, wantCritic) {
				t.Fatalf("skills mixed roles: main=%v critic=%v", mainSkills, criticSkills)
			}
			if runner.Codex.Skills[0] != "stale codex skill" || runner.Claude.Skills[0] != "stale claude skill" {
				t.Fatal("per-job selection changed shared runner settings")
			}
			for _, got := range []*Runner{runner, &configured} {
				if got.Codex.Model != "host-codex" || got.Codex.ReasoningEffort != "low" || got.Claude.Model != "host-claude" || got.Claude.Effort != "low" {
					t.Fatal("skill selection changed host model or effort settings")
				}
			}

			if err := configured.ConfigureRole(RoleSettings{Provider: critic}); err != nil {
				t.Fatal(err)
			}
			selected := configured.Codex.Skills
			if critic == "claude" {
				selected = configured.Claude.Skills
			}
			if len(selected) != 0 {
				t.Fatalf("empty role retained stale skills: %v", selected)
			}
			writeActionTestSkills(t, home, criticRoot, testReviewSkillURL)
			if err := os.RemoveAll(filepath.Dir(wantMain[0])); err != nil {
				t.Fatal(err)
			}
			if err := configured.ConfigureRole(mainRole); err == nil {
				t.Fatal("main used a required skill installed only for its critic")
			}
		})
	}
}

func TestPortableJobCarriesAlwaysOnSkillsToBothRoles(t *testing.T) {
	gitFixtureRemote(t)
	checkout := cloneFixture(t, t.TempDir())
	request := usageJob()
	request.Command = command.Command{Name: "inspect", Action: command.Comment}
	orchestrator := &Orchestrator{
		Main: "codex", Adversarial: "claude",
		AlwaysOnSkills: []string{skills.BuiltinAntiPromptInjection},
		Commands: map[string]config.CommandMapping{
			"inspect": {Action: "comment", Skills: []string{testReviewSkillURL}},
		},
	}
	job, err := attemptFixture(orchestrator).portableJob(testActionContext(t), &preparedAction{checkout: checkout, request: request}, "title", "body")
	if err != nil {
		t.Fatal(err)
	}
	if job.Critic == nil || !slices.Equal(job.Main.Skills, []string{testReviewSkillURL}) || len(job.Critic.Skills) != 0 ||
		!slices.Equal(job.Main.AlwaysOnSkills, orchestrator.AlwaysOnSkills) || !slices.Equal(job.Critic.AlwaysOnSkills, orchestrator.AlwaysOnSkills) {
		t.Fatalf("portable role skills: main=%+v critic=%+v", job.Main, job.Critic)
	}
}

func TestRunnerRequiresEverySkillAndLoadsOptionalSkills(t *testing.T) {
	const extraURL = "https://github.com/example/skills/tree/main/skills/extra"
	const optionalURL = "https://github.com/example/skills/tree/main/skills/optional"
	installActionTestSkills(t, testReviewSkillURL, optionalURL)
	job := monitor.Job{
		Repository: config.Repository{Name: "owner/repo", Actions: []string{"inspect"}},
		Command:    command.Command{Name: "inspect", Action: command.Review},
	}
	orchestrator := &Orchestrator{
		Commands: map[string]config.CommandMapping{
			"inspect": {Action: "review", Skills: []string{testReviewSkillURL, extraURL}},
		},
		AlwaysOnSkills: []string{optionalURL},
	}
	if err := orchestrator.validateCommand(job); err == nil || errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("missing required skill should fail before preparation and remain retryable: %v", err)
	}
	role := RoleSettings{Provider: "codex", Skills: orchestrator.Commands["inspect"].Skills,
		AlwaysOnSkills: orchestrator.AlwaysOnSkills}
	if err := orchestrator.Runner.ConfigureRole(role); err == nil || errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("execution must fail early and retryably for a missing required skill: %v", err)
	}
	installActionTestSkills(t, testReviewSkillURL, extraURL, optionalURL)
	if err := orchestrator.validateCommand(job); err != nil {
		t.Fatal(err)
	}
	if len(orchestrator.Runner.Codex.Skills) != 0 {
		t.Fatal("command validation changed the shared runner")
	}
	configured := orchestrator.Runner
	if err := configured.ConfigureRole(role); err != nil {
		t.Fatal(err)
	}
	if len(configured.Codex.Skills) != 3 {
		t.Fatalf("loaded skills = %v; want two required and one optional", configured.Codex.Skills)
	}
	delete(orchestrator.Commands, "inspect")
	if err := orchestrator.validateCommand(job); err == nil {
		t.Fatal("unmapped command ran")
	}
	orchestrator.Commands["inspect"] = config.CommandMapping{Action: "review"}
	if err := orchestrator.validateCommand(job); err == nil {
		t.Fatal("command with no required skills ran")
	}
}

func TestExecuteMissingRequiredSkillDoesNotClone(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	root := t.TempDir()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "unexpected GitHub request", http.StatusInternalServerError)
	}))
	defer server.Close()
	orchestrator := &Orchestrator{
		WorkRoot: root, GitHub: testGitHub(server, "token"),
		Commands: map[string]config.CommandMapping{
			"inspect": {Action: "comment", Skills: []string{testReviewSkillURL, "https://github.com/example/skills/tree/main/skills/missing"}},
		},
	}
	job := usageJob()
	job.Repository.Actions = []string{"inspect"}
	job.Command = command.Command{Name: "inspect", Action: command.Comment}
	err := orchestrator.Execute(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "skill is not installed") || errors.Is(err, monitor.ErrTerminalJob) || requests != 0 {
		t.Fatalf("missing skill reached preparation: requests=%d error=%v", requests, err)
	}
	assertEmptyDir(t, root)
}

func TestValidateCommandRequiresSupportedMainWorkflow(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	job := monitor.Job{
		Repository: config.Repository{Name: "owner/repo", Actions: []string{"inspect"}},
		Command:    command.Command{Name: "inspect", Action: command.SecurityAudit},
	}
	for _, main := range []string{"codex", "claude"} {
		e := &Orchestrator{
			Commands: map[string]config.CommandMapping{
				"inspect": {Action: "security-audit", Skills: []string{testAuditSkillURL}},
			},
			Main: main,
		}
		err := e.validateCommand(job)
		if main == "claude" {
			if !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("unsupported workflow must fail before invocation: %v", err)
			}
		} else if err != nil {
			t.Fatalf("supported workflow rejected: %v", err)
		}
	}
}
