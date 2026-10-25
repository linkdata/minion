//go:build linux

package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

const preflightTestProxyUser = "u%40ser"
const preflightTestProxyPassword = "p%40ss"

func localWorkerTestStart(t *testing.T, mode string) *localWorker {
	t.Helper()
	report := filepath.Join(t.TempDir(), "child")
	var output bytes.Buffer
	w, err := startLocalWorker(unixTestDirectory(t), "local", os.Args[0], []string{"-test.run=^TestLocalWorkerHelper$"}, func(cmd *exec.Cmd, socket string) {
		cmd.Env = append(os.Environ(), "MINION_LOCAL_WORKER_HELPER="+mode, "MINION_LOCAL_WORKER_SOCKET="+socket, "MINION_LOCAL_WORKER_REPORT="+report)
		cmd.Stdout = &output
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil && mode != "hold" && mode != "known-hold" && mode != "known" && mode != "crash" && mode != "precrash" && mode != "preflight-fail" && mode != "drain-kill" && mode != "stop-exit" {
			t.Errorf("close local worker: %v", err)
		}
		if t.Failed() {
			t.Logf("local worker output: %s", monitor.InertLogText(redactActionText(output.String(), w.secrets...)))
			if detail := w.startupDiagnostic(); detail != "" {
				t.Logf("local worker stderr: %s", detail)
			}
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if got := unixWorkerReport(t, ctx, report, ".ready"); got != strconv.Itoa(w.cmd.Process.Pid) {
		t.Fatalf("direct child PID = %q, want %d", got, w.cmd.Process.Pid)
	}
	return w
}

func localWorkerTestPull(t *testing.T, w *localWorker) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		w.executor.mu.Lock()
		held := w.executor.pull
		w.executor.mu.Unlock()
		if held {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("child never held a pull")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestLocalWorkerIdleRunHTTPClose(t *testing.T) {
	w := localWorkerTestStart(t, "run-http-idle")
	localWorkerTestPull(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("idle RunHTTP worker did not stop cleanly: %v", err)
	}
	if w.cmd.ProcessState == nil || !w.cmd.ProcessState.Success() {
		t.Fatalf("idle RunHTTP child did not consume shutdown response: %v", w.cmd.ProcessState)
	}
}

func TestLocalWorkerAwaitHeldPull(t *testing.T) {
	w := localWorkerTestStart(t, "run-http-idle")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := w.awaitHeldPull(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.exited:
		t.Fatal("held pull belonged to an exited child")
	case <-w.endpoint.serveDone:
		t.Fatal("held pull belonged to a stopped endpoint")
	default:
	}
}

func TestLocalWorkerAwaitHeldPullRejectsCanceledRequest(t *testing.T) {
	w := localWorkerTestStart(t, "run-http-idle")
	localWorkerTestPull(t, w)
	canceled := make(chan struct{})
	close(canceled)
	w.executor.mu.Lock()
	w.executor.pullDone = canceled
	w.executor.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := w.awaitHeldPull(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled held request appeared ready: %v", err)
	}
	if w.cmd.ProcessState == nil {
		t.Fatal("canceled pull left the child live")
	}
}

func TestLocalWorkerAwaitHeldPullTracksRealRequestCancellation(t *testing.T) {
	w := localWorkerTestStart(t, "hold")
	client, err := NewUnixExecutionClient(w.endpoint.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	requestCtx, cancelRequest := context.WithCancel(t.Context())
	defer cancelRequest()
	body, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "local"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, UnixExecutionBaseURL+"/next", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	requestDone := make(chan error, 1)
	go func() {
		response, err := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- err
	}()
	localWorkerTestPull(t, w)
	w.executor.mu.Lock()
	pullDone := w.executor.pullDone
	w.executor.mu.Unlock()
	if pullDone == nil {
		t.Fatal("held pull has no request cancellation channel")
	}
	cancelRequest()
	deadline, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	select {
	case <-pullDone:
	case <-deadline.Done():
		t.Fatal("held pull did not track request cancellation")
	}
	select {
	case <-requestDone:
	case <-deadline.Done():
		t.Fatal("canceled pull client did not finish")
	}
	readyCtx, stopReady := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stopReady()
	if err := w.awaitHeldPull(readyCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled real pull appeared ready: %v", err)
	}
	if w.cmd.ProcessState == nil {
		t.Fatal("canceled readiness check left the direct child live")
	}
}

func TestLocalWorkerAwaitHeldPullReportsChildExit(t *testing.T) {
	w := localWorkerTestStart(t, "precrash")
	if err := w.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := w.awaitHeldPull(ctx)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || !strings.Contains(err.Error(), "before holding a pull") {
		t.Fatalf("pre-pull child exit lost its cause: %v", err)
	}
	if w.cmd.ProcessState == nil {
		t.Fatal("pre-pull child was not reaped")
	}
	if _, err := os.Lstat(w.endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-pull exit left the endpoint: %v", err)
	}
}

func TestLocalWorkerAwaitHeldPullReportsBoundedPreflightDiagnostic(t *testing.T) {
	const secret = "native-preflight-secret"
	const ghToken = "gh-credential"
	const pgPassword = "shortpass"
	const proxy = "http://" + preflightTestProxyUser + ":" + preflightTestProxyPassword + "@proxy.example"
	t.Setenv("CODEX_API_KEY", secret)
	t.Setenv("GH_TOKEN", " "+ghToken+" ")
	t.Setenv("PGPASSWORD", pgPassword)
	t.Setenv("HTTPS_PROXY", proxy)
	w := localWorkerTestStart(t, "preflight-fail")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := w.awaitHeldPull(ctx)
	if err == nil || !strings.Contains(err.Error(), "selected Codex CLI unavailable") || !strings.Contains(err.Error(), `\nforged log line`) || strings.Contains(err.Error(), "\nforged log line") || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), ghToken) || strings.Contains(err.Error(), pgPassword) || strings.Contains(err.Error(), proxy) || strings.Contains(err.Error(), preflightTestProxyUser) || strings.Contains(err.Error(), preflightTestProxyPassword) || strings.Contains(err.Error(), "p@ss") || len(err.Error()) > 9<<10 {
		t.Fatalf("preflight diagnostic was lost, unbounded or active: %v", err)
	}
	w.executor.mu.Lock()
	pull := w.executor.pull
	w.executor.mu.Unlock()
	if pull || w.cmd.ProcessState == nil {
		t.Fatalf("failed preflight held a pull or left its child live: pull=%t state=%v", pull, w.cmd.ProcessState)
	}
}

func TestLocalWorkerAwaitHeldPullReportsEndpointFailure(t *testing.T) {
	w := localWorkerTestStart(t, "hold")
	if err := w.endpoint.listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := w.awaitHeldPull(ctx); err == nil || w.endpoint.serveErr == nil || !errors.Is(err, w.endpoint.serveErr) {
		t.Fatalf("pre-pull endpoint failure lost its cause: wait=%v serve=%v", err, w.endpoint.serveErr)
	}
	if w.cmd.ProcessState == nil {
		t.Fatal("endpoint failure left the direct child live")
	}
	if _, err := os.Lstat(w.endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("endpoint failure left the socket: %v", err)
	}
}

func TestLocalWorkerAwaitHeldPullRejectsCancellationAndForgedReady(t *testing.T) {
	for _, test := range []struct {
		name  string
		forge bool
	}{
		{name: "cancellation"},
		{name: "forged ready", forge: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := localWorkerTestStart(t, "hold")
			if test.forge {
				client, err := NewUnixExecutionClient(w.endpoint.SocketPath())
				if err != nil {
					t.Fatal(err)
				}
				defer client.CloseIdleConnections()
				var rejection httpAcknowledgement
				status, err := unixTestJSON(t, client, "/next", httpPull{Version: ProtocolVersion, ExecutorID: "foreign"}, &rejection, httpControlBytes)
				if err != nil || status != http.StatusConflict {
					t.Fatalf("foreign pull was accepted: status=%d error=%v", status, err)
				}
				w.executor.mu.Lock()
				w.executor.status, w.executor.hasStatus = Status{Version: ProtocolVersion, WorkerState: "ready"}, true
				w.executor.mu.Unlock()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			want := error(context.DeadlineExceeded)
			if !test.forge {
				cancel()
				want = context.Canceled
			}
			if err := w.awaitHeldPull(ctx); !errors.Is(err, want) {
				t.Fatalf("unheld pull appeared ready or lost cancellation: %v", err)
			}
			if w.cmd.ProcessState == nil {
				t.Fatal("canceled readiness wait left the child live")
			}
			if _, err := os.Lstat(w.endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("canceled readiness wait left the endpoint: %v", err)
			}
		})
	}
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

func TestLocalWorkerExternalCloseDoesNotReportExpectedInterrupt(t *testing.T) {
	w := localWorkerTestStart(t, "hold")
	job := testJob(command.Comment)
	done := make(chan httpServerTestResult, 1)
	go func() {
		result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{})
		done <- httpServerTestResult{result, err}
	}()
	unixTestReady(t, w.executor)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if !got.result.matchesJob(job) || !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || strings.Contains(got.err.Error(), "local worker exited") {
			t.Fatalf("expected child interrupt became an exit failure: result=%+v error=%v", got.result, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("externally closed action did not finish")
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
	done := make(chan httpServerTestResult, 1)
	go func() {
		result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{})
		done <- httpServerTestResult{result, err}
	}()
	unixTestReady(t, w.executor)
	if err := w.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		var exit *exec.ExitError
		if !got.result.matchesJob(job) || !errors.As(got.err, &exit) || exit.ExitCode() != 7 || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !strings.Contains(got.err.Error(), "local worker exited") {
			t.Fatalf("pre-delivery exit lost its diagnostic or became uncertain: result=%+v error=%v", got.result, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pre-delivery child exit did not finish action")
	}
	if w.cmd.ProcessState == nil {
		t.Fatal("pre-delivery child was not reaped")
	}
}

func TestExpectedLocalWorkerCancellationRequiresSentStop(t *testing.T) {
	if os.Getenv("MINION_LOCAL_WORKER_SELF_SIGINT") == "1" {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		t.Fatal("SIGINT did not terminate helper")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestExpectedLocalWorkerCancellationRequiresSentStop$")
	cmd.Env = append(os.Environ(), "MINION_LOCAL_WORKER_SELF_SIGINT=1")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("helper did not exit on SIGINT: %v", err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT || expectedLocalWorkerCancellation(err, false) || !expectedLocalWorkerCancellation(err, true) || expectedLocalWorkerCancellation(context.Canceled, false) {
		t.Fatalf("external SIGINT without owner stop was suppressed: %v", err)
	}
}

func TestLocalWorkerBudgetQuarantineStopsLiveChild(t *testing.T) {
	w := localWorkerTestStart(t, "stall")
	w.executor.outcomeGrace = 25 * time.Millisecond
	localWorkerTestPull(t, w)
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
	w, err := startLocalWorker(dir, "local", filepath.Join(dir, "missing"), nil, nil)
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
	if mode == "stop-exit" || mode == "stop-success" || mode == "run-http-idle" {
		interrupt = make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		defer signal.Stop(interrupt)
	}
	var crash chan os.Signal
	if mode == "precrash" {
		crash = make(chan os.Signal, 1)
		signal.Notify(crash, syscall.SIGUSR1)
		defer signal.Stop(crash)
	}
	if err := workspace.WriteAtomic(report+".ready", []byte(strconv.Itoa(os.Getpid()))); err != nil {
		t.Fatal(err)
	}
	if mode == "preflight-fail" {
		fmt.Fprintln(os.Stderr, strings.Repeat("x", 16<<10)+"\nselected Codex CLI unavailable "+os.Getenv("CODEX_API_KEY")+strings.TrimSpace(os.Getenv("GH_TOKEN"))+" "+os.Getenv("PGPASSWORD")+" "+os.Getenv("HTTPS_PROXY")+" "+preflightTestProxyUser+" "+preflightTestProxyPassword+" p@ss\nforged log line")
		os.Exit(7)
	}
	if mode == "stop-exit" || mode == "stop-success" {
		<-interrupt
		if mode == "stop-exit" {
			os.Exit(7)
		}
		return
	}
	if mode == "run-http-idle" {
		err := RunHTTP(t.Context(), client, UnixExecutionBaseURL, "local", &Runner{})
		if !errors.Is(err, context.Canceled) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("idle pull did not receive intentional shutdown: %v", err)
		}
		return
	}
	if mode == "precrash" {
		<-crash
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
