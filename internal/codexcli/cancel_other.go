//go:build !linux

package codexcli

import "os/exec"

func configureCancellation(_ *exec.Cmd) {
	// CommandContext terminates the Codex process on cancellation.
}
