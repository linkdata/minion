//go:build linux

package claudecli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunCancelsDirectCLIProcess(t *testing.T) {
	binary := fakeExecutable(t, "claude", `printf '%s\n' '`+assistantUsageEvent("partial", 3, 5, 7, 2)+`'
printf '%s\n' "$$" > "$0.pid.tmp"
mv -- "$0.pid.tmp" "$0.pid"
exec sleep 30
`)
	workspace := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result aicli.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		defer close(done)
		result, err := Run(ctx, workspace, "task", false, "", config.ClaudeConfig{})
		done <- outcome{result, err}
	}()
	// The real CLI process must start before cancellation.
	waitCtx, stopWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopWait()
	child, err := testsupport.WaitForProcess(waitCtx, binary+".pid", done)
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, stopCleanup := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCleanup()
		if err := child.Cleanup(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.result.Answer != "" || got.result.UsageComplete || got.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 2}) {
			t.Fatalf("canceled Run lost partial usage: result=%+v error=%v", got.result, got.err)
		}
	case <-waitCtx.Done():
		t.Fatal("cancellation did not finish the run")
	}
	// Keep observing the direct process after cancellation, even if its PID is recycled.
	if err := child.WaitExited(waitCtx); err != nil {
		t.Fatalf("direct CLI process survived cancellation: %v", err)
	}
}
