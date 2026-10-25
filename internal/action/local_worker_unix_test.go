//go:build linux

package action

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func localWorkerTestStart(t *testing.T, mode string) *localWorker {
	t.Helper()
	report := filepath.Join(t.TempDir(), "child")
	var output bytes.Buffer
	w, err := startLocalWorker(unixTestDirectory(t), "local", func(ctx context.Context, socket string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLocalWorkerHelper$")
		cmd.Env = append(os.Environ(), "MINION_LOCAL_WORKER_HELPER="+mode, "MINION_LOCAL_WORKER_SOCKET="+socket, "MINION_LOCAL_WORKER_REPORT="+report)
		cmd.Stdout, cmd.Stderr = &output, &output
		return cmd
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil && mode != "hold" && mode != "known-hold" && mode != "known" && mode != "crash" && mode != "precrash" && mode != "drain-kill" && mode != "stop-exit" {
			t.Errorf("close local worker: %v", err)
		}
		if t.Failed() {
			t.Logf("local worker output: %s", output.String())
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if got := unixWorkerReport(t, ctx, report, ".ready"); got != strconv.Itoa(w.cmd.Process.Pid) {
		t.Fatalf("direct child PID = %q, want %d", got, w.cmd.Process.Pid)
	}
	return w
}

func TestLocalWorkerKnownOutcomeSurvivesChildExit(t *testing.T) {
	w := localWorkerTestStart(t, "known")
	job := testJob(command.Comment)
	result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{})
	if err != nil || result.Answer != "known" || !result.matchesJob(job) {
		t.Fatalf("accepted outcome changed after worker exit: result=%+v error=%v", result, err)
	}
	select {
	case <-w.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("direct child was not reaped")
	}
	if w.waitErr == nil || w.cmd.ProcessState == nil {
		t.Fatalf("nonzero direct child exit was not observed: wait=%v state=%v", w.waitErr, w.cmd.ProcessState)
	}
	var exitErr *exec.ExitError
	if err := w.Close(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 || !strings.Contains(err.Error(), "local worker exited") {
		t.Fatalf("spontaneous exit was not reported separately: %v", err)
	}
	if _, err := os.Lstat(w.endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned endpoint remains after child exit: %v", err)
	}
}

func TestLocalWorkerCloseReportsExitCodeAfterInterrupt(t *testing.T) {
	w := localWorkerTestStart(t, "stop-exit")
	var exitErr *exec.ExitError
	if err := w.Close(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("owner cancellation hid child exit code: %v", err)
	}
	if w.cmd.ProcessState == nil {
		t.Fatal("direct child was not reaped")
	}
}

func TestLocalWorkerCloseAcceptsGracefulInterrupt(t *testing.T) {
	w := localWorkerTestStart(t, "stop-success")
	if err := w.Close(); err != nil {
		t.Fatalf("graceful owner cancellation reported as child failure: %v", err)
	}
}

func TestLocalWorkerRejectedAfterCloseHasNoMissingOutcome(t *testing.T) {
	w := localWorkerTestStart(t, "hold")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	job := testJob(command.Comment)
	result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{})
	if !result.matchesJob(job) || err == nil || !strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "stopped before a final outcome") || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("rejected assignment claimed a missing outcome: result=%+v error=%v", result, err)
	}
}

func TestLocalWorkerCrashAfterDeliveryQuarantines(t *testing.T) {
	w := localWorkerTestStart(t, "crash")
	w.executor.outcomeGrace = 25 * time.Millisecond
	job := testJob(command.Comment)
	result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{})
	if !result.matchesJob(job) || !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !strings.Contains(err.Error(), "local worker exited") {
		t.Fatalf("missing final outcome was not quarantined: result=%+v error=%v", result, err)
	}
	w.executor.mu.Lock()
	quarantined := w.executor.unavailable
	w.executor.mu.Unlock()
	if !quarantined || w.cmd.ProcessState == nil {
		t.Fatalf("crashed worker remains available or unjoined: quarantine=%v state=%v", quarantined, w.cmd.ProcessState)
	}
	if _, err := os.Lstat(w.endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash left owned endpoint: %v", err)
	}
}

func TestLocalWorkerCrashBeforeDeliveryReportsExit(t *testing.T) {
	w := localWorkerTestStart(t, "precrash")
	job := testJob(command.Comment)
	result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{})
	if !result.matchesJob(job) || err == nil || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !strings.Contains(err.Error(), "local worker exited") {
		t.Fatalf("pre-delivery exit lost its diagnostic or became uncertain: result=%+v error=%v", result, err)
	}
	if w.cmd.ProcessState == nil {
		t.Fatal("pre-delivery child was not reaped")
	}
}

func TestLocalWorkerBudgetQuarantineStopsLiveChild(t *testing.T) {
	w := localWorkerTestStart(t, "stall")
	w.executor.outcomeGrace = 25 * time.Millisecond
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		w.executor.mu.Lock()
		held := w.executor.pull
		w.executor.mu.Unlock()
		if held {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("child never held a pull")
		case <-time.After(time.Millisecond):
		}
	}
	job := testJob(command.Comment)
	job.RuntimeBudget = time.Second
	result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{})
	if t.Context().Err() != nil || !result.matchesJob(job) || !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("live caller lost uncertain budget outcome: result=%+v error=%v caller=%v", result, err, t.Context().Err())
	}
	w.executor.mu.Lock()
	closed, unavailable := w.executor.closed, w.executor.unavailable
	w.executor.mu.Unlock()
	if !closed || !unavailable || w.cmd.ProcessState == nil {
		t.Fatalf("budget quarantine left child available: closed=%v unavailable=%v state=%v", closed, unavailable, w.cmd.ProcessState)
	}
	if _, err := os.Lstat(w.endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("budget quarantine left owned endpoint: %v", err)
	}
}

func TestLocalWorkerBudgetBeforeDeliveryStopsLiveChild(t *testing.T) {
	w := localWorkerTestStart(t, "hold")
	job := testJob(command.Comment)
	job.RuntimeBudget = 100 * time.Millisecond
	result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{})
	if t.Context().Err() != nil || !result.matchesJob(job) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("undelivered budget expiry became uncertain or lost: result=%+v error=%v caller=%v", result, err, t.Context().Err())
	}
	w.executor.mu.Lock()
	closed := w.executor.closed
	w.executor.mu.Unlock()
	if !closed || w.cmd.ProcessState == nil {
		t.Fatalf("undelivered budget expiry left child live: closed=%v state=%v", closed, w.cmd.ProcessState)
	}
	if _, err := os.Lstat(w.endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("undelivered budget expiry left owned endpoint: %v", err)
	}
}

func TestLocalWorkerAcceptedQuarantineStopsLiveChild(t *testing.T) {
	w := localWorkerTestStart(t, "gap")
	job := testJob(command.Comment)
	result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{})
	if t.Context().Err() != nil || !result.matchesJob(job) || result.Answer != "known" || !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("accepted uncertain outcome changed: result=%+v error=%v caller=%v", result, err, t.Context().Err())
	}
	w.executor.mu.Lock()
	closed, unavailable := w.executor.closed, w.executor.unavailable
	w.executor.mu.Unlock()
	if !closed || !unavailable || w.cmd.ProcessState == nil {
		t.Fatalf("accepted quarantine left child available: closed=%v unavailable=%v state=%v", closed, unavailable, w.cmd.ProcessState)
	}
	if _, err := os.Lstat(w.endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("accepted quarantine left owned endpoint: %v", err)
	}
}

func TestLocalWorkerDrainsAcceptedCallbackBeforeStop(t *testing.T) {
	for _, stop := range []string{"shutdown", "cancellation"} {
		t.Run(stop, func(t *testing.T) {
			w := localWorkerTestStart(t, "drain")
			job := testJob(command.Comment)
			entered, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan httpServerTestResult, 1)
			go func() {
				result, err := w.Execute(ctx, job, runnerExecutionCheckpoints{before: func(CallCheckpoint) error {
					close(entered)
					<-release
					return nil
				}})
				done <- httpServerTestResult{result, err}
			}()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("child never entered the accepted callback")
			}
			busy := job
			busy.Identity.JobID = "other"
			if result, err := w.Execute(t.Context(), busy, runnerExecutionCheckpoints{}); err == nil || !strings.Contains(err.Error(), "busy") || !result.matchesJob(busy) {
				t.Fatalf("second assignment was not rejected: result=%+v error=%v", result, err)
			}
			busyCtx, cancelBusy := context.WithCancel(t.Context())
			cancelBusy()
			if _, err := w.Execute(busyCtx, busy, runnerExecutionCheckpoints{}); err == nil || !strings.Contains(err.Error(), "busy") {
				t.Fatalf("canceled second assignment was not rejected as busy: %v", err)
			}
			w.executor.mu.Lock()
			closedEarly := w.executor.closed
			w.executor.mu.Unlock()
			if closedEarly {
				t.Fatal("canceled busy request stopped the active child")
			}
			closed := make(chan error, 1)
			if stop == "shutdown" {
				go func() { closed <- w.Close() }()
			} else {
				cancel()
			}
			tx := w.executor.transaction()
			if tx == nil {
				t.Fatal("accepted callback lost its transaction")
			}
			select {
			case <-tx.budget.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("stop did not reach the transaction")
			}
			select {
			case err := <-closed:
				t.Fatalf("close returned before callback drained: %v", err)
			default:
			}
			select {
			case <-w.exited:
				t.Fatal("direct child was reaped before callback drain")
			default:
			}
			if _, err := os.Lstat(w.endpoint.SocketPath()); err != nil {
				t.Fatalf("endpoint closed before callback drain: %v", err)
			}
			close(release)
			select {
			case got := <-done:
				if !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || got.result.Answer != "known" {
					t.Fatalf("accepted canceled outcome lost: result=%+v error=%v", got.result, got.err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("accepted callback did not finish")
			}
			if stop == "shutdown" {
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("shutdown did not reap child")
				}
			}
			if w.cmd.ProcessState == nil {
				t.Fatal("direct child was not reaped")
			}
			if _, err := os.Lstat(w.endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stop left owned endpoint: %v", err)
			}
		})
	}
}

func TestLocalWorkerCloseReportsExitDuringCallbackDrain(t *testing.T) {
	w := localWorkerTestStart(t, "drain-kill")
	w.executor.outcomeGrace = 25 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	done := make(chan httpServerTestResult, 1)
	go func() {
		result, err := w.Execute(t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{before: func(CallCheckpoint) error {
			close(entered)
			<-release
			return nil
		}})
		done <- httpServerTestResult{result, err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("child did not enter accepted callback")
	}
	tx := w.executor.transaction()
	if tx == nil {
		t.Fatal("accepted callback lost its transaction")
	}
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	select {
	case <-tx.budget.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not begin callback drain")
	}
	if err := w.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("killed direct child was not reaped")
	}
	if _, err := os.Lstat(w.endpoint.SocketPath()); err != nil {
		t.Fatalf("endpoint closed before entered callback drained: %v", err)
	}
	close(release)
	select {
	case err := <-closed:
		if w.waitErr == nil || !errors.Is(err, w.waitErr) || w.cmd.ProcessState == nil {
			t.Fatalf("spontaneous exit during drain was lost: close=%v wait=%v state=%v", err, w.waitErr, w.cmd.ProcessState)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not finish after callback release")
	}
	select {
	case got := <-done:
		if !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("missing outcome was not quarantined: result=%+v error=%v", got.result, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not finish after child exit")
	}
	if _, err := os.Lstat(w.endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Close left owned endpoint: %v", err)
	}
}

func TestLocalWorkerClosePreservesReplacedEndpoint(t *testing.T) {
	w := localWorkerTestStart(t, "hold")
	path := w.endpoint.SocketPath()
	moved := path + ".moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Fatalf("replacement was not reported: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "replacement" || w.cmd.ProcessState == nil {
		t.Fatalf("replacement changed or child not reaped: data=%q error=%v state=%v", data, err, w.cmd.ProcessState)
	}
}

func TestLocalWorkerKnownOutcomeKeepsEndpointCloseErrorSeparate(t *testing.T) {
	w := localWorkerTestStart(t, "known-hold")
	job := testJob(command.Comment)
	result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{})
	if err != nil || result.Answer != "known" || !result.matchesJob(job) {
		t.Fatalf("accepted outcome changed: result=%+v error=%v", result, err)
	}
	path := w.endpoint.SocketPath()
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("endpoint replacement was not reported by Close")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "replacement" || w.cmd.ProcessState == nil {
		t.Fatalf("replacement changed or child not reaped: data=%q error=%v state=%v", data, err, w.cmd.ProcessState)
	}
}

func TestLocalWorkerListenerFailureReapsChild(t *testing.T) {
	w := localWorkerTestStart(t, "hold")
	if err := w.endpoint.listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("listener failure did not stop direct child")
	}
	if err := w.Close(); err == nil {
		t.Fatal("listener failure was not reported")
	}
	if _, err := os.Lstat(w.endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listener failure left owned endpoint: %v", err)
	}
}

func TestLocalWorkerStartFailureClosesEndpoint(t *testing.T) {
	dir := unixTestDirectory(t)
	w, err := startLocalWorker(dir, "local", func(ctx context.Context, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, filepath.Join(dir, "missing"))
	})
	if w != nil || err == nil || !strings.Contains(err.Error(), "start local worker") {
		t.Fatalf("start failure: worker=%v error=%v", w, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "execution.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("start failure retained owned endpoint: %v", err)
	}
}

func TestLocalWorkerHelper(t *testing.T) {
	mode := os.Getenv("MINION_LOCAL_WORKER_HELPER")
	if mode == "" {
		return
	}
	client, err := NewUnixExecutionClient(os.Getenv("MINION_LOCAL_WORKER_SOCKET"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	report := os.Getenv("MINION_LOCAL_WORKER_REPORT")
	var interrupt chan os.Signal
	if mode == "stop-exit" || mode == "stop-success" {
		interrupt = make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		defer signal.Stop(interrupt)
	}
	if err := workspace.WriteAtomic(report+".ready", []byte(strconv.Itoa(os.Getpid()))); err != nil {
		t.Fatal(err)
	}
	if mode == "stop-exit" || mode == "stop-success" {
		<-interrupt
		if mode == "stop-exit" {
			os.Exit(7)
		}
		return
	}
	if mode == "precrash" {
		os.Exit(7)
	}
	if mode == "hold" {
		select {}
	}
	var assignment httpAssignment
	status, err := unixTestJSON(t, client, "/next", httpPull{Version: ProtocolVersion, ExecutorID: "local"}, &assignment, httpJobBytes)
	if err != nil || status != http.StatusOK {
		t.Fatalf("pull: status=%d error=%v", status, err)
	}
	if mode == "crash" {
		os.Exit(7)
	}
	if mode == "stall" {
		select {}
	}
	job := assignment.Job
	if mode == "drain" || mode == "drain-kill" {
		call := CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1, Model: job.Main.Model, Effort: job.Main.Effort}
		unixTestAck(t, client, "/before", httpCall{Sequence: 1, Checkpoint: CallCheckpoint{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Call: call}}, 1)
	}
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "known"}
	sequence := uint64(1)
	var failure *httpFailure
	if mode == "drain" || mode == "drain-kill" || mode == "gap" {
		sequence, failure = 2, newHTTPFailure(context.Canceled)
	}
	unixTestAck(t, client, "/outcome", httpOutcome{Sequence: sequence, Result: result, Failure: failure}, sequence)
	if mode == "known" {
		os.Exit(7)
	}
	if mode != "drain" && mode != "drain-kill" && mode != "known-hold" && mode != "gap" {
		t.Fatalf("unexpected helper mode %q", mode)
	}
	// The owner must stop the child only after the accepted outcome drains.
	select {}
}
