//go:build linux

package claudecli

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunRejectsCompletedAnswerAfterPipeDrainOverrun(t *testing.T) {
	binary := fakeExecutable(t, "claude", `cat >/dev/null
printf '%s\n' '`+successfulResult+`'
( i=0; while [ -f "$0" ] && [ "$i" -lt 30 ] && [ ! -f "$0.pid.stop" ]; do sleep 1; i=$((i+1)); done ) &
printf '%s\n' "$!" > "$0.pid.tmp"
mv -- "$0.pid.tmp" "$0.pid"
exit 0
`)
	testsupport.StopOnCleanup(t, binary+".pid.stop")
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	type outcome struct {
		result aicli.Result
		err    error
	}
	done := make(chan outcome, 1)
	workspace := t.TempDir()
	go func() {
		defer close(done)
		result, err := Run(ctx, workspace, "task", false, "", config.ClaudeConfig{})
		done <- outcome{result, err}
	}()
	child, err := testsupport.WaitForProcess(ctx, binary+".pid", done)
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	t.Cleanup(func() {
		waitCtx, stopWait := context.WithTimeout(context.WithoutCancel(t.Context()), 3*time.Second)
		defer stopWait()
		if err := child.Cleanup(waitCtx); err != nil {
			t.Errorf("finite fixture child remained live after cleanup: %v", err)
		}
	})
	select {
	case got := <-done:
		if !errors.Is(got.err, exec.ErrWaitDelay) || ctx.Err() != nil || got.result.Answer != "" || got.result.UsageComplete || got.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
			t.Fatalf("pipe overrun accepted completed answer or lost usage: result=%+v error=%v parent=%v", got.result, got.err, ctx.Err())
		}
	case <-ctx.Done():
		t.Fatal("pipe drain did not finish within parent budget")
	}
}
