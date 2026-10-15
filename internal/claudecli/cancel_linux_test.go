//go:build linux

package claudecli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
)

func TestRunCancelsActiveChildProcess(t *testing.T) {
	binary := fakeExecutable(t, "claude", `sleep 30 &
printf '%s\n' "$!" > "$0.pid"
wait
`)
	workspace := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, workspace, "task", false, "", config.ClaudeConfig{})
		done <- err
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
		case err := <-done:
			t.Fatalf("run exited before child started: %v", err)
		case <-deadline.C:
			t.Fatal("child process did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want cancellation", err)
		}
	case <-deadline.C:
		t.Fatal("cancellation did not finish the run")
	}
	for {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(err, os.ErrNotExist) {
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
