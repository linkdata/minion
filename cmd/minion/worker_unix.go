//go:build !windows

package main

import (
	"context"

	"github.com/linkdata/minion/internal/action"
)

func runSelectedLocalWorker(ctx context.Context, socketPath string, runner action.Runner, selection action.WorkerSelection) error {
	selected, err := action.NewSelectedRunner(runner, selection)
	if err != nil {
		return err
	}
	if err := selected.PreflightSelection(ctx); err != nil {
		return err
	}
	client, err := action.NewUnixExecutionClient(socketPath)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	return action.RunHTTPLoop(ctx, client, action.UnixExecutionBaseURL, "local", selected)
}
