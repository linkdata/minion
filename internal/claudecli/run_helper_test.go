package claudecli

import (
	"context"
	"errors"
	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

func Run(ctx context.Context, dir, prompt string, writable bool, outputDir string, settings config.ClaudeConfig) (result aicli.Result, runError error) {
	var session Session
	result, runError = session.Run(ctx, dir, prompt, writable, outputDir, settings)
	if err := session.Close(); err != nil {
		result.Answer = ""
		runError = errors.Join(runError, err)
	}
	return result, runError
}
