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

func TestRunCancelsActiveChildProcess(t *testing.T) {
	binary := fakeExecutable(t, "claude", `printf '%s\n' '`+assistantUsageEvent("partial", 3, 5, 7, 2)+`'
( i=0; while [ -f "$0" ] && [ "$i" -lt 30 ] && [ ! -f "$0.pid.stop" ]; do sleep 1; i=$((i+1)); done ) &
printf '%s\n' "$!" > "$0.pid.tmp"
mv -- "$0.pid.tmp" "$0.pid"
wait
`)
	testsupport.StopOnCleanup(t, binary+".pid.stop")
	workspace := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
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
	defer func() {
		cancel()
		for range done {
		}
	}()
	// The real subprocess must start before cancellation; a synthetic clock
	// cannot establish whether its process group has actually been created.
	waitCtx, stopWait := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopWait()
	child, err := testsupport.WaitForProcess(waitCtx, binary+".pid", done)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, stopCleanup := context.WithTimeout(context.WithoutCancel(t.Context()), 3*time.Second)
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
	// Keep observing this child after cancellation, even if its PID is recycled.
	if err := child.WaitExited(waitCtx); err != nil {
		t.Fatalf("child survived process-group cancellation: %v", err)
	}
}
