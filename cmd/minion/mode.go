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
		adminCtx, cancel := context.WithCancel(ctx)
		ctx = adminCtx
		defer cancel()
		admin, err := startAdminServer(ctx, *watcher.Config.Admin, owner, watcher.Config.StatePath, cancel)
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
		logger.Printf("admin dashboard listening on %s", admin.listener.Addr())
	}
	if watcher.Config.RemoteExecutor != nil {
		endpoint, err := attachSelectedRemoteWorker(owner, watcher)
		if err != nil {
			logger.Printf("start selected remote worker: %s", monitor.InertLogText(err.Error()))
			return 1
		}
		return runSelectedRemoteService(ctx, owner, watcher, store, loop, logger, endpoint, readyTimeout)
	}
	done, closeWorker, err := attachSelectedLocalWorker(ctx, owner, watcher, path, []string{"-mode=execute"}, readyTimeout)
	if err != nil {
		logger.Printf("start selected worker: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	return runSelectedLocalService(ctx, watcher, store, loop, done, logger, closeWorker)
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
