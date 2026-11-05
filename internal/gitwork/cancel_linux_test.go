//go:build linux

package gitwork

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/testsupport"
)

func TestGitOutputLimitStopsDirectProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "git.pid")
	fakeOnPath(t, "git", "#!/bin/sh\nprintf '%s' $$ > "+shellQuote(pidFile)+"\nprintf overflow-output\nexec sleep 30\n")
	started := time.Now()
	_, _, err := (&Checkout{path: dir}).boundedGit(t.Context(), nil, 4, false, "status")
	if time.Since(started) > 3*time.Second || !errors.Is(err, ErrCheckoutTooLarge) || errors.Is(err, context.Canceled) {
		t.Fatalf("limited Git took %s: %v", time.Since(started), err)
	}
	assertGitProcessStopped(t, pidFile)
}

func TestGitDeadlineStopsDirectProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "git.pid")
	fakeOnPath(t, "git", "#!/bin/sh\nprintf '%s' $$ > "+shellQuote(pidFile)+"\nprintf 'deadline diagnostic\\nforged journal\\n' >&2\nexec sleep 30\n")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, _, err := (&Checkout{path: dir}).boundedGit(ctx, nil, 1024, false, "status")
	var exit *exec.ExitError
	if time.Since(started) > 5*time.Second || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &exit) || errors.Is(err, ErrUnsupportedSource) || !strings.Contains(err.Error(), "deadline diagnostic\\nforged journal") {
		t.Fatalf("deadline Git took %s: %v", time.Since(started), err)
	}
	assertGitProcessStopped(t, pidFile)
}

func TestGitTruncationRejectsPipeDrainOverrun(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "holder.pid")
	stopFile := pidFile + ".stop"
	testsupport.StopOnCleanup(t, stopFile)
	script := "#!/bin/sh\n(sleep 1; printf overflow-output; while [ ! -f " + shellQuote(stopFile) + " ]; do sleep .05; done) &\nprintf '%s' $! > " + shellQuote(pidFile) + "\nexit 0\n"
	fakeOnPath(t, "git", script)
	output, truncated, err := (&Checkout{path: dir}).boundedGit(t.Context(), nil, 4, true, "status")
	process, pinErr := testsupport.PinProcess(pidFile)
	if pinErr != nil {
		t.Fatalf("pin finite pipe holder: %v", pinErr)
	}
	defer process.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		if err := process.Cleanup(ctx); err != nil {
			t.Errorf("clean finite pipe holder: %v", err)
		}
	}()
	if len(output) != 0 || truncated || !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("pipe-drain overrun accepted as truncation: output=%q truncated=%t error=%v", output, truncated, err)
	}
}

func assertGitProcessStopped(t *testing.T, pidFile string) {
	t.Helper()
	process, err := testsupport.PinProcess(pidFile)
	if errors.Is(err, os.ErrProcessDone) {
		return
	}
	if err != nil {
		t.Fatalf("pin Git process: %v", err)
	}
	defer process.Close()
	waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := process.WaitExited(waitCtx); err != nil {
		t.Fatalf("Git process is still running: %v", err)
	}
}
