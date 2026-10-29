package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

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
	owner.ValidateFixedRefusal = func(ctx context.Context, job monitor.Job) error {
		return validator.RevalidateFixedRefusal(ctx, owner.Store, job)
	}
}

// runSelectedLocalService owns the selected child across the monitor loop.
func runSelectedLocalService(ctx context.Context, watcher *monitor.Monitor, store *state.Store, loop bool, done <-chan struct{}, logger *log.Logger, closeWorker func() error) (code int) {
	if done == nil || closeWorker == nil {
		logger.Print("selected local worker is missing")
		return 1
	}
	defer func() {
		if err := closeWorker(); err != nil {
			logger.Printf("close selected local worker: %s", monitor.InertLogText(err.Error()))
			code = 1
		}
	}()
	return runMonitorLoop(ctx, watcher, store, loop, done, logger, false)
}
