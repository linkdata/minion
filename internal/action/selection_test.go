package action

import (
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
)

func TestOrchestratorWorkerSelectionEnabledInstalledSubset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	commands := map[string]config.CommandMapping{
		"inspect":  {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}},
		"repair":   {Action: "fix", Skills: []string{skills.BuiltinAntiPromptInjection}},
		"missing":  {Action: "review", Skills: []string{"https://github.com/example/skills/tree/main/skills/not-installed"}},
		"disabled": {Action: "review", Skills: []string{skills.BuiltinAntiPromptInjection}},
	}
	cfg := config.Config{Main: "codex", Commands: commands, Repositories: []config.Repository{
		{Name: "owner/one", Actions: []string{"inspect", "missing"}},
		{Name: "owner/two", Actions: []string{"repair", "inspect"}},
	}}
	alwaysOn := []string{skills.BuiltinAntiPromptInjection}
	e := Orchestrator{Commands: commands, Main: "codex", CodexModel: "resolved-main", CodexEffort: "high", AlwaysOnSkills: alwaysOn}
	selected, err := e.WorkerSelection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Commands) != 2 || selected.Commands["inspect"].Action != "comment" || selected.Commands["repair"].Action != "fix" {
		t.Fatalf("selected wrong commands: %+v", selected.Commands)
	}
	if _, ok := selected.Commands["missing"]; ok {
		t.Fatal("selected an uninstalled command")
	}
	if _, ok := selected.Commands["disabled"]; ok {
		t.Fatal("selected a command disabled in every repository")
	}
	if !sameRoleSettings(selected.Main, RoleSettings{Provider: "codex", Model: "resolved-main", Effort: "high", AlwaysOnSkills: alwaysOn}) || selected.Critic != nil {
		t.Fatalf("selected wrong roles: %+v", selected)
	}
	commands["inspect"].Skills[0] = testReviewSkillURL
	alwaysOn[0] = testAuditSkillURL
	if !slices.Equal(selected.Commands["inspect"].Skills, []string{skills.BuiltinAntiPromptInjection}) ||
		!slices.Equal(selected.Main.AlwaysOnSkills, []string{skills.BuiltinAntiPromptInjection}) {
		t.Fatalf("caller mutation changed selection: %+v", selected)
	}
}

func TestOrchestratorWorkerSelectionResolvedCriticRoles(t *testing.T) {
	commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
	cfg := config.Config{Main: "claude", Adversarial: "codex", Commands: commands, Repositories: []config.Repository{{Actions: []string{"inspect"}}}}
	e := Orchestrator{Commands: commands, Main: "claude", Adversarial: "codex", ClaudeModel: "resolved-claude", ClaudeEffort: "max", CodexModel: "resolved-codex", CodexEffort: "high", AlwaysOnSkills: []string{skills.BuiltinAntiPromptInjection}}
	selected, err := e.WorkerSelection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !sameRoleSettings(selected.Main, RoleSettings{Provider: "claude", Model: "resolved-claude", Effort: "max", AlwaysOnSkills: e.AlwaysOnSkills}) ||
		selected.Critic == nil || !sameRoleSettings(*selected.Critic, RoleSettings{Provider: "codex", Model: "resolved-codex", Effort: "high", AlwaysOnSkills: e.AlwaysOnSkills}) {
		t.Fatalf("selected wrong resolved roles: %+v", selected)
	}
}

func TestOrchestratorWorkerSelectionRejectsMappingMismatch(t *testing.T) {
	configured := config.CommandMapping{Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}
	for _, used := range []config.CommandMapping{
		{Action: "review", Skills: configured.Skills},
		{Action: configured.Action, Skills: []string{testReviewSkillURL}},
	} {
		cfg := config.Config{Commands: map[string]config.CommandMapping{"inspect": configured}, Repositories: []config.Repository{{Actions: []string{"inspect"}}}}
		e := Orchestrator{Commands: map[string]config.CommandMapping{"inspect": used}, CodexModel: "resolved", CodexEffort: "high"}
		if _, err := e.WorkerSelection(cfg); err == nil || !strings.Contains(err.Error(), "differs from orchestrator") {
			t.Fatalf("mismatched mapping accepted: %+v, %v", used, err)
		}
	}
}

func TestOrchestratorWorkerSelectionRejectsProviderDrift(t *testing.T) {
	commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
	cfg := config.Config{Commands: commands, Repositories: []config.Repository{{Actions: []string{"inspect"}}}}
	e := Orchestrator{Commands: commands, CodexModel: "resolved", CodexEffort: "high", ClaudeModel: "resolved"}
	e.Main = "claude"
	if _, err := e.WorkerSelection(cfg); err == nil || !strings.Contains(err.Error(), "main provider differs") {
		t.Fatalf("default Codex config accepted Claude main: %v", err)
	}
	e.Main, e.Adversarial = "codex", "claude"
	if _, err := e.WorkerSelection(cfg); err == nil || !strings.Contains(err.Error(), "critic provider differs") {
		t.Fatalf("config without critic accepted orchestrator critic: %v", err)
	}
}

func TestOrchestratorWorkerSelectionRejectsEmptySelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
	e := Orchestrator{Commands: commands, CodexModel: "resolved", CodexEffort: "high"}
	for _, cfg := range []config.Config{
		{Commands: map[string]config.CommandMapping{}, Repositories: []config.Repository{{Actions: nil}}},
		{Commands: map[string]config.CommandMapping{"missing": {Action: "comment", Skills: []string{"https://github.com/example/skills/tree/main/skills/not-installed"}}}, Repositories: []config.Repository{{Actions: []string{"missing"}}}},
	} {
		e.Commands = cfg.Commands
		if _, err := e.WorkerSelection(cfg); err == nil || !strings.Contains(err.Error(), "no installed enabled commands") {
			t.Fatalf("empty selection accepted: %v", err)
		}
	}
}

func TestNewSelectedRunnerFreezesStartupSelection(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	job.Main.Model, job.Main.Effort = "main", "high"
	job.Main.AlwaysOnSkills = []string{testReviewSkillURL}
	job.Critic.Model, job.Critic.Effort = "critic", "medium"
	job.Critic.AlwaysOnSkills = []string{testAuditSkillURL}
	selected := WorkerSelection{
		Commands: map[string]config.CommandMapping{job.Command: {Action: string(job.Action), Skills: slices.Clone(job.Main.Skills)}},
		Main:     RoleSettings{Provider: job.Main.Provider, Model: job.Main.Model, Effort: job.Main.Effort, AlwaysOnSkills: slices.Clone(job.Main.AlwaysOnSkills)},
		Critic:   &RoleSettings{Provider: job.Critic.Provider, Model: job.Critic.Model, Effort: job.Critic.Effort, AlwaysOnSkills: slices.Clone(job.Critic.AlwaysOnSkills)},
	}
	runner, err := NewSelectedRunner(Runner{runModel: forbidModel(t)}, selected)
	if err != nil {
		t.Fatal(err)
	}
	mapping := selected.Commands[job.Command]
	mapping.Skills[0] = testAuditSkillURL
	selected.Commands[job.Command] = mapping
	selected.Commands["other"] = config.CommandMapping{Action: string(command.Comment), Skills: []string{testAuditSkillURL}}
	selected.Main.Model = "other"
	selected.Main.AlwaysOnSkills[0] = testAuditSkillURL
	selected.Critic.Model = "other"
	selected.Critic.AlwaysOnSkills[0] = testReviewSkillURL
	if err := runner.selection.matches(job); err != nil {
		t.Fatalf("caller mutation changed startup selection: %v", err)
	}
	for _, widened := range []Job{
		func() Job { changed := job; changed.Command = "other"; return changed }(),
		func() Job { changed := job; changed.Main.Skills = []string{testAuditSkillURL}; return changed }(),
		func() Job { changed := job; changed.Main.AlwaysOnSkills = []string{testAuditSkillURL}; return changed }(),
		func() Job {
			changed := job
			critic := *job.Critic
			critic.Model = "other"
			changed.Critic = &critic
			return changed
		}(),
		func() Job {
			changed := job
			critic := *job.Critic
			critic.AlwaysOnSkills = []string{testReviewSkillURL}
			changed.Critic = &critic
			return changed
		}(),
	} {
		if err := runner.selection.matches(widened); err == nil {
			t.Fatalf("widened assignment matched frozen selection: %+v", widened)
		}
	}
	if _, err := NewSelectedRunner(*runner, selected); err == nil {
		t.Fatal("selected runner accepted a second startup selection")
	}
}

func TestNewSelectedRunnerRequiresResolvedModelAndCodexEffort(t *testing.T) {
	commands := map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
	}
	for _, tc := range []struct {
		name   string
		main   RoleSettings
		critic *RoleSettings
	}{
		{name: "main model", main: RoleSettings{Provider: "codex", Effort: "high"}},
		{name: "main Codex effort", main: RoleSettings{Provider: "codex", Model: "main"}},
		{name: "critic model", main: RoleSettings{Provider: "codex", Model: "main", Effort: "high"}, critic: &RoleSettings{Provider: "claude"}},
		{name: "critic Codex effort", main: RoleSettings{Provider: "claude", Model: "main"}, critic: &RoleSettings{Provider: "codex", Model: "critic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSelectedRunner(Runner{}, WorkerSelection{Commands: commands, Main: tc.main, Critic: tc.critic}); err == nil {
				t.Fatal("selected runner accepted an unresolved role")
			}
		})
	}
	if _, err := NewSelectedRunner(Runner{}, WorkerSelection{
		Commands: commands,
		Main:     RoleSettings{Provider: "claude", Model: "main"},
	}); err != nil {
		t.Fatalf("Claude's optional default effort was rejected: %v", err)
	}
}
