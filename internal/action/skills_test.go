package action

import (
	"bytes"
	"errors"
	"fmt"
	"log"
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

func installActionTestSkills(t *testing.T, sources ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeActionTestSkills(t, home, ".agents", sources...)
}

func writeActionTestSkills(t *testing.T, home, parent string, sources ...string) {
	t.Helper()
	for _, source := range sources {
		name, err := skills.Name(source)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(home, parent, "skills", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Test skill\n---\nUse this test skill.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name == "security-audit" {
			for _, validator := range auditValidators {
				if err := os.WriteFile(filepath.Join(dir, validator.script), []byte("JSON.parse(require('fs').readFileSync(process.argv[2], 'utf8'));\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		metadata := fmt.Sprintf(`{"url":%q,"commit":%q}`, source, strings.Repeat("a", 40))
		if err := os.WriteFile(filepath.Join(dir, ".minion-install.json"), []byte(metadata), 0o600); err != nil {
			t.Fatal(err)
		}
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
	if err := orchestrator.validateCommand(job); err != nil {
		t.Fatalf("logical command depends on execution-host skills: %v", err)
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
	configured := orchestrator.Runner
	if err := configured.ConfigureRole(role); err != nil {
		t.Fatal(err)
	}
	if len(configured.Codex.Skills) != 3 {
		t.Fatalf("loaded skills = %v; want two required and one optional", configured.Codex.Skills)
	}
	if len(orchestrator.Runner.Codex.Skills) != 0 {
		t.Fatal("skill selection changed the shared orchestrator")
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

func TestValidateCommandRequiresSupportedMainWorkflow(t *testing.T) {
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
