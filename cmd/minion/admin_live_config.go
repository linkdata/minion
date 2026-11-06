package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sync"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
)

var errAdminRestartRequired = errors.New("configuration changed outside the admin UI; restart Minion before editing here")

// adminLiveConfig keeps disk writes and the running worker's configuration distinct.
// The monitor applies saved edits only between iterations, after an accepted action ends.
type adminLiveConfig struct {
	path           string
	mu             sync.RWMutex
	active         config.Config
	desired        config.Config
	disk           config.Config
	remoteSelected map[string]bool
	generation     uint64
	pending        bool
	lastError      string
	updates        chan struct{}
}

func newAdminLiveConfig(path string, cfg config.Config) (*adminLiveConfig, error) {
	disk, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	resolved := disk
	applyRuntimeModels(&resolved, cfg)
	if !sameFixedConfig(cfg, resolved) {
		return nil, errAdminRestartRequired
	}
	selected := make(map[string]bool)
	if cfg.RemoteExecutor != nil {
		for _, repo := range cfg.Repositories {
			for _, name := range repo.Actions {
				mapping := cfg.Commands[name]
				if len(mapping.Skills) != 0 && mapping.SupportsProvider(cfg.Main) {
					selected[name] = true
				}
			}
		}
	}
	return &adminLiveConfig{path: path, active: cfg, desired: cfg, disk: disk, remoteSelected: selected, updates: make(chan struct{}, 1)}, nil
}

func (a *adminLiveConfig) current() config.Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.active
}

func (a *adminLiveConfig) admit(accept func() error) error {
	// A completed save must not be followed by an acceptance under the old policy.
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.pending {
		return monitor.ErrConfigUpdatePending
	}
	return accept()
}

func (a *adminLiveConfig) snapshot() adminConfigSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return adminConfigSnapshot{Config: a.desired, Pending: a.pending, Error: a.lastError, Remote: a.desired.RemoteExecutor != nil}
}

func sameFixedConfig(a, b config.Config) bool {
	a.Repositories, a.Commands, a.AlwaysOnSkills = b.Repositories, b.Commands, b.AlwaysOnSkills
	a.RequesterPolicies, a.AIInstructions = b.RequesterPolicies, b.AIInstructions
	a.CriticStopping.RequiredCleanRounds = b.CriticStopping.RequiredCleanRounds
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func sameSkillConfig(a, b config.Config) bool {
	return reflect.DeepEqual(a.Commands, b.Commands) && reflect.DeepEqual(a.AlwaysOnSkills, b.AlwaysOnSkills)
}

func cloneConfig(cfg config.Config) (config.Config, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return config.Config{}, err
	}
	var cloned config.Config
	if err := json.Unmarshal(data, &cloned); err != nil {
		return config.Config{}, err
	}
	return cloned, nil
}

func applyRuntimeModels(cfg *config.Config, active config.Config) {
	cfg.Codex.Model, cfg.Codex.ReasoningEffort = active.Codex.Model, active.Codex.ReasoningEffort
	cfg.Claude.Model, cfg.Claude.Effort = active.Claude.Model, active.Claude.Effort
	cfg.MainSettings, cfg.CriticSettings = active.MainSettings, active.CriticSettings
}

// Invited repositories stay on disk but outside the running selection until restart.
func visibleRepositories(current, disk []config.Repository, added map[string]bool) []config.Repository {
	visible := make(map[string]bool, len(current)+len(added))
	for _, repo := range current {
		visible[repo.Name] = true
	}
	for name := range added {
		visible[name] = true
	}
	var selected []config.Repository
	for _, repo := range disk {
		if visible[repo.Name] {
			selected = append(selected, repo)
		}
	}
	return selected
}

func sameEnabledCommands(a, b config.Config) bool {
	if len(a.Repositories) != len(b.Repositories) {
		return false
	}
	for i, repo := range a.Repositories {
		if repo.Name != b.Repositories[i].Name || !slices.Equal(repo.Actions, b.Repositories[i].Actions) {
			return false
		}
	}
	return true
}

func hasRunnableLocalCommand(cfg config.Config) bool {
	for _, repo := range cfg.Repositories {
		for _, name := range repo.Actions {
			if installed, err := cfg.CommandInstalled(name); err == nil && installed {
				return true
			}
		}
	}
	return false
}

func (a *adminLiveConfig) save(edit func(*config.Config) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var activate config.Config
	updated, err := config.Update(a.path, func(raw *config.Config) error {
		before, err := cloneConfig(*raw)
		if err != nil {
			return err
		}
		if err := before.Validate(filepath.Dir(a.path)); err != nil {
			return err
		}
		if !sameFixedConfig(a.disk, before) {
			return errAdminRestartRequired
		}
		after, err := cloneConfig(before)
		if err != nil {
			return err
		}
		if err := edit(&after); err != nil {
			return err
		}
		if err := after.Validate(filepath.Dir(a.path)); err != nil {
			return err
		}
		if !sameFixedConfig(before, after) {
			return errors.New("this setting requires a restart")
		}
		added := make(map[string]bool)
		for _, repo := range after.Repositories {
			if _, existed := before.Repository(repo.Name); !existed {
				added[repo.Name] = true
			}
		}
		activate = after
		activate.Repositories = visibleRepositories(a.desired.Repositories, after.Repositories, added)
		if len(activate.Repositories) == 0 {
			return errAdminRestartRequired
		}
		selectionChanged := !sameSkillConfig(a.desired, activate) || !sameEnabledCommands(a.desired, activate)
		if activate.RemoteExecutor != nil && (!sameSkillConfig(a.desired, activate) || hasUnselectedCommand(a.remoteSelected, activate)) {
			return errors.New("this change requires restarting the remote worker")
		}
		if activate.RemoteExecutor == nil && selectionChanged && hasRunnableLocalCommand(a.desired) && !hasRunnableLocalCommand(activate) {
			return errors.New("at least one enabled command must have its required skills installed")
		}
		if !reflect.DeepEqual(before.Repositories, after.Repositories) {
			raw.Repositories = after.Repositories
		}
		if !reflect.DeepEqual(before.Commands, after.Commands) {
			raw.Commands = after.Commands
		}
		if !reflect.DeepEqual(before.AlwaysOnSkills, after.AlwaysOnSkills) {
			raw.AlwaysOnSkills = after.AlwaysOnSkills
		}
		if !reflect.DeepEqual(before.RequesterPolicies, after.RequesterPolicies) {
			raw.RequesterPolicies = after.RequesterPolicies
		}
		raw.CriticStopping.RequiredCleanRounds = after.CriticStopping.RequiredCleanRounds
		raw.AIInstructions = after.AIInstructions
		return nil
	})
	if err != nil {
		return err
	}
	a.disk = updated
	applyRuntimeModels(&activate, a.active)
	before, _ := json.Marshal(a.desired)
	after, _ := json.Marshal(activate)
	if bytes.Equal(before, after) && a.lastError == "" {
		return nil
	}
	a.desired = activate
	a.generation++
	a.pending = true
	a.lastError = ""
	select {
	case a.updates <- struct{}{}:
	default:
	}
	return nil
}

func (a *adminLiveConfig) next() (config.Config, uint64) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.desired, a.generation
}

func (a *adminLiveConfig) applied(cfg config.Config, generation uint64, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.active = cfg
	}
	if generation != a.generation {
		select {
		case a.updates <- struct{}{}:
		default:
		}
		return
	}
	a.pending = false
	if err != nil {
		a.lastError = fmt.Sprintf("Saved configuration could not be activated: %v", err)
	} else {
		a.lastError = ""
	}
}
