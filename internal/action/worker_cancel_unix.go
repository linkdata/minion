//go:build !windows

package action

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

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
	return ok && status.Signaled() && (status.Signal() == syscall.SIGINT || status.Signal() == syscall.SIGKILL)
}
