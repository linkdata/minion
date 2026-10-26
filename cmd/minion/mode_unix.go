//go:build !windows

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
	"github.com/linkdata/minion/internal/workspace"
)

const localModeAvailable = true

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
	workRoot, err := workspace.CanonicalPath(owner.WorkRoot)
	if err != nil {
		logger.Printf("worker work root: %v", err)
		return 1
	}
	parent, err := workspace.CanonicalPath(os.TempDir())
	if err != nil {
		logger.Printf("worker runtime parent: %v", err)
		return 1
	}
	runtimeDir, err := os.MkdirTemp(parent, "minion-exec-")
	if err != nil {
		logger.Printf("create worker runtime: %v", err)
		return 1
	}
	defer func() {
		if err := os.Remove(runtimeDir); err != nil {
			logger.Printf("remove worker runtime: %v", err)
			code = 1
		}
	}()
	if workspace.Within(workRoot, runtimeDir) {
		logger.Printf("worker runtime %q overlaps the model work root", runtimeDir)
		return 1
	}
	done, closeWorker, err := attachSelectedLocalWorker(ctx, owner, watcher, runtimeDir, path, []string{"-mode=execute"}, readyTimeout)
	if err != nil {
		logger.Printf("start selected worker: %s", monitor.InertLogText(err.Error()))
		return 1
	}
	return runSelectedLocalService(ctx, watcher, store, loop, done, logger, closeWorker)
}
