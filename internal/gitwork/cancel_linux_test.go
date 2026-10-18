//go:build linux

package gitwork

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/testsupport"
)

func assertGitChildStopped(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("Git child did not start: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid Git child PID %q: %v", data, err)
	}
	stat, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stat.Close(); err != nil {
			t.Error(err)
		}
	}()
	// SIGKILL delivery precedes zombie visibility. Pin the proc entry so a
	// recycled numeric PID cannot redirect later observations to another task.
	waitCtx, stopWait := context.WithTimeout(context.Background(), time.Second)
	defer stopWait()
	if err := testsupport.WaitStopped(waitCtx, stat); err != nil {
		t.Fatalf("Git child is still running: pid=%d: %v", pid, err)
	}
}

func TestGitOutputLimitStopsChildHoldingPipes(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	alias := "!printf '%s' $$ > " + shellQuote(pidFile) + "; printf overflow-output; exec sleep 6"
	started := time.Now()
	_, _, err := (&Checkout{path: dir}).boundedGit(context.Background(), nil, 4, false, "-c", "alias.probe="+alias, "probe")
	elapsed := time.Since(started)
	assertGitChildStopped(t, pidFile)
	if elapsed > 3*time.Second || !errors.Is(err, ErrCheckoutTooLarge) || errors.Is(err, context.Canceled) {
		t.Fatalf("limited Git took %s: %v", elapsed, err)
	}
}

func TestGitDeadlineStopsLazyFetchChildHoldingPipes(t *testing.T) {
	t.Setenv("EMAIL", "probe@example.invalid")
	fixture := newFixture(t)
	root := t.TempDir()
	server := filepath.Join(root, "server.git")
	fixture.git("", "clone", "--bare", fixture.remote, server)
	fixture.git(server, "config", "uploadpack.allowFilter", "true")
	fixture.git(server, "config", "uploadpack.allowAnySHA1InWant", "true")
	clone := filepath.Join(root, "clone")
	fixture.git("", "clone", "--filter=blob:none", "--no-checkout", "--branch", "main", "file://"+server, clone)
	blob := fixture.git(clone, "rev-parse", "HEAD:README.md")
	if missing := fixture.git(clone, "rev-list", "--objects", "--missing=print", "HEAD"); !strings.Contains(missing, "?"+blob) {
		t.Fatalf("fixture blob was not filtered: %s", missing)
	}
	pidFile := filepath.Join(root, "fetch.pid")
	stall := filepath.Join(root, "upload-pack")
	if err := os.WriteFile(stall, []byte("#!/bin/sh\nprintf '%s' $$ > "+shellQuote(pidFile)+"\nprintf 'deadline diagnostic\\nforged journal\\n' >&2\nexec sleep 6\n"), 0700); err != nil {
		t.Fatal(err)
	}
	fixture.git(clone, "config", "remote.origin.uploadpack", stall)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err := (&Checkout{path: clone}).boundedGit(ctx, strings.NewReader(blob+"\n"), 1024, false, "cat-file", "--batch")
	elapsed := time.Since(started)
	assertGitChildStopped(t, pidFile)
	var exit *exec.ExitError
	if elapsed > 3*time.Second || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &exit) || errors.Is(err, ErrUnsupportedSource) || !strings.Contains(err.Error(), `deadline diagnostic\nforged journal`) {
		t.Fatalf("deadline Git took %s: %v", elapsed, err)
	}
}
