// Package executil runs direct child commands with a bounded pipe drain.
package executil

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

const waitDelay = 2 * time.Second

// Prepare lets a contextual command handle interruption before a bounded
// force-kill, and bounds the wait for inherited output pipes.
func Prepare(cmd *exec.Cmd) {
	// shortcut: direct-process signals cannot prove descendant cleanup; use VM or container isolation if that becomes required.
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			err := cmd.Process.Signal(os.Interrupt)
			if err == nil || errors.Is(err, os.ErrProcessDone) {
				return err
			}
			return cmd.Process.Kill()
		}
	}
	if cmd.WaitDelay <= 0 || cmd.WaitDelay > waitDelay {
		cmd.WaitDelay = waitDelay
	}
}

func Run(cmd *exec.Cmd) error {
	Prepare(cmd)
	return cmd.Run()
}
