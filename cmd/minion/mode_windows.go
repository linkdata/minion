//go:build windows

package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

const localModeAvailable = false

func runExecuteMode() int {
	_, _ = os.Stderr.WriteString("minion: -mode=execute is unavailable on Windows until native transport is implemented\n") //nolint:errcheck // -- The command exits with an error even if stderr fails.
	return 2
}

func runOrchestrateMode(_ context.Context, _ *action.Orchestrator, _ *monitor.Monitor, _ *state.Store, _ bool, logger *log.Logger, _ string, _ time.Duration) int {
	logger.Print("-mode=orchestrate is unavailable on Windows until native transport is implemented")
	return 2
}
