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
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.EINVAL) {
		return true
	}
	// TerminateProcess also reports access denied after the child has exited.
	// The CreateProcess-owned handle has terminate rights; keep errors from
	// DuplicateHandle and other operations visible.
	var call *os.SyscallError
	return errors.As(err, &call) && call.Syscall == "TerminateProcess" && errors.Is(call.Err, syscall.ERROR_ACCESS_DENIED)
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
