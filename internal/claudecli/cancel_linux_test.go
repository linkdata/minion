//go:build linux

package claudecli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunCancelsActiveChildProcess(t *testing.T) {
	binary := fakeExecutable(t, "claude", `printf '%s\n' '`+assistantUsageEvent("partial", 3, 5, 7, 2)+`'
sleep 30 &
printf '%s\n' "$!" > "$0.pid"
wait
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
		result, err := Run(ctx, workspace, "task", false, "", config.ClaudeConfig{})
		done <- outcome{result, err}
	}()
	// The real subprocess must start before cancellation; a synthetic clock
	// cannot establish whether its process group has actually been created.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var pid int
start:
	for {
		select {
		case <-ticker.C:
			if data, err := os.ReadFile(binary + ".pid"); err == nil {
				if pid, err = strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
					break start
				}
			}
		case got := <-done:
			t.Fatalf("run exited before child started: %v", got.err)
		case <-deadline.C:
			t.Fatal("child process did not start")
		}
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.result.Answer != "" || got.result.UsageComplete || got.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 2}) {
			t.Fatalf("canceled Run lost partial usage: result=%+v error=%v", got.result, got.err)
		}
	case <-deadline.C:
		t.Fatal("cancellation did not finish the run")
	}
	for {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			// A process can disappear after ReadFile opens its proc entry.
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(data))
		if len(fields) > 2 && fields[2] == "Z" {
			return // A killed child can await reaping by the container's init.
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("child survived process-group cancellation")
		}
	}
}
