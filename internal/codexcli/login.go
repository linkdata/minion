package codexcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/processgroup"
)

// CheckLogin inspects stored authentication without an inference turn.
// Configured and native environment credentials bypass the stored-login check.
// Model discovery runs separately.
func CheckLogin(ctx context.Context, settings config.CodexConfig) (resultErr error) {
	if settings.APIKey != "" || os.Getenv("CODEX_API_KEY") != "" || os.Getenv("CODEX_ACCESS_TOKEN") != "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
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
	defer func() {
		if !errors.Is(resultErr, processgroup.ErrScopeUnconfirmed) {
			resultErr = errors.Join(resultErr, os.RemoveAll(dir))
		}
	}()
	env := childEnvironment(os.Environ(), dir, dir, home, false)
	if _, err := aicli.Probe(ctx, binary, "codex", []string{"login", "status"}, dir, env, "", ""); err != nil {
		return fmt.Errorf("codex login status failed: %w", err)
	}
	return nil
}
