package action

import (
	"cmp"
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

// WorkerSelection derives a trusted worker boundary from enabled, installed commands.
func (e *Orchestrator) WorkerSelection(cfg config.Config) (WorkerSelection, error) {
	if cmp.Or(cfg.Main, "codex") != e.mainProvider() {
		return WorkerSelection{}, errors.New("worker selection main provider differs from config")
	}
	if cfg.Adversarial != e.Adversarial {
		return WorkerSelection{}, errors.New("worker selection critic provider differs from config")
	}
	selected := WorkerSelection{Commands: make(map[string]config.CommandMapping), Main: e.roleSettings(e.mainProvider(), nil)}
	if e.Adversarial != "" {
		critic := e.roleSettings(e.Adversarial, nil)
		selected.Critic = &critic
	}
	seen := make(map[string]bool)
	for _, repo := range cfg.Repositories {
		for _, name := range repo.Actions {
			if seen[name] {
				continue
			}
			seen[name] = true
			configured, ok := cfg.Commands[name]
			used, present := e.Commands[name]
			if !ok || !present || configured.Action != used.Action || !slices.Equal(configured.Skills, used.Skills) {
				return WorkerSelection{}, fmt.Errorf("worker command %q differs from orchestrator command mapping", name)
			}
			installed, err := cfg.CommandInstalled(name)
			if err != nil {
				e.logf("disable %s: skill unavailable: %v", name, err)
			}
			if installed && err == nil {
				selected.Commands[name] = used
			}
		}
	}
	if len(selected.Commands) == 0 {
		return WorkerSelection{}, errors.New("worker selection has no installed enabled commands")
	}
	runner, err := NewSelectedRunner(Runner{}, selected)
	if err != nil {
		return WorkerSelection{}, err
	}
	return *runner.selection, nil
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
