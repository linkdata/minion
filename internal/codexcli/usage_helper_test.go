package codexcli

import (
	"context"
	"time"

	"github.com/linkdata/minion/internal/usage"
)

// collectUsage reads bounded first-line headers of recent rollouts to identify
// this action's agent tree. Unrelated transcripts are never read past their header.
func collectUsage(ctx context.Context, home, rootID string, started time.Time, fallback *usage.Tokens, expectedChildren []string) (usage.Tokens, bool, error) {
	files, ambiguous, complete := usageFiles(ctx, home, rootID, started, nil)
	return collectUsageFiles(ctx, rootID, fallback, expectedChildren, files, ambiguous, complete)
}
