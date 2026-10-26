//go:build linux

package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestLocalWorkerRunnerCleansBeforeReuse(t *testing.T) {
	for _, mode := range []string{"success", "model-failure"} {
		t.Run(mode, func(t *testing.T) {
			installActionTestSkills(t, testReviewSkillURL)
			w, workRoot, report := startRunnerCleanupWorker(t, mode)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := w.WaitCapacity(ctx); err != nil {
				t.Fatal(err)
			}
			first := runnerCleanupJob("first")
			result, err := w.Execute(ctx, first, runnerExecutionCheckpoints{})
			if !result.matchesJob(first) || mode == "success" && (err != nil || result.Answer != proposalJSON("worker answer", "")) || mode == "model-failure" && (err == nil || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || result.Answer != "") {
				t.Fatalf("first result=%+v error=%v", result, err)
			}
			assertRunnerCleanupRoot(t, workRoot)
			if err := w.WaitCapacity(ctx); err != nil {
				t.Fatalf("clean attempt could not reuse worker: %v", err)
			}
			second := runnerCleanupJob("second")
			result, err = w.Execute(ctx, second, runnerExecutionCheckpoints{})
			if err != nil || !result.matchesJob(second) || result.Answer != proposalJSON("worker answer", "") {
				t.Fatalf("second result=%+v error=%v", result, err)
			}
			assertRunnerCleanupRoot(t, workRoot)
			if got := unixWorkerReport(t, ctx, report, ".done"); got != strconv.Itoa(w.cmd.Process.Pid) {
				t.Fatalf("reused worker PID=%q, want %d", got, w.cmd.Process.Pid)
			}
		})
	}
}

func TestLocalWorkerRunnerCleanupOnCancellation(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	w, workRoot, report := startRunnerCleanupWorker(t, "cancel")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	actionCtx, stopAction := context.WithCancel(ctx)
	done := make(chan httpServerTestResult, 1)
	go func() {
		result, err := w.Execute(actionCtx, runnerCleanupJob("canceled"), runnerExecutionCheckpoints{})
		done <- httpServerTestResult{result, err}
	}()
	unixWorkerReport(t, ctx, report, ".entered")
	stopAction()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled assignment error=%v, result=%+v", got.err, got.result)
		}
	case <-ctx.Done():
		t.Fatalf("canceled worker did not stop: %v", ctx.Err())
	}
	assertRunnerCleanupRoot(t, workRoot)
	if w.reuse.Load() != workerRetired || w.cmd.ProcessState == nil {
		t.Fatalf("canceled worker retained capacity or process: state=%d process=%v", w.reuse.Load(), w.cmd.ProcessState)
	}
}

func TestLocalWorkerRunnerCrashRetainsRecoveryEvidence(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	w, workRoot, report := startRunnerCleanupWorker(t, "crash")
	w.executor.outcomeGrace = 25 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	job := runnerCleanupJob("crashed")
	result, runErr := w.Execute(ctx, job, runnerExecutionCheckpoints{})
	if runErr == nil || !errors.Is(runErr, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(runErr, errLocalWorkerCleanupUnverified) || !result.matchesJob(job) {
		t.Fatalf("crash result=%+v error=%v", result, runErr)
	}
	unixWorkerReport(t, ctx, report, ".entered")
	entries, err := os.ReadDir(workRoot)
	if err != nil {
		t.Fatal(err)
	}
	orphan := ""
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "minion-") {
			orphan = entry.Name()
		}
	}
	if orphan == "" {
		t.Fatalf("crash erased attempted workspace: entries=%v", entries)
	}
	owner := Orchestrator{WorkRoot: workRoot}
	if err := owner.retainWorkerDiagnostics(job, result, runErr); err != nil {
		t.Fatalf("record unverified cleanup scope: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workRoot, "worker-diagnostics", job.Identity.AttemptID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var record retainedWorkerDiagnostics
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := workspace.CanonicalPath(workRoot)
	if err != nil {
		t.Fatal(err)
	}
	if record.CleanupUnverifiedWorkRoot != canonicalRoot || record.WorkerReportedFailure != nil || len(record.WorkerReportedDiagnostics) != 0 || strings.Contains(string(data), orphan) {
		t.Fatalf("crash recovery record claimed unobserved worker facts: %+v", record)
	}
	if w.reuse.Load() != workerRetired || w.cmd.ProcessState == nil {
		t.Fatalf("crashed worker retained capacity or process: state=%d process=%v", w.reuse.Load(), w.cmd.ProcessState)
	}
}

func startRunnerCleanupWorker(t *testing.T, mode string) (*localWorker, string, string) {
	t.Helper()
	workRoot := t.TempDir()
	report := filepath.Join(t.TempDir(), "runner")
	if err := workspace.WriteAtomic(filepath.Join(workRoot, "unrelated"), []byte("keep")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	w, err := startLocalWorker(unixTestDirectory(t), "local", os.Args[0], []string{"-test.run=^TestLocalWorkerRunnerCleanupHelper$"}, 10*time.Second, WorkerCredentials{}, func(cmd *exec.Cmd, socket string) error {
		cmd.Env = append(os.Environ(), "MINION_RUNNER_CLEANUP_HELPER="+mode, "MINION_RUNNER_CLEANUP_SOCKET="+socket, "MINION_RUNNER_CLEANUP_WORKROOT="+workRoot, "MINION_RUNNER_CLEANUP_REPORT="+report)
		cmd.Stdout = &output
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil && mode != "crash" {
			t.Errorf("close cleanup worker: %v", err)
		}
		if t.Failed() {
			t.Logf("cleanup worker output: %s", monitor.InertLogText(output.String()))
		}
	})
	return w, workRoot, report
}

func assertRunnerCleanupRoot(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "unrelated" {
		t.Fatalf("attempt-owned workspace remains: entries=%v error=%v", entries, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "unrelated")); err != nil || string(data) != "keep" {
		t.Fatalf("unrelated workspace changed: data=%q error=%v", data, err)
	}
}

func runnerCleanupJob(id string) Job {
	job := testJob(command.Comment)
	job.Identity.JobID, job.Identity.AttemptID = id, id
	job.Main.Skills = []string{testReviewSkillURL}
	return job
}

func TestLocalWorkerRunnerCleanupHelper(t *testing.T) {
	mode := os.Getenv("MINION_RUNNER_CLEANUP_HELPER")
	if mode == "" {
		return
	}
	report := os.Getenv("MINION_RUNNER_CLEANUP_REPORT")
	defer func() {
		if t.Failed() {
			_ = workspace.WriteAtomic(report+".failed", []byte("helper failed"))
		}
	}()
	client, err := NewUnixExecutionClient(os.Getenv("MINION_RUNNER_CLEANUP_SOCKET"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	calls := 0
	runner := &Runner{WorkRoot: os.Getenv("MINION_RUNNER_CLEANUP_WORKROOT"), runModel: func(ctx context.Context, _ modelRun) (aicli.Result, error) {
		calls++
		if mode == "crash" || mode == "cancel" {
			if err := workspace.WriteAtomic(report+".entered", []byte(strconv.Itoa(os.Getpid()))); err != nil {
				return aicli.Result{}, err
			}
			if mode == "crash" {
				os.Exit(7)
			}
			<-ctx.Done()
			return aicli.Result{}, ctx.Err()
		}
		if mode == "model-failure" && calls == 1 {
			return aicli.Result{}, errors.New("fixture model failure")
		}
		return aicli.Result{Answer: proposalJSON("worker answer", "")}, nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	count := 2
	if mode == "crash" || mode == "cancel" {
		count = 1
	}
	for i := range count {
		err := RunHTTP(ctx, client, UnixExecutionBaseURL, "local", runner)
		if mode == "model-failure" && i == 0 {
			if err == nil || !strings.Contains(err.Error(), "fixture model failure") {
				t.Fatalf("first run error=%v", err)
			}
		} else if mode == "cancel" {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled run error=%v", err)
			}
		} else if err != nil {
			t.Fatalf("run %d error=%v", i+1, err)
		}
	}
	if mode == "success" || mode == "model-failure" {
		if err := workspace.WriteAtomic(report+".done", []byte(strconv.Itoa(os.Getpid()))); err != nil {
			t.Fatal(err)
		}
		if err := RunHTTP(ctx, client, UnixExecutionBaseURL, "local", runner); !errors.Is(err, context.Canceled) {
			t.Fatalf("idle worker stop error=%v", err)
		}
	}
	if mode == "cancel" && calls != 1 {
		t.Fatalf("canceled model calls=%d", calls)
	}
}
