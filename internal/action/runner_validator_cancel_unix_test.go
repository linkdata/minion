//go:build !windows

package action

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestRunnerCancellationDuringInstalledAuditValidatorPreservesCause(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	bin := t.TempDir()
	startedPath, holdPath := filepath.Join(bin, "started"), filepath.Join(bin, "hold")
	for _, name := range []string{startedPath, holdPath} {
		if err := syscall.Mkfifo(name, 0600); err != nil {
			t.Fatal(err)
		}
	}
	marker, err := os.OpenFile(startedPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := marker.Close(); err != nil {
			t.Error(err)
		}
	}()
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$2\" = -e ]; then exit 0; fi\nprintf started > %s\nexec /bin/cat < %s\n", shellQuote(startedPath), shellQuote(holdPath))
	fakeOnPath(t, "node", script)
	job := testJob(command.SecurityAudit)
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		for _, file := range auditArtifactsFixture(job, request.dir, "Audit report\n") {
			if err := os.WriteFile(filepath.Join(request.outputDir, file.Path), file.Data, 0600); err != nil {
				return aicli.Result{}, err
			}
		}
		return aicli.Result{Answer: "completed model answer"}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan error, 1)
	go func() {
		var signal [7]byte
		_, err := io.ReadFull(marker, signal[:])
		if err == nil && string(signal[:]) != "started" {
			err = fmt.Errorf("unexpected validator marker %q", signal)
		}
		started <- err
	}()
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		defer close(done)
		result, err := runner.Run(ctx, job)
		done <- outcome{result, err}
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("canceled runner did not exit")
		}
	}()
	guard := time.NewTimer(10 * time.Second)
	defer guard.Stop()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case early := <-done:
		t.Fatalf("runner stopped before validator entry: %v", early.err)
	case <-guard.C:
		t.Fatal("validator did not start")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, monitor.ErrTerminalJob) || !strings.Contains(got.err.Error(), "audit validate-findings.cjs failed") || len(got.result.Calls) != 1 || got.result.Answer != "" {
			t.Fatalf("validator cancellation lost cause or publishable boundary: calls=%d answer=%q error=%v", len(got.result.Calls), got.result.Answer, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled validator did not stop")
	}
}
