//go:build !linux

package processgroup

import "os/exec"

// Configure preserves CommandContext's process cancellation on other hosts.
func Configure(_ *exec.Cmd) {}
