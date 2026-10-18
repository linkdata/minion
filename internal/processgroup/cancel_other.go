//go:build !linux

package processgroup

import (
	"os/exec"
	"time"
)

// Configure preserves CommandContext's process cancellation on other hosts.
// The command must be created by exec.CommandContext.
func Configure(cmd *exec.Cmd) {
	cmd.WaitDelay = 2 * time.Second
}
