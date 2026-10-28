//go:build !windows

package action

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestAuditValidationUnavailableOnSignaledValidator(t *testing.T) {
	for _, test := range []struct {
		name, script string
		unavailable  bool
	}{
		{"signaled", "kill -TERM $$", true},
		{"invalid report", "exit 1", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := exec.CommandContext(t.Context(), "sh", "-c", test.script).Run()
			if err == nil || auditValidationUnavailable(err) != test.unavailable {
				t.Fatalf("validator error=%v, unavailable=%t", err, auditValidationUnavailable(err))
			}
		})
	}
}

func TestCanceledAuditValidationRetainsIncompleteInProcess(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	bin := t.TempDir()
	startedPath, holdPath := filepath.Join(bin, "started"), filepath.Join(bin, "hold")
	for _, path := range []string{startedPath, holdPath} {
		if err := syscall.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	started, err := os.OpenFile(startedPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = started.Close() })
	fakeOnPath(t, "node", "#!/bin/sh\nif [ \"$2\" = -e ]; then exit 0; fi\nprintf started > "+shellQuote(startedPath)+"\nexec /bin/cat < "+shellQuote(holdPath)+"\n")
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	root := t.TempDir()
	e := &Orchestrator{WorkRoot: root}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.preparePublication(ctx, &preparedAction{job: job}, result)
		done <- err
	}()
	marker := make(chan error, 1)
	go func() {
		var signal [7]byte
		_, err := io.ReadFull(started, signal[:])
		if err == nil && string(signal[:]) != "started" {
			err = errors.New("unexpected validator marker")
		}
		marker <- err
	}()
	select {
	case err := <-marker:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("validator did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errors.Is(err, monitor.ErrTerminalJob) {
			t.Fatalf("canceled validation was not preserved: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canceled validator did not stop")
	}
	dirs, err := filepath.Glob(filepath.Join(root, "audits", "incomplete-*"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("incomplete audit missing after cancellation: dirs=%v error=%v", dirs, err)
	}
}
