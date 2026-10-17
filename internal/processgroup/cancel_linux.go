//go:build linux

package processgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Configure makes context cancellation terminate the command's process group.
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
