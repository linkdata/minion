//go:build !windows

package action

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestExpectedLocalWorkerCancellationRejectsSpontaneousSIGINT(t *testing.T) {
	if os.Getenv("MINION_TEST_WORKER_SELF_SIGINT") == "1" {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		t.Fatal("SIGINT did not stop the helper")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestExpectedLocalWorkerCancellationRejectsSpontaneousSIGINT$")
	cmd.Env = append(os.Environ(), "MINION_TEST_WORKER_SELF_SIGINT=1", "GORACE=atexit_sleep_ms=0")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("self-SIGINT child did not have an exit status: %v", err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
		t.Fatalf("child did not exit from SIGINT: %v", exit.Sys())
	}
	if expectedLocalWorkerCancellation(err, true) {
		t.Fatal("spontaneous SIGINT was treated as Minion's direct kill")
	}
	if !expectedLocalWorkerCancellation(context.Canceled, true) || expectedLocalWorkerCancellation(context.Canceled, false) {
		t.Fatal("context cancellation classification changed")
	}
}
