//go:build !linux

package processgroup

import (
	"errors"
	"os/exec"
	"testing"
)

func TestWorkerRequiresLinux(t *testing.T) {
	handle, err := StartWorker(exec.Command("unused"), func(string) error {
		t.Fatal("unsupported worker reached the recording callback")
		return nil
	})
	if handle != nil || !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("unsupported worker claimed ownership: handle=%v error=%v", handle, err)
	}
	if released, err := new(WorkerHandle).Wait(); released || !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("unsupported worker claimed release: released=%t error=%v", released, err)
	}
}
