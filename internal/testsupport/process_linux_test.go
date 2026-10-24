//go:build linux

package testsupport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWaitExitedParsesTheLastCommDelimiter(t *testing.T) {
	for _, test := range []struct {
		name, stat string
		exited     bool
		invalid    bool
	}{
		{name: "zombie with closing parenthesis in comm", stat: "7 (odd) name) Z 1 2", exited: true},
		{name: "comm cannot fake zombie state", stat: "7 (odd) Z label) S 1 2"},
		{name: "stopped is not exited", stat: "7 (worker) T 1 2"},
		{name: "missing delimiter", stat: "malformed", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stat")
			if err := os.WriteFile(path, []byte(test.stat), 0600); err != nil {
				t.Fatal(err)
			}
			pinned, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := pinned.Close(); err != nil {
					t.Error(err)
				}
			}()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err = (&Process{stat: pinned}).WaitExited(ctx)
			switch {
			case test.exited:
				if err != nil {
					t.Fatal(err)
				}
			case test.invalid:
				if err == nil || !strings.Contains(err.Error(), "invalid pinned process state") {
					t.Fatalf("malformed state=%v", err)
				}
			default:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("live process reported exited: %v", err)
				}
			}
		})
	}
}

func TestPinProcessRequiresAValidPIDFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pid")
	if _, err := PinProcess(path); !errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("missing PID marker was mistaken for an exited child: %v", err)
	}
	for _, data := range []string{"", " \t\n"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if child, err := PinProcess(path); child != nil || !errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("unpublished PID %q was mistaken for a child or exit: %v, %v", data, child, err)
		}
	}
	for _, data := range []string{"invalid", "0", "-1"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if child, err := PinProcess(path); child != nil || err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("invalid PID %q was accepted or mistaken for exit: %v, %v", data, child, err)
		}
	}
}

func TestWaitForProcessRetriesUnpublishedPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pid")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	child, err := WaitForProcess(ctx, path, make(chan struct{}))
	if child != nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "fixture child did not start") {
		t.Fatalf("unpublished PID stopped polling before startup deadline: %v, %v", child, err)
	}
}

func TestCleanupDoesNotSignalStalePID(t *testing.T) {
	runCtx, stop := context.WithTimeout(t.Context(), 3*time.Second)
	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", "IFS= read -r value; printf '%s\\n' \"$value\"")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop()
		_ = input.Close()
		_ = cmd.Wait()
	})

	pidFile := filepath.Join(t.TempDir(), "stale.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	child, err := PinProcess(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := child.Cleanup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup of unrelated process: %v", err)
	}
	if _, err := fmt.Fprintln(input, "still alive"); err != nil {
		t.Fatal(err)
	}
	if got, err := bufio.NewReader(output).ReadString('\n'); err != nil || got != "still alive\n" {
		t.Fatalf("unrelated process did not respond after cleanup: %q, %v", got, err)
	}
	if _, err := os.Stat(pidFile + ".stop"); err != nil {
		t.Fatalf("stop marker was not written: %v", err)
	}
}
