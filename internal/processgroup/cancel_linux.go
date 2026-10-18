//go:build linux

package processgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Configure bounds pipe draining and cancels the original process group.
// The command must be created by exec.CommandContext.
// Numeric group IDs can race leader reaping; positive process ownership is #96 work.
func Configure(cmd *exec.Cmd) {
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
