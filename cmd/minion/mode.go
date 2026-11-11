package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func runExecuteMode() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	defer stop()
	if err := action.RunSelectedLocalWorker(ctx, os.Stdin); err != nil {
		// The parent accepts only fixed preflight messages from child stderr.
		fmt.Fprintln(os.Stderr, "minion preflight:", monitor.InertLogText(err.Error())) //nolint:errcheck // -- The worker is exiting after this diagnostic.
		return 1
	}
	return 0
}

func runOrchestrateMode(ctx context.Context, owner *action.Orchestrator, watcher *monitor.Monitor, store *state.Store, loop bool, logger *log.Logger, path string, readyTimeout time.Duration) (code int) {
	var live *adminLiveConfig
	if terminalRecoveryPending(store) {
		code = recoverAcceptedService(ctx, watcher, store, loop, nil, logger)
		if code != 0 || !loop || ctx.Err() != nil {
			return code
		}
	}
	if err := store.SyncPathAncestors(); err != nil {
		logger.Printf("sync selected state path: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	if err := watcher.ReconcilePublicationWrites(ctx, store); err != nil {
		logger.Printf("reconcile issued publication before worker startup: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	if watcher.Config.Admin != nil && loop {
		var err error
		live, err = newAdminLiveConfig(watcher.ConfigPath, watcher.Config)
		if err != nil {
			logger.Printf("admin configuration: %s", monitor.InertLogText(err.Error()))
			return 1
		}
		watcher.BeforeAccept = live.admit
		adminCtx, cancel := context.WithCancel(ctx)
		ctx = adminCtx
		defer cancel()
		admin, err := startAdminServer(ctx, *watcher.Config.Admin, owner, watcher.Config.StatePath, cancel, live)
		if err != nil {
			logger.Printf("admin dashboard: %s", monitor.InertLogText(err.Error()))
			return 1
		}
		defer func() {
			if err := admin.Close(); err != nil {
				logger.Printf("admin dashboard failed: %s", monitor.InertLogText(err.Error()))
				code = 1
			}
		}()
		if admin.preferences != nil && watcher.Config.Admin.UserModelChoices != nil {
			watcher.UserPreferences = admin.preferences.preferencesForGitHubID
		}
		suffix := ""
		if !admin.signInEnabled {
			suffix = " (sign-in disabled until password_hash or OAuth is configured)"
		}
		if admin.entryURL == "" {
			logger.Printf("Minion UI bound to %s%s; set -listenurl to a reachable HTTPS URL covered by the admin certificate", admin.listener.Addr(), suffix)
		} else {
			logger.Printf("Minion UI listening on %s%s (bound to %s)", admin.entryURL, suffix, admin.listener.Addr())
		}
	} else if watcher.Config.Admin != nil && watcher.Config.Admin.UserModelChoices != nil && len(watcher.Config.Repositories) > 0 {
		links, err := openUserLinks(watcher.Config.StatePath + ".link")
		if err != nil {
			logger.Printf("user preferences: %s", monitor.InertLogText(err.Error()))
			return 1
		}
		watcher.UserPreferences = links.preferencesForGitHubID
	}
	if len(watcher.Config.Repositories) == 0 {
		return runInvitationOnly(ctx, watcher, store, loop, logger, live)
	}
	if watcher.Config.RemoteExecutor != nil {
		endpoint, err := attachSelectedRemoteWorker(owner, watcher)
		if err != nil {
			logger.Printf("start selected remote worker: %s", monitor.InertLogText(err.Error()))
			return 1
		}
		return runSelectedRemoteService(ctx, owner, watcher, store, loop, logger, endpoint, readyTimeout, live)
	}
	done, closeWorker, err := attachSelectedLocalWorker(ctx, owner, watcher, path, []string{"-mode=execute"}, readyTimeout)
	if err != nil {
		logger.Printf("start selected worker: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	return runSelectedLocalServiceLive(ctx, owner, watcher, store, loop, done, logger, closeWorker, path, []string{"-mode=execute"}, readyTimeout, live)
}

func runInvitationOnly(ctx context.Context, watcher *monitor.Monitor, store *state.Store, loop bool, logger *log.Logger, live *adminLiveConfig) int {
	// A newly accepted repository waits for a restart and worker selection.
	watcher.SelectedCommands = map[string]bool{}
	logger.Print("waiting for repository invitations")
	var applied uint64
	waitForRestart := func() int {
		if loop {
			<-ctx.Done() // Keep the admin UI open without making more invitation decisions.
		}
		return 0
	}
	for {
		if live != nil && live.firstRepositoryNeedsRestart() {
			return waitForRestart()
		}
		if live != nil {
			cfg, generation := live.next()
			watcher.Config = cfg
			if generation != applied {
				live.applied(cfg, generation, nil)
				applied = generation
			}
		}
		next, err := watcher.RunOnce(ctx, store)
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			if errors.Is(err, monitor.ErrConfigUpdatePending) {
				continue
			}
			logger.Printf("monitor pass: %s", monitor.InertLogText(err.Error()))
			if !loop || errors.Is(err, monitor.ErrAcceptanceState) {
				return 1
			}
		}
		if err == nil && live != nil {
			var runtime config.AdminConfig
			if watcher.Config.Admin != nil {
				runtime = *watcher.Config.Admin
			}
			saved, err := config.LoadWithAdminRuntime(watcher.ConfigPath, runtime)
			if err != nil {
				logger.Printf("check invited repositories: %s", monitor.InertLogText(err.Error()))
				return 1
			}
			if len(saved.Repositories) > 0 {
				live.markFirstRepositorySaved()
				return waitForRestart()
			}
		}
		if !loop {
			return 0
		}
		wait := time.Until(next)
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		var updates <-chan struct{}
		if live != nil {
			updates = live.updates
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0
		case <-updates:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func terminalRecoveryPending(store *state.Store) bool {
	return terminalRecoveryPendingAt(store, time.Now())
}

func terminalRecoveryPendingAt(store *state.Store, now time.Time) bool {
	accepted := store.Data.Accepted
	if accepted == nil {
		return false
	}
	_, decided := store.Data.TerminalDecisions[accepted.JobID]
	return accepted.Assigned || accepted.Terminal || accepted.FixedRefusal != "" || decided ||
		store.Data.JobFailures[accepted.JobID].ModelCallStarted || !now.Before(accepted.Deadline)
}

func recoverAcceptedService(ctx context.Context, watcher *monitor.Monitor, store *state.Store, loop bool, workerExited <-chan struct{}, logger *log.Logger) int {
	if err := store.SyncPathAncestors(); err != nil {
		logger.Printf("sync selected state path: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	return runMonitorLoop(ctx, watcher, store, loop, workerExited, logger, true)
}
