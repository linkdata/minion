package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/linkdata/minion/internal/config"
)

var errAdminRestartRequired = errors.New("configuration changed outside the admin UI; restart Minion before editing here")

// adminLiveConfig keeps disk writes and the running worker's configuration distinct.
// The monitor applies saved edits only between iterations, after an accepted action ends.
type adminLiveConfig struct {
	path       string
	mu         sync.RWMutex
	active     config.Config
	desired    config.Config
	disk       config.Config
	generation uint64
	pending    bool
	lastError  string
	updates    chan struct{}
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
	return &adminLiveConfig{path: path, active: cfg, desired: cfg, disk: disk, updates: make(chan struct{}, 1)}, nil
}

func (a *adminLiveConfig) current() config.Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.active
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
	return bytes.Equal(mustConfigJSON(a.Commands, a.AlwaysOnSkills), mustConfigJSON(b.Commands, b.AlwaysOnSkills))
}

func hasNewCommand(before, after config.Config) bool {
	known := make(map[string]bool)
	for _, repo := range before.Repositories {
		for _, name := range repo.Actions {
			known[name] = true
		}
	}
	for _, repo := range after.Repositories {
		for _, name := range repo.Actions {
			if !known[name] {
				return true
			}
		}
	}
	return false
}

func mustConfigJSON(commands map[string]config.CommandMapping, always []string) []byte {
	data, _ := json.Marshal(struct {
		Commands map[string]config.CommandMapping
		Always   []string
	}{commands, always})
	return data
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
	cfg.Codex, cfg.Claude = active.Codex, active.Claude
	cfg.MainSettings, cfg.CriticSettings = active.MainSettings, active.CriticSettings
}

func (a *adminLiveConfig) save(edit func(*config.Config) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
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
		if after.RemoteExecutor != nil && (!sameSkillConfig(before, after) || hasNewCommand(before, after)) {
			return errors.New("this change requires restarting the remote worker")
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
	applyRuntimeModels(&updated, a.active)
	before, _ := json.Marshal(a.desired)
	after, _ := json.Marshal(updated)
	if bytes.Equal(before, after) {
		return nil
	}
	a.desired = updated
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
