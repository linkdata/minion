//go:build linux

package codexcli

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
	binary := installFakeCodex(t, `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}' '{"type":"turn.completed","usage":{"input_tokens":15,"cached_input_tokens":7,"output_tokens":11}}'
sleep 5 &
printf '%s\n' "$!" > "$0.pid.tmp"
mv -- "$0.pid.tmp" "$0.pid"
exit 0
`)
	writeUsageRollout(t, os.Getenv("CODEX_HOME"), "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "response", 15, 7, 11), usageEvent("task_complete", "root-turn"))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	type outcome struct {
		result aicli.Result
		err    error
	}
	done := make(chan outcome, 1)
	workspace := t.TempDir()
	go func() {
		result, err := Run(ctx, workspace, "task", false, "", config.CodexConfig{})
		done <- outcome{result, err}
	}()
	child, err := testsupport.WaitForProcess(ctx, binary+".pid", done)
	if err != nil {
		t.Fatal(err)
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
