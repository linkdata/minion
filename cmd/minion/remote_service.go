package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func hasUnselectedCommand(selected map[string]bool, cfg config.Config, auditHostSupported bool) bool {
	for _, repo := range cfg.Repositories {
		for _, name := range repo.Actions {
			mapping := cfg.Commands[name]
			if len(mapping.Skills) != 0 && mapping.SupportsProvider(cfg.Main) && mapping.SupportedOnHost() &&
				(mapping.Action != string(command.SecurityAudit) || auditHostSupported) && !selected[name] {
				return true
			}
		}
	}
	return false
}

func attachSelectedRemoteWorker(owner *action.Orchestrator, watcher *monitor.Monitor) (*action.RemoteEndpoint, error) {
	if owner == nil || watcher == nil || watcher.Config.RemoteExecutor == nil {
		return nil, errors.New("selected remote service requires an owner, monitor and remote executor config")
	}
	remote := watcher.Config.RemoteExecutor
	certificate, err := loadTLSCertificate("remote", remote.TLSCertFile, remote.TLSKeyFile)
	if err != nil {
		return nil, err
	}
	commands, endpoint, err := owner.AttachRemoteWorker(watcher.Config, certificate)
	if err != nil {
		return nil, err
	}
	owner.ValidateAssignment = func(ctx context.Context, job monitor.Job) error {
		if owner.Store == nil {
			return fmt.Errorf("%w: accepted state store is unavailable", monitor.ErrAcceptanceState)
		}
		return watcher.RevalidateAssignment(ctx, owner.Store, job)
	}
	bindPublicationValidation(owner, watcher)
	watcher.Handler = owner
	watcher.SelectedExecutorID = remote.ExecutorID
	watcher.SelectedCommands = commands
	watcher.AfterAcceptedCleanup = endpoint.RequireFreshPull
	return endpoint, nil
}

func loadTLSCertificate(kind, certFile, keyFile string) (tls.Certificate, error) {
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load %s TLS certificate: %w", kind, err)
	}
	return certificate, nil
}

func runSelectedRemoteService(ctx context.Context, owner *action.Orchestrator, watcher *monitor.Monitor, store *state.Store, loop bool, logger *log.Logger, endpoint *action.RemoteEndpoint, readyTimeout time.Duration, live *adminLiveConfig) (code int) {
	if endpoint == nil {
		logger.Print("selected remote worker is missing")
		return 1
	}
	defer func() {
		if err := endpoint.Close(); err != nil {
			logger.Printf("close selected remote worker: %s", monitor.InertLogText(err.Error()))
			code = 1
		}
	}()
	if err := watcher.ClearOrphanAcceptedResult(store); err != nil {
		logger.Printf("clear completed remote result: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	if store.Data.Accepted != nil {
		code = recoverAcceptedService(ctx, watcher, store, true, endpoint.Done(), logger)
		if code != 0 || !loop || ctx.Err() != nil {
			return code
		}
	}
	capacityCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	capacityDone := make(chan error, 1)
	go func() { capacityDone <- owner.WaitCapacity(capacityCtx) }()
	var updates <-chan struct{}
	if live != nil {
		updates = live.updates
	}
	retryConfig := false // An update consumed before capacity must activate before polling.
	var err error
capacity:
	for {
		select {
		case err = <-capacityDone:
			break capacity
		case <-updates:
			retryConfig = true
			cfg, _ := live.next()
			current := watcher.Config
			current.Codex.APIKey = owner.Runner.Codex.APIKey
			current.Claude.APIKey = owner.Runner.Claude.APIKey
			if modelKeysChanged(current, cfg) {
				oldCodex, oldClaude := owner.Runner.Codex, owner.Runner.Claude
				applyModelKeys(owner, cfg)
				if keyErr := owner.UpdateRemoteWorkerCredentials(); keyErr != nil {
					owner.SetRunnerCredentials(oldCodex, oldClaude)
					logger.Printf("activate admin model key before worker preflight: %s", monitor.InertLogText(keyErr.Error()))
				}
			}
		}
	}
	cancel()
	if err != nil {
		logger.Printf("wait for selected remote worker: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	auditHostSupported := endpoint.AuditHostSupported()
	if !auditHostSupported {
		for name := range watcher.SelectedCommands {
			if watcher.Config.Commands[name].Action == string(command.SecurityAudit) {
				delete(watcher.SelectedCommands, name)
			}
		}
	}
	if live != nil {
		live.setRemoteSelected(watcher.SelectedCommands, auditHostSupported)
	}
	if len(watcher.SelectedCommands) == 0 {
		logger.Print("selected remote worker has no supported commands")
		return 1
	}
	for {
		if !retryConfig {
			var updates <-chan struct{}
			if live != nil {
				updates = live.updates
			}
			code = runMonitorLoopWithConfigUpdates(ctx, watcher, store, loop, endpoint.Done(), logger, false, updates)
			if code != monitorConfigChanged {
				return code
			}
		}
		retryConfig = false
		cfg, generation := live.next()
		if !sameSkillConfig(watcher.Config, cfg) || hasUnselectedCommand(watcher.SelectedCommands, cfg, auditHostSupported) {
			err := errors.New("remote worker selection cannot change while connected")
			live.applied(watcher.Config, generation, err)
			logger.Printf("activate admin config: %s", monitor.InertLogText(err.Error()))
			if code = waitForConfigRetry(ctx, live.updates, endpoint.Done()); code != monitorConfigChanged {
				return code
			}
			retryConfig = true
			continue
		}
		if modelKeysChanged(watcher.Config, cfg) {
			oldCodex, oldClaude := owner.Runner.Codex, owner.Runner.Claude
			applyModelKeys(owner, cfg)
			if err := owner.UpdateRemoteWorkerCredentials(); err != nil {
				owner.SetRunnerCredentials(oldCodex, oldClaude)
				live.applied(watcher.Config, generation, err)
				logger.Printf("activate admin model key: %s", monitor.InertLogText(err.Error()))
				if code = waitForConfigRetry(ctx, live.updates, endpoint.Done()); code != monitorConfigChanged {
					return code
				}
				retryConfig = true
				continue
			}
		}
		syncUnselectedModelKeys(owner, cfg)
		watcher.Config = cfg
		owner.AIInstructions = cfg.AIInstructions
		owner.CleanCriticRounds = cfg.RequiredCleanRounds
		bindPublicationValidation(owner, watcher)
		live.applied(cfg, generation, nil)
	}
}
