package action

import (
	"fmt"
	"os"
	"path/filepath"
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
	for _, source := range sources {
		name, err := skills.Name(source)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(home, ".agents", "skills", name)
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
