//go:build windows

package action

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func localWorkerProcessDone(err error) bool {
	// Process.Wait releases the Windows handle before Cmd.Wait drains I/O pipes.
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.EINVAL)
}

func expectedLocalWorkerCancellation(err error, directStopSent bool) bool {
	if !directStopSent {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}
