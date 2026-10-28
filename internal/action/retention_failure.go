package action

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"

	"github.com/linkdata/minion/internal/monitor"
)

// Host I/O can be retried from a saved result only when every cause is a host
// I/O failure. A joined content mismatch must remain terminal even if another
// operation (such as a rename) also failed with a retryable I/O error.
func retryableHostIO(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !retryableHostIO(cause) {
				return false
			}
		}
		return true
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return retryableHostIO(wrapped)
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) ||
		errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) || errors.Is(err, syscall.EIO) ||
		errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.ENOMEM) || errors.Is(err, syscall.ETXTBSY) {
		return true
	}
	if runtime.GOOS == "windows" {
		// Win32 sharing/lock contention and disk-full/quota errors.
		return errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33)) ||
			errors.Is(err, syscall.Errno(39)) || errors.Is(err, syscall.Errno(112)) || errors.Is(err, syscall.Errno(1295))
	}
	return false
}

func retainedRecordFailure(stage string, err error) error {
	marker := monitor.ErrTerminalJob
	if retryableHostIO(err) {
		marker = monitor.ErrPostModelPublication
	}
	return fmt.Errorf("%w: %s: %w", marker, stage, err)
}
