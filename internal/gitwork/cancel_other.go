//go:build !linux

package gitwork

import "os/exec"

func configureGitCancellation(_ *exec.Cmd) {
	// CommandContext kills Git; WaitDelay bounds inherited-pipe cleanup.
}
