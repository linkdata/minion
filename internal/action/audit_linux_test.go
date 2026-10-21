//go:build linux

package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/testsupport"
)

func TestAuditPermissionProbeDoesNotRetryAfterDescendants(t *testing.T) {
	if err := processgroup.Check(); err != nil {
		t.Skipf("delegated process ownership unavailable: %v", err)
	}
	dir := t.TempDir()
	calls, pidFile := filepath.Join(dir, "calls"), filepath.Join(dir, "child.pid")
	testsupport.StopOnCleanup(t, pidFile+".stop")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> " + shellQuote(calls) + "\n" +
		"(while [ ! -f " + shellQuote(pidFile+".stop") + " ]; do sleep 1; done) </dev/null >/dev/null 2>&1 &\n" +
		"printf '%s' \"$!\" > " + shellQuote(pidFile) + "\nexit 0\n"
	bin := fakeOnPath(t, "node", script)
	flag, err := auditNodePermissionFlag(context.Background(), bin, filepath.Join(bin, "node"))
	if flag != "" || !errors.Is(err, processgroup.ErrDescendants) {
		t.Fatalf("permission probe accepted surviving child: flag=%q error=%v", flag, err)
	}
	if calls, readErr := os.ReadFile(calls); readErr != nil || string(calls) != "--permission\n" {
		t.Fatalf("permission probe retried after ownership failure: calls=%q error=%v", calls, readErr)
	}
	child, err := testsupport.PinProcess(pidFile)
	if errors.Is(err, os.ErrProcessDone) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := child.WaitExited(waitCtx); err != nil {
		t.Fatalf("audit probe child survived: %v", err)
	}
}

func TestAuditPermissionProbeDoesNotRetryAfterExhaustion(t *testing.T) {
	if err := processgroup.Check(); err != nil {
		t.Skipf("delegated process ownership unavailable: %v", err)
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	bin := fakeOnPath(t, "node", "#!/bin/sh\nprintf '%s\\n' \"$1\" >> "+shellQuote(calls)+"\n/bin/true\nexit 1\n")
	if err := processgroup.Configure(1, processgroup.DefaultMaxMemoryMiB<<20); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = processgroup.Configure(processgroup.DefaultMaxTasks, processgroup.DefaultMaxMemoryMiB<<20) })
	flag, err := auditNodePermissionFlag(context.Background(), bin, filepath.Join(bin, "node"))
	if flag != "" || !errors.Is(err, processgroup.ErrExhausted) {
		t.Fatalf("permission probe hid process exhaustion: flag=%q error=%v", flag, err)
	}
	if calls, readErr := os.ReadFile(calls); readErr != nil || string(calls) != "--permission\n" {
		t.Fatalf("permission probe retried after exhaustion: calls=%q error=%v", calls, readErr)
	}
}
