//go:build linux

package claudecli

import (
	"context"
	"errors"
	"os"
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
sleep 3 &
printf '%s\n' "$!" > "$0.pid"
exit 0
`)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	type outcome struct {
		result aicli.Result
		err    error
	}
	done := make(chan outcome, 1)
	workspace := t.TempDir()
	go func() {
		result, err := Run(ctx, workspace, "task", false, "", config.ClaudeConfig{})
		done <- outcome{result, err}
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var child *testsupport.Process
	for child == nil {
		select {
		case <-ticker.C:
			var err error
			child, err = testsupport.PinProcess(binary + ".pid")
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
		case got := <-done:
			t.Fatalf("run exited before the finite fixture child was pinned: %+v, %v", got.result, got.err)
		case <-ctx.Done():
			t.Fatal("fixture child did not start")
		}
	}
	t.Cleanup(func() {
		waitCtx, stopWait := context.WithTimeout(context.Background(), time.Second)
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
