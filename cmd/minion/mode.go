package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/linkdata/minion/internal/action"
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
		entry := admin.entryURL
		if entry == "" {
			entry = admin.listener.Addr().String() + " (no certificate-valid URL could be inferred)"
		}
		suffix := ""
		if !admin.signInEnabled {
			suffix = " (sign-in disabled until password_hash or OAuth is configured)"
		}
		if admin.entryURL != "" {
			logger.Printf("Minion UI listening on %s%s (bound to %s)", entry, suffix, admin.listener.Addr())
		} else {
			logger.Printf("Minion UI listening on %s%s", entry, suffix)
		}
	} else if watcher.Config.Admin != nil && watcher.Config.Admin.UserModelChoices != nil {
		links, err := openUserLinks(watcher.Config.StatePath + ".link")
		if err != nil {
			logger.Printf("user preferences: %s", monitor.InertLogText(err.Error()))
			return 1
		}
		watcher.UserPreferences = links.preferencesForGitHubID
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
