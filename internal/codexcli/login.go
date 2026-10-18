package codexcli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

// CheckLogin checks stored authentication without an inference turn. Configured
// API keys are validated by model discovery instead of the unrelated stored login.
func CheckLogin(ctx context.Context, settings config.CodexConfig) (resultErr error) {
	if settings.APIKey != "" {
		return nil
	}
	binary, err := aicli.ExecutablePath("codex")
	if err != nil {
		return err
	}
	home, err := HomePath()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "minion-codex-login-")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(dir)) }()
	env := childEnvironment(os.Environ(), dir, dir, home, false)
	if _, err := aicli.Probe(ctx, binary, "codex", []string{"login", "status"}, dir, env, ""); err != nil {
		return fmt.Errorf("codex login status failed: %w", err)
	}
	return nil
}
