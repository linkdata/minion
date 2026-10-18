package codexcli

import (
	"context"
	"errors"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

// Run executes one task in a new conversation and removes its scratch paths.
// Use Session.Run to continue an action conversation across invocations.
func Run(ctx context.Context, dir, prompt string, writable bool, outputDir string, settings config.CodexConfig) (result aicli.Result, runError error) {
	var session Session
	defer func() {
		if err := session.Close(); err != nil {
			result.Answer = ""
			runError = errors.Join(runError, err)
		}
	}()
	return session.Run(ctx, dir, prompt, writable, outputDir, settings)
}
