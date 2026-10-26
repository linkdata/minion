//go:build !windows

package action

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func localWorkerProcessDone(err error) bool {
	return errors.Is(err, os.ErrProcessDone)
}

func expectedLocalWorkerCancellation(err error, directStopSent bool) bool {
	if !directStopSent {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true // Cmd.Wait can report context cancellation after a successful exit.
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}
