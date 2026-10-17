//go:build linux

package processgroup

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestLeaderExitBoundsPipeDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30 &\nexit 0\n")
	Configure(cmd)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Normal leader exit does not kill descendants. Always clean this owned
	// group even when WaitDelay or a test assertion fails.
	defer func() {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Error(err)
		}
	}()
	started := time.Now()
	err := cmd.Wait()
	if !errors.Is(err, exec.ErrWaitDelay) || ctx.Err() != nil || time.Since(started) > 5*time.Second {
		t.Fatalf("pipe drain elapsed=%v err=%v parent=%v", time.Since(started), err, ctx.Err())
	}
}
