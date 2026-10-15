package action

import (
	"errors"
	"fmt"
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

const (
	testReviewSkillURL  = "https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail-review"
	testFixSkillURL     = "https://github.com/DietrichGebert/ponytail/tree/main/skills/ponytail"
	testAuditSkillURL   = "https://github.com/cloudflare/security-audit-skill/tree/main/skills/security-audit"
	testReleaseSkillURL = "https://github.com/example/skills/tree/main/skills/release-notes"
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
		metadata := fmt.Sprintf(`{"url":%q,"commit":%q}`, source, strings.Repeat("a", 40))
		if err := os.WriteFile(filepath.Join(dir, ".minion-install.json"), []byte(metadata), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestForCommandSeparatesMainAndCriticSkills(t *testing.T) {
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
			e := &Executor{
				Main: main, Adversarial: critic,
				Codex: config.CodexConfig{Skills: []string{"stale codex skill"}}, Claude: config.ClaudeConfig{Skills: []string{"stale claude skill"}},
				Commands: map[string]config.CommandMapping{
					"inspect": {Action: "review", Skills: []string{testReviewSkillURL}},
				},
				AlwaysOnSkills: []string{optional, skills.BuiltinAntiPromptInjection, missing},
			}
			job := monitor.Job{
				Repository: config.Repository{Name: "owner/repo", Actions: []string{"inspect"}},
				Command:    command.Command{Name: "inspect", Action: command.Review},
			}
			configured, err := e.forCommand(job)
			if err != nil {
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
			if e.Codex.Skills[0] != "stale codex skill" || e.Claude.Skills[0] != "stale claude skill" {
				t.Fatal("per-job selection changed shared executor settings")
			}
			writeActionTestSkills(t, home, criticRoot, testReviewSkillURL)
			if err := os.RemoveAll(filepath.Dir(wantMain[0])); err != nil {
				t.Fatal(err)
			}
			if _, err := e.forCommand(job); err == nil {
				t.Fatal("main used a required skill installed only for its critic")
			}
		})
	}
}

func TestForCommandRequiresEverySkillAndLoadsOptionalSkills(t *testing.T) {
	const extraURL = "https://github.com/example/skills/tree/main/skills/extra"
	const optionalURL = "https://github.com/example/skills/tree/main/skills/optional"
	installActionTestSkills(t, testReviewSkillURL, optionalURL)
	job := monitor.Job{
		Repository: config.Repository{Name: "owner/repo", Actions: []string{"inspect"}},
		Command:    command.Command{Name: "inspect", Action: command.Review},
	}
	executor := &Executor{
		Commands: map[string]config.CommandMapping{
			"inspect": {Action: "review", Skills: []string{testReviewSkillURL, extraURL}},
		},
		AlwaysOnSkills: []string{optionalURL},
	}
	if _, err := executor.forCommand(job); err == nil {
		t.Fatal("command ran with a missing required skill")
	}
	installActionTestSkills(t, testReviewSkillURL, extraURL, optionalURL)
	configured, err := executor.forCommand(job)
	if err != nil {
		t.Fatal(err)
	}
	if len(configured.Codex.Skills) != 3 {
		t.Fatalf("loaded skills = %v; want two required and one optional", configured.Codex.Skills)
	}
	if len(executor.Codex.Skills) != 0 {
		t.Fatal("skill selection changed the shared executor")
	}
	delete(executor.Commands, "inspect")
	if _, err := executor.forCommand(job); err == nil {
		t.Fatal("unmapped command ran")
	}
	executor.Commands["inspect"] = config.CommandMapping{Action: "review"}
	if _, err := executor.forCommand(job); err == nil {
		t.Fatal("command with no required skills ran")
	}
}

func TestForCommandRequiresSupportedMainWorkflow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeActionTestSkills(t, home, ".agents", testAuditSkillURL)
	writeActionTestSkills(t, home, ".claude", testAuditSkillURL)
	job := monitor.Job{
		Repository: config.Repository{Name: "owner/repo", Actions: []string{"inspect"}},
		Command:    command.Command{Name: "inspect", Action: command.SecurityAudit},
	}
	for _, main := range []string{"codex", "claude"} {
		e := &Executor{Main: main, Commands: map[string]config.CommandMapping{
			"inspect": {Action: "security-audit", Skills: []string{testAuditSkillURL}},
		}}
		_, err := e.forCommand(job)
		if main == "claude" {
			if !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("unsupported workflow must fail before invocation: %v", err)
			}
		} else if err != nil {
			t.Fatalf("supported workflow rejected: %v", err)
		}
	}
}
