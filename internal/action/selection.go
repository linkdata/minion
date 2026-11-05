package action

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

// WorkerSelection comes from trusted startup configuration, never from a Job.
// Main.Skills is supplied by the selected command mapping.
type WorkerSelection struct {
	Commands              map[string]config.CommandMapping `json:"commands"`
	Main                  RoleSettings                     `json:"main"`
	Critic                *RoleSettings                    `json:"critic,omitempty"`
	MainChoices           []config.RoleModelConfig         `json:"main_choices,omitempty"`
	CriticChoices         []config.RoleModelConfig         `json:"critic_choices,omitempty"`
	RetainedMainChoices   []config.RoleModelConfig         `json:"retained_main_choices,omitempty"`
	RetainedCriticChoices []config.RoleModelConfig         `json:"retained_critic_choices,omitempty"`
}

// WorkerSelection derives a trusted worker boundary from enabled, installed commands.
func (e *Orchestrator) WorkerSelection(cfg config.Config) (WorkerSelection, error) {
	if cmp.Or(cfg.Main, "codex") != e.mainProvider() {
		return WorkerSelection{}, errors.New("worker selection main provider differs from config")
	}
	if cfg.Adversarial != e.Adversarial {
		return WorkerSelection{}, errors.New("worker selection critic provider differs from config")
	}
	if err := e.matchConfiguredRoleModels(cfg); err != nil {
		return WorkerSelection{}, err
	}
	selected := WorkerSelection{Commands: make(map[string]config.CommandMapping), Main: e.mainRoleSettings(nil),
		MainChoices: slices.Clone(cfg.UserChoices(false)), CriticChoices: slices.Clone(cfg.UserChoices(true))}
	if e.Adversarial != "" {
		critic := e.criticRoleSettings()
		selected.Critic = &critic
	}
	if e.Store != nil {
		if err := e.Store.WithLock(func() error {
			addFrozen := func(main, critic config.RoleModelConfig) {
				mainDefault := config.RoleModelConfig{Model: selected.Main.Model, Effort: selected.Main.Effort}
				if main != (config.RoleModelConfig{}) && main.ValidSetting() &&
					main != mainDefault && !slices.Contains(selected.MainChoices, main) && !slices.Contains(selected.RetainedMainChoices, main) {
					selected.RetainedMainChoices = append(selected.RetainedMainChoices, main)
				}
				if selected.Critic != nil {
					criticDefault := config.RoleModelConfig{Model: selected.Critic.Model, Effort: selected.Critic.Effort}
					if critic != (config.RoleModelConfig{}) && critic.ValidSetting() &&
						critic != criticDefault && !slices.Contains(selected.CriticChoices, critic) && !slices.Contains(selected.RetainedCriticChoices, critic) {
						selected.RetainedCriticChoices = append(selected.RetainedCriticChoices, critic)
					}
				}
			}
			if accepted := e.Store.Data.Accepted; accepted != nil && !accepted.Terminal {
				if accepted.MainProvider == selected.Main.Provider &&
					(accepted.CriticProvider == "" || selected.Critic != nil && accepted.CriticProvider == selected.Critic.Provider) {
					addFrozen(accepted.MainSettings, accepted.CriticSettings)
				}
			}
			for _, failure := range e.Store.Data.JobFailures {
				if failure.ModelCallStarted || failure.MainProvider != selected.Main.Provider || failure.AcceptedAt.IsZero() || !time.Now().Before(failure.Deadline) ||
					failure.CriticProvider != "" && (selected.Critic == nil || failure.CriticProvider != selected.Critic.Provider) ||
					failure.CriticProvider == "" && selected.Critic != nil {
					continue
				}
				addFrozen(failure.MainSettings, failure.CriticSettings)
			}
			return nil
		}); err != nil {
			return WorkerSelection{}, err
		}
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

func (e *Orchestrator) matchConfiguredRoleModels(cfg config.Config) error {
	for _, role := range []struct {
		name       string
		configured config.RoleModelConfig
		selected   RoleSettings
	}{
		{name: "main", configured: cfg.RoleModelSettings(false), selected: e.mainRoleSettings(nil)},
		{name: "critic", configured: cfg.RoleModelSettings(true), selected: e.criticRoleSettings()},
	} {
		if role.name == "critic" && cfg.Adversarial == "" {
			continue
		}
		if role.configured.Model != "" && role.configured.Model != role.selected.Model ||
			role.configured.Effort != "" && role.configured.Effort != role.selected.Effort {
			return fmt.Errorf("worker %s model settings differ from config", role.name)
		}
	}
	return nil
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
	if selected.Main.Model == "" || selected.Main.Effort == "" {
		return nil, errors.New("worker main selection requires a resolved model and effort")
	}
	if selected.Critic != nil {
		if err := selected.Critic.Validate(); err != nil {
			return nil, fmt.Errorf("worker critic selection: %w", err)
		}
		if selected.Critic.Model == "" || selected.Critic.Effort == "" {
			return nil, errors.New("worker critic selection requires a resolved model and effort")
		}
		if len(selected.Critic.Skills) != 0 {
			return nil, errors.New("worker critic selection accepts only always-on skills")
		}
	}
	if selected.Critic == nil && (len(selected.CriticChoices) != 0 || len(selected.RetainedCriticChoices) != 0) {
		return nil, errors.New("worker critic choices require a critic")
	}
	if err := validateRoleChoices("main", selected.Main, selected.MainChoices); err != nil {
		return nil, err
	}
	if err := validateRoleChoices("retained main", selected.Main, selected.RetainedMainChoices); err != nil {
		return nil, err
	}
	if selected.Critic != nil {
		if err := validateRoleChoices("critic", *selected.Critic, selected.CriticChoices); err != nil {
			return nil, err
		}
		if err := validateRoleChoices("retained critic", *selected.Critic, selected.RetainedCriticChoices); err != nil {
			return nil, err
		}
	}
	frozen := WorkerSelection{Commands: make(map[string]config.CommandMapping, len(selected.Commands)), Main: cloneRoleSettings(selected.Main),
		MainChoices: slices.Clone(selected.MainChoices), CriticChoices: slices.Clone(selected.CriticChoices),
		RetainedMainChoices: slices.Clone(selected.RetainedMainChoices), RetainedCriticChoices: slices.Clone(selected.RetainedCriticChoices)}
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

func validateRoleChoices(name string, base RoleSettings, choices []config.RoleModelConfig) error {
	for i, pair := range choices {
		role := base
		role.Model, role.Effort = pair.Model, pair.Effort
		if !pair.ValidChoice() || slices.Contains(choices[:i], pair) || role.Validate() != nil {
			return fmt.Errorf("worker %s choice is invalid or duplicated", name)
		}
	}
	return nil
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
	selectedMain := main
	selectedMain.Model, selectedMain.Effort = job.Main.Model, job.Main.Effort
	if !sameRoleSettings(job.Main, selectedMain) || !roleChoiceAllowed(main, s.MainChoices, s.RetainedMainChoices, job.Main) {
		return errors.New("assignment main role differs from worker selection")
	}
	if (job.Critic == nil) != (s.Critic == nil) {
		return errors.New("assignment critic role differs from worker selection")
	}
	if job.Critic != nil {
		selectedCritic := *s.Critic
		selectedCritic.Model, selectedCritic.Effort = job.Critic.Model, job.Critic.Effort
		if !sameRoleSettings(*job.Critic, selectedCritic) || !roleChoiceAllowed(*s.Critic, s.CriticChoices, s.RetainedCriticChoices, *job.Critic) {
			return errors.New("assignment critic role differs from worker selection")
		}
	}
	return nil
}

func roleChoiceAllowed(defaultRole RoleSettings, choices, retained []config.RoleModelConfig, actual RoleSettings) bool {
	pair := config.RoleModelConfig{Model: actual.Model, Effort: actual.Effort}
	return pair == (config.RoleModelConfig{Model: defaultRole.Model, Effort: defaultRole.Effort}) ||
		slices.Contains(choices, pair) || slices.Contains(retained, pair)
}
