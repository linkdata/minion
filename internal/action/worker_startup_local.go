package action

import (
	"context"
	"fmt"
	"io"
)

// RunSelectedLocalWorker reads one startup message, preflights, then pulls Jobs.
func RunSelectedLocalWorker(ctx context.Context, input io.Reader) error {
	return runSelectedLocalWorker(ctx, input, nil)
}

func runSelectedLocalWorker(ctx context.Context, input io.Reader, call modelCall) error {
	startup, err := decodeWorkerStartup(input)
	if err != nil {
		return err
	}
	runner, err := startup.runner(call)
	if err != nil {
		return err
	}
	if err := runner.PreflightSelection(ctx); err != nil {
		return err
	}
	// The startup copy authenticates preflight only; each assignment carries fresh credentials.
	startup.clearCredentials(runner)
	client, err := NewLocalExecutionClient(startup.Endpoint)
	if err != nil {
		return fmt.Errorf("worker endpoint: %w", err)
	}
	startup.Endpoint.Token = ""
	defer client.CloseIdleConnections()
	return RunHTTPLoop(ctx, client, startup.Endpoint.BaseURL, "local", runner)
}

func (s *WorkerStartup) clearCredentials(runner *Runner) {
	s.CodexAPIKey, s.CodexNativeAPIKey, s.CodexAccessToken, s.ClaudeAPIKey = "", "", "", ""
	runner.Codex.APIKey, runner.Codex.NativeAPIKey, runner.Codex.NativeAccessToken, runner.Claude.APIKey = "", "", "", ""
}
