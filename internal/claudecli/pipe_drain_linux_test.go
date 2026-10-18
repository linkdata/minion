//go:build linux

package claudecli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
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
		until := time.NewTimer(time.Second)
		defer until.Stop()
		observe := time.NewTicker(5 * time.Millisecond)
		defer observe.Stop()
		buffer := make([]byte, 4096)
		for {
			n, err := stat.ReadAt(buffer, 0)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				return
			}
			_, state, ok := strings.Cut(string(buffer[:n]), ") ")
			if (err != nil && !errors.Is(err, io.EOF)) || !ok {
				t.Errorf("read fixture child state: %q, %v", buffer[:n], err)
				return
			}
			if strings.HasPrefix(state, "Z ") {
				return
			} // Exited; the host reaps the orphan.
			select {
			case <-observe.C:
			case <-until.C:
				t.Error("finite fixture child remained live after cleanup")
				return
			}
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
