package action

import (
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
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

func TestOrchestratorWorkerSelectionRequiresHostSupport(t *testing.T) {
	commands := map[string]config.CommandMapping{
		"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}},
		"audit":   {Action: "security-audit", Skills: []string{skills.BuiltinAntiPromptInjection}},
	}
	cfg := config.Config{Main: "codex", Commands: commands, Repositories: []config.Repository{{Actions: []string{"inspect", "audit"}}}}
	e := Orchestrator{Commands: commands, Main: "codex", CodexModel: "main", CodexEffort: "high"}
	selected, err := e.WorkerSelection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, offered := selected.Commands["audit"]; offered != (runtime.GOOS != "windows") || selected.Commands["inspect"].Action != "comment" {
		t.Fatalf("host=%s selected commands = %v", runtime.GOOS, selected.Commands)
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

func TestWorkerSelectionFreezesDistinctSameProviderRoles(t *testing.T) {
	commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
	cfg := config.Config{Main: "codex", Adversarial: "codex", MainSettings: config.RoleModelConfig{Model: "main-model", Effort: "high"},
		CriticSettings: config.RoleModelConfig{Model: "critic-model", Effort: "low"}, Commands: commands,
		Repositories: []config.Repository{{Actions: []string{"inspect"}}}}
	e := Orchestrator{Main: cfg.Main, Adversarial: cfg.Adversarial, Commands: commands,
		MainModel: cfg.MainSettings.Model, MainEffort: cfg.MainSettings.Effort,
		CriticModel: cfg.CriticSettings.Model, CriticEffort: cfg.CriticSettings.Effort}
	selected, err := e.WorkerSelection(cfg)
	if err != nil || selected.Main.Model != "main-model" || selected.Critic == nil || selected.Critic.Model != "critic-model" {
		t.Fatalf("distinct same-provider selection: %+v, %v", selected, err)
	}
	job := testJob(command.Comment)
	job.Command = "inspect"
	job.Main, job.Critic = selected.Main, selected.Critic
	job.Main.Skills = commands["inspect"].Skills
	job.CriticMaxRounds = 1
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	runner, err := NewSelectedRunner(Runner{}, selected)
	if err != nil || runner.selection.matches(job) != nil {
		t.Fatalf("same-provider assignment lost role settings: %v", err)
	}
	job.Critic.Model = "main-model"
	if err := runner.selection.matches(job); err == nil || !strings.Contains(err.Error(), "critic role differs") {
		t.Fatalf("critic model drift accepted: %v", err)
	}
}

func TestWorkerSelectionAllowsOnlyApprovedRolePairs(t *testing.T) {
	commandSkills := []string{skills.BuiltinAntiPromptInjection}
	alwaysOn := []string{testReviewSkillURL}
	commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: commandSkills}}
	choices := &config.UserModelChoices{
		Main:   []config.RoleModelConfig{{Model: "approved-main", Effort: "low"}},
		Critic: []config.RoleModelConfig{{Model: "approved-critic", Effort: "medium"}},
	}
	cfg := config.Config{Main: "codex", Adversarial: "claude", Commands: commands,
		Codex:        config.CodexConfig{Model: "default-main", ReasoningEffort: "medium"},
		Claude:       config.ClaudeConfig{Model: "default-critic", Effort: "low"},
		Admin:        &config.AdminConfig{UserModelChoices: choices},
		Repositories: []config.Repository{{Actions: []string{"inspect"}}}}
	e := Orchestrator{Main: cfg.Main, Adversarial: cfg.Adversarial, Commands: commands,
		CodexModel: cfg.Codex.Model, CodexEffort: cfg.Codex.ReasoningEffort,
		ClaudeModel: cfg.Claude.Model, ClaudeEffort: cfg.Claude.Effort,
		MainModel: cfg.Codex.Model, MainEffort: cfg.Codex.ReasoningEffort,
		CriticModel: cfg.Claude.Model, CriticEffort: cfg.Claude.Effort,
		AlwaysOnSkills: alwaysOn}
	selected, err := e.WorkerSelection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewSelectedRunner(Runner{}, selected)
	if err != nil {
		t.Fatal(err)
	}
	job := withClaudeCritic(testJob(command.Comment))
	job.Command = "inspect"
	job.Main = selected.Main
	job.Main.Skills = commandSkills
	job.Critic = selected.Critic
	job.Main.Model, job.Main.Effort = choices.Main[0].Model, choices.Main[0].Effort
	job.Critic.Model, job.Critic.Effort = choices.Critic[0].Model, choices.Critic[0].Effort
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := runner.selection.matches(job); err != nil {
		t.Fatalf("approved pair rejected: %v", err)
	}
	choices.Main[0].Model = "changed-after-selection"
	if err := runner.selection.matches(job); err != nil {
		t.Fatalf("policy pointer mutation changed frozen worker selection: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Job)
	}{
		{"higher effort", func(j *Job) { j.Main.Effort = "high" }},
		{"unapproved model", func(j *Job) { j.Critic.Model = "other-critic" }},
		{"main provider", func(j *Job) { j.Main.Provider = "claude" }},
		{"required skill", func(j *Job) { j.Main.Skills = []string{testAuditSkillURL} }},
		{"always-on skill", func(j *Job) { j.Critic.AlwaysOnSkills = []string{testAuditSkillURL} }},
		{"command", func(j *Job) { j.Command = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := job
			critic := *job.Critic
			changed.Critic = &critic
			tc.edit(&changed)
			err := runner.selection.matches(changed)
			if err == nil {
				t.Fatal("selection accepted broadened assignment")
			}
		})
	}
	// A previously accepted action keeps its frozen pair if the operator removes it before restart.
	oldPair := job.Main
	at := time.Now().UTC()
	store := openState(t, at.Add(-time.Minute))
	accepted := dashboardAccepted(job, at)
	accepted.MainSettings = config.RoleModelConfig{Model: oldPair.Model, Effort: oldPair.Effort}
	store.Data.Accepted = accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	cfg.Admin.UserModelChoices.Main = nil
	e.Store = store
	restarted, err := e.WorkerSelection(cfg)
	if err != nil || slices.Contains(restarted.MainChoices, accepted.MainSettings) || !slices.Contains(restarted.RetainedMainChoices, accepted.MainSettings) {
		t.Fatalf("removed active pair was not retained only for recovery: %+v, %v", restarted, err)
	}
}

func TestWorkerSelectionRetainsWithdrawnClaudePair(t *testing.T) {
	commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
	cfg := config.Config{Main: "codex", Adversarial: "claude", Commands: commands,
		Codex:        config.CodexConfig{Model: "main", ReasoningEffort: "high"},
		Claude:       config.ClaudeConfig{Model: "new-critic", Effort: "high"},
		Repositories: []config.Repository{{Actions: []string{"inspect"}}}}
	store := openState(t, time.Now().UTC())
	job := withClaudeCritic(testJob(command.Comment))
	job.Command = "inspect"
	accepted := dashboardAccepted(job, time.Now().UTC())
	accepted.CriticSettings = config.RoleModelConfig{Model: "old-critic", Effort: "low"}
	store.Data.Accepted = accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	e := Orchestrator{Main: cfg.Main, Adversarial: cfg.Adversarial, Commands: commands, Store: store,
		CodexModel: cfg.Codex.Model, CodexEffort: cfg.Codex.ReasoningEffort,
		ClaudeModel: cfg.Claude.Model, ClaudeEffort: cfg.Claude.Effort,
		MainModel: cfg.Codex.Model, MainEffort: cfg.Codex.ReasoningEffort,
		CriticModel: cfg.Claude.Model, CriticEffort: cfg.Claude.Effort}
	selected, err := e.WorkerSelection(cfg)
	if err != nil || !slices.Contains(selected.RetainedCriticChoices, accepted.CriticSettings) {
		t.Fatalf("pending Claude pair lost after policy change: %+v, %v", selected.RetainedCriticChoices, err)
	}
	runner, err := NewSelectedRunner(Runner{}, selected)
	if err != nil {
		t.Fatal(err)
	}
	job.Main = selected.Main
	job.Main.Skills = commands["inspect"].Skills
	critic := *selected.Critic
	critic.Model, critic.Effort = accepted.CriticSettings.Model, accepted.CriticSettings.Effort
	job.Critic = &critic
	if err := runner.selection.matches(job); err != nil {
		t.Fatalf("frozen Claude pair assignment rejected: %v", err)
	}
}

func TestWorkerSelectionRetainsFrozenPreparationRetryPair(t *testing.T) {
	commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
	cfg := config.Config{Main: "codex", Commands: commands, Codex: config.CodexConfig{Model: "new-main", ReasoningEffort: "high"},
		Repositories: []config.Repository{{Actions: []string{"inspect"}}}}
	at := time.Now().UTC()
	store := openState(t, at)
	oldPair := config.RoleModelConfig{Model: "approved-before-restart", Effort: "low"}
	store.Data.JobFailures["pending"] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: at.Add(time.Hour),
		MainProvider: "codex", MainSettings: oldPair}
	e := Orchestrator{Main: "codex", Commands: commands, Store: store,
		CodexModel: cfg.Codex.Model, CodexEffort: cfg.Codex.ReasoningEffort,
		MainModel: cfg.Codex.Model, MainEffort: cfg.Codex.ReasoningEffort}
	selected, err := e.WorkerSelection(cfg)
	if err != nil || !slices.Contains(selected.RetainedMainChoices, oldPair) {
		t.Fatalf("pending retry pair was lost after policy removal: %+v, %v", selected, err)
	}
	job := testJob(command.Comment)
	job.Command = "inspect"
	job.Main = selected.Main
	job.Main.Skills = commands["inspect"].Skills
	job.Main.Model, job.Main.Effort = oldPair.Model, oldPair.Effort
	if err := selected.matches(job); err != nil {
		t.Fatalf("frozen retry pair was rejected: %v", err)
	}
	job.Main.Effort = "high"
	if err := selected.matches(job); err == nil {
		t.Fatal("unapproved higher effort reached selected worker")
	}
}

func TestWorkerSelectionSkipsStartedFailureButRetainsActiveAcceptedPair(t *testing.T) {
	commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
	cfg := config.Config{Main: "codex", Commands: commands, Codex: config.CodexConfig{Model: "new-main", ReasoningEffort: "high"},
		Repositories: []config.Repository{{Actions: []string{"inspect"}}}}
	at := time.Now().UTC()
	store := openState(t, at)
	job := testJob(command.Comment)
	oldPair := config.RoleModelConfig{Model: "old-main", Effort: "low"}
	store.Data.JobFailures[job.Identity.JobID] = state.JobFailure{Attempts: 1, ModelCallStarted: true,
		AcceptedAt: at, Deadline: at.Add(time.Hour), MainProvider: "codex", MainSettings: oldPair}
	e := Orchestrator{Main: "codex", Commands: commands, Store: store,
		CodexModel: cfg.Codex.Model, CodexEffort: cfg.Codex.ReasoningEffort,
		MainModel: cfg.Codex.Model, MainEffort: cfg.Codex.ReasoningEffort}
	selected, err := e.WorkerSelection(cfg)
	if err != nil || slices.Contains(selected.RetainedMainChoices, oldPair) {
		t.Fatalf("started failure retained an unusable choice: %+v, %v", selected.RetainedMainChoices, err)
	}
	accepted := dashboardAccepted(job, at)
	accepted.ResultReady = false
	accepted.MainProvider, accepted.MainSettings = "codex", oldPair
	store.Data.Accepted = accepted
	selected, err = e.WorkerSelection(cfg)
	if err != nil || !slices.Contains(selected.RetainedMainChoices, oldPair) {
		t.Fatalf("active accepted choice was lost: %+v, %v", selected.RetainedMainChoices, err)
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

func TestWorkerSelectionRejectsResolvedRoleSettingsDrift(t *testing.T) {
	commands := map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}
	cfg := config.Config{Main: "codex", Adversarial: "claude", MainSettings: config.RoleModelConfig{Model: "main-model", Effort: "high"},
		CriticSettings: config.RoleModelConfig{Model: "critic-model", Effort: "low"}, Commands: commands,
		Repositories: []config.Repository{{Actions: []string{"inspect"}}}}
	e := Orchestrator{Main: "codex", Adversarial: "claude", Commands: commands, MainModel: "wrong-main", MainEffort: "high", CriticModel: "critic-model", CriticEffort: "low"}
	if _, err := e.WorkerSelection(cfg); err == nil || !strings.Contains(err.Error(), "main model settings differ") {
		t.Fatalf("main setting drift accepted: %v", err)
	}
	e.MainModel = "main-model"
	e.CriticEffort = "wrong-effort"
	if _, err := e.WorkerSelection(cfg); err == nil || !strings.Contains(err.Error(), "critic model settings differ") {
		t.Fatalf("critic setting drift accepted: %v", err)
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

func TestNewSelectedRunnerRequiresResolvedModelAndEffort(t *testing.T) {
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
		{name: "main Claude effort", main: RoleSettings{Provider: "claude", Model: "main"}},
		{name: "critic model", main: RoleSettings{Provider: "codex", Model: "main", Effort: "high"}, critic: &RoleSettings{Provider: "claude"}},
		{name: "critic Codex effort", main: RoleSettings{Provider: "claude", Model: "main", Effort: "high"}, critic: &RoleSettings{Provider: "codex", Model: "critic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSelectedRunner(Runner{}, WorkerSelection{Commands: commands, Main: tc.main, Critic: tc.critic}); err == nil {
				t.Fatal("selected runner accepted an unresolved role")
			}
		})
	}
	if _, err := NewSelectedRunner(Runner{}, WorkerSelection{
		Commands: commands,
		Main:     RoleSettings{Provider: "claude", Model: "main", Effort: "high"},
	}); err != nil {
		t.Fatalf("resolved Claude effort was rejected: %v", err)
	}
}
