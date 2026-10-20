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

func assertGitChildStopped(t *testing.T, pidFile string) {
	t.Helper()
	child, err := testsupport.PinProcess(pidFile)
	if errors.Is(err, os.ErrProcessDone) {
		return
	}
	if err != nil {
		t.Fatalf("pin Git child: %v", err)
	}
	defer func() {
		if err := child.Close(); err != nil {
			t.Error(err)
		}
	}()
	// SIGKILL delivery precedes zombie visibility; keep observing the pinned child.
	waitCtx, stopWait := context.WithTimeout(context.Background(), time.Second)
	defer stopWait()
	if err := child.WaitExited(waitCtx); err != nil {
		t.Fatalf("Git child is still running: %v", err)
	}
}

func TestGitOutputLimitStopsChildHoldingPipes(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	testsupport.StopOnCleanup(t, pidFile+".stop")
	alias := "!printf '%s' $$ > " + shellQuote(pidFile+".tmp") + " && mv " + shellQuote(pidFile+".tmp") + " " + shellQuote(pidFile) + "; printf overflow-output; i=0; while [ -f " + shellQuote(pidFile) + " ] && [ \"$i\" -lt 6 ] && [ ! -f " + shellQuote(pidFile+".stop") + " ]; do sleep 1; i=$((i+1)); done"
	started := time.Now()
	_, _, err := (&Checkout{path: dir}).boundedGit(context.Background(), nil, 4, false, "-c", "alias.probe="+alias, "probe")
	elapsed := time.Since(started)
	assertGitChildStopped(t, pidFile)
	if elapsed > 3*time.Second || !errors.Is(err, ErrCheckoutTooLarge) || errors.Is(err, context.Canceled) {
		t.Fatalf("limited Git took %s: %v", elapsed, err)
	}
}

func TestGitDeadlineStopsChildHoldingPipes(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	testsupport.StopOnCleanup(t, pidFile+".stop")
	alias := "!printf '%s' $$ > " + shellQuote(pidFile+".tmp") + " && mv " + shellQuote(pidFile+".tmp") + " " + shellQuote(pidFile) + "; printf 'deadline diagnostic\\nforged journal\\n' >&2; i=0; while [ -f " + shellQuote(pidFile) + " ] && [ \"$i\" -lt 6 ] && [ ! -f " + shellQuote(pidFile+".stop") + " ]; do sleep 1; i=$((i+1)); done"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, _, err := (&Checkout{path: dir}).boundedGit(ctx, nil, 1024, false, "-c", "alias.probe="+alias, "probe")
		done <- err
	}()
	waitCtx, stopWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopWait()
	child, err := testsupport.WaitForProcess(waitCtx, pidFile, done)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := child.Cleanup(cleanupCtx); err != nil {
			t.Error(err)
		}
	}()
	select {
	case err = <-done:
	case <-waitCtx.Done():
		t.Fatal("deadline did not finish the Git run")
	}
	elapsed := time.Since(started)
	if err := child.WaitExited(waitCtx); err != nil {
		t.Fatalf("Git child survived deadline cancellation: %v", err)
	}
	var exit *exec.ExitError
	if elapsed > 5*time.Second || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &exit) || errors.Is(err, ErrUnsupportedSource) || !strings.Contains(err.Error(), `deadline diagnostic\nforged journal`) {
		t.Fatalf("deadline Git took %s: %v", elapsed, err)
	}
}
