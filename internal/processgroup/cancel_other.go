//go:build !linux

package processgroup

import (
	"os/exec"
	"time"
)

// Configure preserves CommandContext's process cancellation on other hosts.
func Configure(cmd *exec.Cmd) {
	cmd.WaitDelay = 2 * time.Second
}
