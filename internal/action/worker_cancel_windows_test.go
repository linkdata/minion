//go:build windows

package action

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestLocalWorkerWindowsProcessDoneClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"reaped handle", syscall.EINVAL, true},
		{"process done", os.ErrProcessDone, true},
		{"terminated open handle", os.NewSyscallError("TerminateProcess", syscall.ERROR_ACCESS_DENIED), true},
		{"duplicate handle denied", os.NewSyscallError("DuplicateHandle", syscall.ERROR_ACCESS_DENIED), false},
		{"other terminate failure", os.NewSyscallError("TerminateProcess", syscall.Errno(6)), false},
		{"unrelated error", errors.New("other kill failure"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := localWorkerProcessDone(test.err); got != test.want {
				t.Fatalf("process done for %v = %t, want %t", test.err, got, test.want)
			}
		})
	}
}
