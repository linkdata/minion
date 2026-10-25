package action

import (
	"slices"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
)

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
