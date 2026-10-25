package action

import (
	"errors"
	"fmt"
	"slices"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

// WorkerSelection comes from trusted startup configuration, never from a Job.
// Main.Skills is supplied by the selected command mapping.
type WorkerSelection struct {
	Commands map[string]config.CommandMapping
	Main     RoleSettings
	Critic   *RoleSettings
}

// NewSelectedRunner freezes one direct worker's assignment boundary for reuse.
func NewSelectedRunner(runner Runner, selected WorkerSelection) (*Runner, error) {
	if runner.selection != nil {
		return nil, errors.New("worker runner already has a selection")
	}
	if len(selected.Commands) == 0 || len(selected.Main.Skills) != 0 {
		return nil, errors.New("worker selection requires commands and command-specific main skills")
	}
	if err := selected.Main.Validate(); err != nil {
		return nil, fmt.Errorf("worker main selection: %w", err)
	}
	if selected.Main.Model == "" || selected.Main.Provider == "codex" && selected.Main.Effort == "" {
		return nil, errors.New("worker main selection requires a resolved model and Codex effort")
	}
	if selected.Critic != nil {
		if err := selected.Critic.Validate(); err != nil {
			return nil, fmt.Errorf("worker critic selection: %w", err)
		}
		if selected.Critic.Model == "" || selected.Critic.Provider == "codex" && selected.Critic.Effort == "" {
			return nil, errors.New("worker critic selection requires a resolved model and Codex effort")
		}
		if len(selected.Critic.Skills) != 0 || selected.Critic.Provider == selected.Main.Provider {
			return nil, errors.New("worker critic selection must use the other provider and only always-on skills")
		}
	}
	frozen := WorkerSelection{Commands: make(map[string]config.CommandMapping, len(selected.Commands)), Main: cloneRoleSettings(selected.Main)}
	if selected.Critic != nil {
		critic := cloneRoleSettings(*selected.Critic)
		frozen.Critic = &critic
	}
	for name, mapping := range selected.Commands {
		switch command.Action(mapping.Action) {
		case command.Review, command.Fix, command.Comment, command.SecurityAudit:
		default:
			return nil, fmt.Errorf("worker command %q has unsupported action", name)
		}
		role := selected.Main
		role.Skills = mapping.Skills
		if !workspace.ValidIdentitySegment(name) || len(mapping.Skills) == 0 || !mapping.SupportsProvider(role.Provider) {
			return nil, fmt.Errorf("worker command %q has invalid selection", name)
		}
		if err := role.Validate(); err != nil {
			return nil, fmt.Errorf("worker command %q: %w", name, err)
		}
		mapping.Skills = slices.Clone(mapping.Skills)
		frozen.Commands[name] = mapping
	}
	runner.selection = &frozen
	return &runner, nil
}

func cloneRoleSettings(role RoleSettings) RoleSettings {
	role.Skills = slices.Clone(role.Skills)
	role.AlwaysOnSkills = slices.Clone(role.AlwaysOnSkills)
	return role
}

func sameRoleSettings(a, b RoleSettings) bool {
	return a.Provider == b.Provider && a.Model == b.Model && a.Effort == b.Effort &&
		slices.Equal(a.Skills, b.Skills) && slices.Equal(a.AlwaysOnSkills, b.AlwaysOnSkills)
}

func (s *WorkerSelection) matches(job Job) error {
	mapping, ok := s.Commands[job.Command]
	if !ok || mapping.Action != string(job.Action) {
		return errors.New("assignment command differs from worker selection")
	}
	main := s.Main
	main.Skills = mapping.Skills
	if !sameRoleSettings(job.Main, main) {
		return errors.New("assignment main role differs from worker selection")
	}
	if (job.Critic == nil) != (s.Critic == nil) || job.Critic != nil && !sameRoleSettings(*job.Critic, *s.Critic) {
		return errors.New("assignment critic role differs from worker selection")
	}
	return nil
}
