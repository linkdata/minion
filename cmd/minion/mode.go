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

func runOrchestrateMode(ctx context.Context, owner *action.Orchestrator, watcher *monitor.Monitor, store *state.Store, loop bool, logger *log.Logger, path string, readyTimeout time.Duration) int {
	if terminalRecoveryPending(store) {
		code := recoverTerminalService(ctx, watcher, store, loop, logger)
		if code != 0 || !loop || ctx.Err() != nil {
			return code
		}
	}
	if err := store.SyncPathAncestors(); err != nil {
		logger.Printf("sync selected state path: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	done, closeWorker, err := attachSelectedLocalWorker(ctx, owner, watcher, path, []string{"-mode=execute"}, readyTimeout)
	if err != nil {
		logger.Printf("start selected worker: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	return runSelectedLocalService(ctx, watcher, store, loop, done, logger, closeWorker)
}

func terminalRecoveryPending(store *state.Store) bool {
	accepted := store.Data.Accepted
	if accepted == nil {
		return false
	}
	return accepted.Assigned || accepted.Terminal || store.Data.JobFailures[accepted.JobID].ModelCallStarted || !time.Now().Before(accepted.Deadline)
}

func recoverTerminalService(ctx context.Context, watcher *monitor.Monitor, store *state.Store, loop bool, logger *log.Logger) int {
	if err := store.SyncPathAncestors(); err != nil {
		logger.Printf("sync selected state path: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	return runMonitorLoop(ctx, watcher, store, loop, nil, logger, true)
}
