//go:build linux

package codexcli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
sleep 3 &
printf '%s\n' "$!" > "$0.pid"
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
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var child *os.Process
	var stat *os.File
	for child == nil {
		select {
		case <-ticker.C:
			data, err := os.ReadFile(binary + ".pid")
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || pid <= 0 {
				t.Fatalf("invalid fixture child PID: %q, %v", data, err)
			}
			stat, err = os.Open(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
			if err != nil {
				t.Fatal(err)
			}
			child, err = os.FindProcess(pid)
			if err != nil {
				_ = stat.Close()
				t.Fatal(err)
			}
		case got := <-done:
			t.Fatalf("run exited before the finite fixture child was pinned: %+v, %v", got.result, got.err)
		case <-ctx.Done():
			t.Fatal("fixture child did not start")
		}
	}
	t.Cleanup(func() {
		// Kill only the pinned finite fixture, never a historical process group.
		if err := child.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		defer func() {
			if err := child.Release(); err != nil {
				t.Error(err)
			}
			if err := stat.Close(); err != nil {
				t.Error(err)
			}
		}()
		waitCtx, stopWait := context.WithTimeout(context.Background(), time.Second)
		defer stopWait()
		if err := testsupport.WaitStopped(waitCtx, stat); err != nil {
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
