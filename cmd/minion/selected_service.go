package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"slices"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/codexcli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func selectedSkillConfigChanged(before, after config.Config, selected map[string]bool) bool {
	if !slices.Equal(before.AlwaysOnSkills, after.AlwaysOnSkills) {
		return true
	}
	for name := range selected {
		old, oldOK := before.Commands[name]
		current, currentOK := after.Commands[name]
		if oldOK != currentOK || old.Action != current.Action || !slices.Equal(old.Skills, current.Skills) {
			return true
		}
	}
	return false
}

func hasNewInstalledCommand(cfg config.Config, selected map[string]bool) bool {
	for _, repo := range cfg.Repositories {
		for _, name := range repo.Actions {
			if !selected[name] {
				installed, err := cfg.CommandInstalled(name)
				if err == nil && installed {
					return true
				}
			}
		}
	}
	return false
}

func modelKeysChanged(before, after config.Config) bool {
	return (before.Main == "codex" || before.Adversarial == "codex") && before.Codex.APIKey != after.Codex.APIKey ||
		(before.Main == "claude" || before.Adversarial == "claude") && before.Claude.APIKey != after.Claude.APIKey
}

func applyModelKeys(owner *action.Orchestrator, cfg config.Config) {
	codex, claude := owner.Runner.Codex, owner.Runner.Claude
	codex.APIKey = cfg.Codex.APIKey
	if cfg.Codex.APIKey == "" && cfg.RemoteExecutor == nil {
		codex = codexcli.CaptureAmbientNativeCredentials(codex)
	}
	claude.APIKey = cfg.Claude.APIKey
	if cfg.RemoteExecutor == nil {
		claude.APIKey = claudeRunnerKey(cfg.Claude.APIKey)
	}
	owner.SetRunnerCredentials(codex, claude)
}

// attachSelectedLocalWorker prepares one selected child before the monitor can
// discover work. Public service mode selection remains with the CLI switch.
func attachSelectedLocalWorker(ctx context.Context, owner *action.Orchestrator, watcher *monitor.Monitor, path string, args []string, readyTimeout time.Duration) (<-chan struct{}, func() error, error) {
	if owner == nil || watcher == nil {
		return nil, nil, errors.New("selected local service requires an owner and monitor")
	}
	selected, done, closeWorker, err := owner.AttachLocalWorker(ctx, watcher.Config, path, args, readyTimeout, func(cmd *exec.Cmd, endpoint action.LocalEndpointAccess, selected action.WorkerSelection, credentials action.WorkerCredentials) error {
		startup := action.WorkerStartup{
			Selection: selected, WorkRoot: owner.WorkRoot, Endpoint: endpoint,
			CodexAPIKey: credentials.CodexAPIKey, CodexNativeAPIKey: credentials.CodexNativeAPIKey,
			CodexAccessToken: credentials.CodexAccessToken, ClaudeAPIKey: credentials.ClaudeAPIKey,
			ClaudeAllowAllUnixSockets: owner.Runner.Claude.AllowAllUnixSockets,
		}
		input, err := action.EncodeWorkerStartup(startup)
		if err != nil {
			return fmt.Errorf("encode selected worker startup: %w", err)
		}
		cmd.Stdin = bytes.NewReader(input)
		cmd.Env = selectedWorkerEnv()
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	owner.ValidateAssignment = func(ctx context.Context, job monitor.Job) error {
		if owner.Store == nil {
			return fmt.Errorf("%w: accepted state store is unavailable", monitor.ErrAcceptanceState)
		}
		return watcher.RevalidateAssignment(ctx, owner.Store, job)
	}
	bindPublicationValidation(owner, watcher)
	watcher.Handler = owner
	watcher.SelectedExecutorID = action.LocalExecutorID
	watcher.SelectedCommands = make(map[string]bool, len(selected.Commands))
	for name := range selected.Commands {
		watcher.SelectedCommands[name] = true
	}
	return done, closeWorker, nil
}

func bindPublicationValidation(owner *action.Orchestrator, watcher *monitor.Monitor) {
	validator := *watcher // Keep startup recovery's repository list while invitations append to watcher.
	owner.ValidatePublication = func(ctx context.Context, job monitor.Job, requireAssigned bool) error {
		return validator.RevalidatePublication(ctx, owner.Store, job, requireAssigned)
	}
}

// runSelectedLocalService owns the selected child across the monitor loop.
func runSelectedLocalService(ctx context.Context, watcher *monitor.Monitor, store *state.Store, loop bool, done <-chan struct{}, logger *log.Logger, closeWorker func() error) (code int) {
	return runSelectedLocalServiceLive(ctx, nil, watcher, store, loop, done, logger, closeWorker, "", nil, 0, nil)
}

func runSelectedLocalServiceLive(ctx context.Context, owner *action.Orchestrator, watcher *monitor.Monitor, store *state.Store, loop bool, done <-chan struct{}, logger *log.Logger, closeWorker func() error, path string, args []string, readyTimeout time.Duration, live *adminLiveConfig) (code int) {
	if done == nil || closeWorker == nil {
		logger.Print("selected local worker is missing")
		return 1
	}
	defer func() {
		if closeWorker == nil {
			return
		}
		if err := closeWorker(); err != nil {
			logger.Printf("close selected local worker: %s", monitor.InertLogText(err.Error()))
			code = 1
		}
	}()
	retryConfig := false // A retry wake goes straight to activation, before another poll.
	for {
		if !retryConfig {
			var updates <-chan struct{}
			if live != nil {
				updates = live.updates
			}
			code = runMonitorLoopWithConfigUpdates(ctx, watcher, store, loop, done, logger, false, updates)
			if code != monitorConfigChanged {
				return code
			}
		}
		retryConfig = false
		cfg, generation := live.next()
		refresh := modelKeysChanged(watcher.Config, cfg) || selectedSkillConfigChanged(watcher.Config, cfg, watcher.SelectedCommands) || hasNewInstalledCommand(cfg, watcher.SelectedCommands)
		if !refresh {
			owner.Commands, owner.AlwaysOnSkills = cfg.Commands, cfg.AlwaysOnSkills
			owner.AIInstructions = cfg.AIInstructions
			owner.CleanCriticRounds = cfg.RequiredCleanRounds
			watcher.Config = cfg
			bindPublicationValidation(owner, watcher)
			live.applied(cfg, generation, nil)
			continue
		}
		oldCfg, oldCommands, oldSkills := watcher.Config, owner.Commands, owner.AlwaysOnSkills
		oldCodex, oldClaude := owner.Runner.Codex, owner.Runner.Claude
		owner.Commands, owner.AlwaysOnSkills = cfg.Commands, cfg.AlwaysOnSkills
		if _, err := owner.WorkerSelection(cfg); err != nil {
			owner.Commands, owner.AlwaysOnSkills = oldCommands, oldSkills
			logger.Printf("activate admin skill change: %s", monitor.InertLogText(err.Error()))
			live.applied(oldCfg, generation, err)
			if code = waitForConfigRetry(ctx, live.updates, done); code != monitorConfigChanged {
				return code
			}
			retryConfig = true
			continue
		}
		if err := closeWorker(); err != nil {
			logger.Printf("close selected worker for config change: %s", monitor.InertLogText(err.Error()))
			live.applied(oldCfg, generation, err)
			return 1
		}
		owner.ClearSelectedExecutor()
		watcher.Config = cfg
		watcher.SelectedCommands = nil
		applyModelKeys(owner, cfg)
		var err error
		done, closeWorker, err = attachSelectedLocalWorker(ctx, owner, watcher, path, args, readyTimeout)
		if err == nil {
			owner.AIInstructions = cfg.AIInstructions
			owner.CleanCriticRounds = cfg.RequiredCleanRounds
			live.applied(cfg, generation, nil)
			continue
		}
		logger.Printf("activate admin worker change: %s", monitor.InertLogText(err.Error()))
		owner.Commands, owner.AlwaysOnSkills = oldCommands, oldSkills
		owner.SetRunnerCredentials(oldCodex, oldClaude)
		watcher.Config = oldCfg
		watcher.SelectedCommands = nil
		var rollbackErr error
		done, closeWorker, rollbackErr = attachSelectedLocalWorker(ctx, owner, watcher, path, args, readyTimeout)
		live.applied(oldCfg, generation, err)
		if rollbackErr != nil {
			logger.Printf("restore selected worker: %s", monitor.InertLogText(rollbackErr.Error()))
			return 1
		}
		if code = waitForConfigRetry(ctx, live.updates, done); code != monitorConfigChanged {
			return code
		}
		retryConfig = true
	}
}
