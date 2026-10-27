package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func startHTTPSWorkerTest(t *testing.T, mode string) *localWorker {
	t.Helper()
	w, err := startLocalWorker("local", os.Args[0], []string{"-test.run=^TestLocalWorkerHTTPSHelper$"}, 5*time.Second, WorkerCredentials{}, func(cmd *exec.Cmd, access LocalEndpointAccess) error {
		input, err := json.Marshal(access)
		if err != nil {
			return err
		}
		cmd.Stdin = bytes.NewReader(input)
		cmd.Env = append(os.Environ(), "MINION_HTTPS_WORKER_TEST="+mode, "GORACE=atexit_sleep_ms=0")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestLocalWorkerRequiresAuthenticatedHeldPull(t *testing.T) {
	w := startHTTPSWorkerTest(t, "idle")
	job := testJob(command.Comment)
	if result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{}); err == nil || !strings.Contains(err.Error(), "not ready") || !result.matchesJob(job) {
		t.Fatalf("assignment bypassed capacity proof: result=%+v error=%v", result, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatalf("live held pull lost ready capacity: %v", err)
	}
	if err := w.Close(); err != nil || w.cmd.ProcessState == nil || !w.cmd.ProcessState.Success() || w.stopSent.Load() {
		t.Fatalf("idle worker failed graceful close: error=%v state=%v direct-stop=%v", err, w.cmd.ProcessState, w.stopSent.Load())
	}
	if w.stopUntil.IsZero() {
		t.Fatal("idle close did not set one worker stop deadline")
	}
}

func TestLocalWorkerCanceledHTTPSPullCannotSupplyCapacity(t *testing.T) {
	w := startHTTPSWorkerTest(t, "hold")
	access := w.endpoint.Access()
	client, err := NewLocalExecutionClient(access)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "local"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, cancelRequest := context.WithCancel(t.Context())
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, access.BaseURL+"/next", bytes.NewReader(pull))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	requestDone := make(chan struct{})
	go func() {
		response, _ := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		close(requestDone)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var pullDone <-chan struct{}
	for pullDone == nil {
		w.executor.mu.Lock()
		if w.executor.pull {
			pullDone = w.executor.pullDone
		}
		changed := w.executor.changed
		w.executor.mu.Unlock()
		if pullDone == nil {
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatal("HTTPS pull was not held")
			}
		}
	}
	cancelRequest()
	select {
	case <-pullDone:
	case <-ctx.Done():
		t.Fatal("held HTTPS pull did not track cancellation")
	}
	select {
	case <-requestDone:
	case <-ctx.Done():
		t.Fatal("canceled HTTPS pull did not finish")
	}
	w.readyTimeout = 100 * time.Millisecond
	if err := w.WaitCapacity(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled HTTPS pull appeared ready: %v", err)
	}
	if w.cmd.ProcessState == nil || w.reuse.Load() != workerRetired {
		t.Fatalf("canceled pull left child or capacity live: process=%v state=%d", w.cmd.ProcessState, w.reuse.Load())
	}
}

func TestLocalWorkerDirectStopAndCrashAreDistinct(t *testing.T) {
	t.Run("direct stop", func(t *testing.T) {
		w := startHTTPSWorkerTest(t, "ignore-idle-cancel")
		w.stopGrace = 25 * time.Millisecond
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := w.WaitCapacity(ctx); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil || !w.stopSent.Load() || w.cmd.ProcessState == nil {
			t.Fatalf("direct stop failed: error=%v sent=%v state=%v", err, w.stopSent.Load(), w.cmd.ProcessState)
		}
		until := w.stopUntil
		if until.IsZero() {
			t.Fatal("unresponsive idle worker was stopped without a deadline")
		}
		w.beginStop(until.Add(time.Minute))
		if !w.stopUntil.Equal(until) {
			t.Fatalf("idle stop deadline was renewed: first=%v later=%v", until, w.stopUntil)
		}
	})
	t.Run("crash before pull", func(t *testing.T) {
		w := startHTTPSWorkerTest(t, "crash-before-pull")
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := w.WaitCapacity(ctx); err == nil || errors.Is(err, errLocalWorkerCleanupUnverified) || w.cmd.ProcessState == nil || w.cmd.ProcessState.Success() {
			t.Fatalf("crash became capacity or a clean stop: error=%v state=%v", err, w.cmd.ProcessState)
		}
	})
}

func TestLocalWorkerCrashAndCleanupFailureRetireCapacity(t *testing.T) {
	for _, mode := range []string{"crash-after-job", "cleanup-failed"} {
		t.Run(mode, func(t *testing.T) {
			w := startHTTPSWorkerTest(t, mode)
			w.executor.outcomeGrace = 25 * time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := w.WaitCapacity(ctx); err != nil {
				t.Fatal(err)
			}
			job := testJob(command.Comment)
			result, err := w.Execute(ctx, job, runnerExecutionCheckpoints{})
			if !result.matchesJob(job) || w.reuse.Load() != workerRetired || w.cmd.ProcessState == nil {
				t.Fatalf("worker retained capacity: result=%+v error=%v state=%d process=%v", result, err, w.reuse.Load(), w.cmd.ProcessState)
			}
			if mode == "crash-after-job" && (!errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(err, errLocalWorkerCleanupUnverified) || !strings.Contains(err.Error(), "exited")) {
				t.Fatalf("missing outcome became known: %v", err)
			}
			if mode == "cleanup-failed" && (err != nil || !w.unclean.Load() || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "cleanup_failed") {
				t.Fatalf("known cleanup failure was lost: result=%+v error=%v unclean=%v", result, err, w.unclean.Load())
			}
		})
	}
}

func TestLocalWorkerStopKeepsHTTPDeadline(t *testing.T) {
	for _, stop := range []string{"cancel", "close", "expiry"} {
		t.Run(stop, func(t *testing.T) {
			w := startHTTPSWorkerTest(t, "stall-after-job")
			w.executor.outcomeGrace = 200 * time.Millisecond
			wait, stopWait := context.WithTimeout(t.Context(), 5*time.Second)
			defer stopWait()
			if err := w.WaitCapacity(wait); err != nil {
				t.Fatal(err)
			}
			job := testJob(command.Comment)
			if stop == "expiry" {
				job.RuntimeBudget = 1500 * time.Millisecond
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan httpServerTestResult, 1)
			go func() {
				result, err := w.Execute(ctx, job, runnerExecutionCheckpoints{})
				done <- httpServerTestResult{result, err}
			}()
			tx := waitLocalWorkerDelivery(t, wait, w)
			closed := make(chan error, 1)
			switch stop {
			case "cancel":
				cancel()
			case "close":
				go func() { closed <- w.Close() }()
			}
			select {
			case <-tx.stop:
			case <-wait.Done():
				t.Fatal("HTTP transaction did not start its stop deadline")
			}
			until := tx.controlDeadline()
			if until.IsZero() || !w.stopUntil.Equal(until) || w.reuse.Load() != workerRetired {
				t.Fatalf("stop did not retire worker on HTTP deadline: HTTP=%v worker=%v reuse=%d", until, w.stopUntil, w.reuse.Load())
			}
			w.beginStop(until.Add(time.Minute))
			if !w.stopUntil.Equal(until) {
				t.Fatalf("later stop renewed deadline: first=%v later=%v", until, w.stopUntil)
			}
			childLimit := time.NewTimer(time.Until(until.Add(time.Second)))
			defer childLimit.Stop()
			select {
			case <-w.exited:
			case <-childLimit.C:
				t.Fatal("direct child outlived the first stop deadline")
			}
			select {
			case got := <-done:
				if !got.result.matchesJob(job) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
					t.Fatalf("stopped delivered attempt changed outcome: result=%+v error=%v", got.result, got.err)
				}
			case <-wait.Done():
				t.Fatal("stopped attempt did not finish")
			}
			if stop == "close" {
				select {
				case <-closed:
				case <-wait.Done():
					t.Fatal("service close did not join the direct child")
				}
			}
			if w.cmd.ProcessState == nil || w.reuse.Load() != workerRetired {
				t.Fatalf("stopped worker regained capacity or left direct child: process=%v reuse=%d", w.cmd.ProcessState, w.reuse.Load())
			}
		})
	}
}

func waitLocalWorkerDelivery(t *testing.T, ctx context.Context, w *localWorker) *httpTransaction {
	t.Helper()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		if tx := w.executor.transaction(); tx != nil {
			tx.mu.Lock()
			delivered := tx.delivered
			tx.mu.Unlock()
			if delivered {
				return tx
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("local worker did not receive its assignment")
		case <-ticker.C:
		}
	}
}

func TestLocalWorkerKnownOutcomeSurvivesChildExit(t *testing.T) {
	w := startHTTPSWorkerTest(t, "known-then-exit")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	job := testJob(command.Comment)
	result, err := w.Execute(ctx, job, runnerExecutionCheckpoints{})
	if err != nil || result.Answer != "known" || !result.matchesJob(job) {
		t.Fatalf("accepted result changed after child exit: result=%+v error=%v", result, err)
	}
	select {
	case <-w.exited:
	case <-ctx.Done():
		t.Fatal("known-result child was not reaped")
	}
	if w.waitErr == nil || w.cmd.ProcessState == nil {
		t.Fatalf("unexpected exit was not observed: wait=%v state=%v", w.waitErr, w.cmd.ProcessState)
	}
	if err := w.WaitCapacity(ctx); err == nil {
		t.Fatal("exited child gained another ready slot")
	}
	var exit *exec.ExitError
	if err := w.Close(); !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("known result hid separate close error: %v", err)
	}
}

func TestLocalWorkerDrainsPaidCallbackBeforeChildStop(t *testing.T) {
	for _, stop := range []string{"close", "cancel"} {
		t.Run(stop, func(t *testing.T) {
			w := startHTTPSWorkerTest(t, "drain")
			w.executor.outcomeGrace = 2 * time.Second
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := w.WaitCapacity(ctx); err != nil {
				t.Fatal(err)
			}
			job := testJob(command.Comment)
			entered, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			actionCtx, cancelAction := context.WithCancel(t.Context())
			defer cancelAction()
			done := make(chan httpServerTestResult, 1)
			go func() {
				result, err := w.Execute(actionCtx, job, runnerExecutionCheckpoints{after: func(CallCheckpoint) error {
					close(entered)
					<-release
					return nil
				}})
				done <- httpServerTestResult{result, err}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("child did not enter paid callback")
			}
			tx := w.executor.transaction()
			if tx == nil {
				t.Fatal("paid callback lost its transaction")
			}
			closed := make(chan error, 1)
			if stop == "close" {
				go func() { closed <- w.Close() }()
			} else {
				cancelAction()
			}
			select {
			case <-tx.budget.Done():
			case <-ctx.Done():
				t.Fatal("stop did not reach the transaction")
			}
			select {
			case err := <-closed:
				t.Fatalf("worker closed before callback drained: %v", err)
			default:
			}
			select {
			case <-w.exited:
				t.Fatal("child exited before callback drained")
			default:
			}
			close(release)
			select {
			case got := <-done:
				if !errors.Is(got.err, context.Canceled) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || got.result.Answer != "known" || len(got.result.Calls) != 1 || got.result.Calls[0].Tokens.Input != 7 {
					t.Fatalf("paid receipt or known outcome lost: result=%+v error=%v", got.result, got.err)
				}
			case <-ctx.Done():
				t.Fatal("paid callback did not finish")
			}
			if stop == "close" {
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("Close did not reap child")
				}
			}
			if w.cmd.ProcessState == nil || w.reuse.Load() != workerRetired {
				t.Fatalf("stop retained child or capacity: process=%v state=%d", w.cmd.ProcessState, w.reuse.Load())
			}
		})
	}
}

func TestLocalWorkerShortStopRetiresWhileCallbackDrains(t *testing.T) {
	w := startHTTPSWorkerTest(t, "drain")
	w.executor.outcomeGrace = 100 * time.Millisecond
	wait, stopWait := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopWait()
	if err := w.WaitCapacity(wait); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	job := testJob(command.Comment)
	done := make(chan httpServerTestResult, 1)
	go func() {
		result, err := w.Execute(t.Context(), job, runnerExecutionCheckpoints{after: func(CallCheckpoint) error {
			close(entered)
			<-release
			return nil
		}})
		done <- httpServerTestResult{result, err}
	}()
	select {
	case <-entered:
	case <-wait.Done():
		t.Fatal("direct child did not enter paid callback")
	}
	tx := w.executor.transaction()
	if tx == nil {
		t.Fatal("paid callback lost its transaction")
	}
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	select {
	case <-tx.stop:
	case <-wait.Done():
		t.Fatal("service close did not start HTTP stop")
	}
	until := tx.controlDeadline()
	if until.IsZero() || !w.stopUntil.Equal(until) || w.reuse.Load() != workerRetired {
		t.Fatalf("blocked callback kept capacity or renewed deadline: HTTP=%v worker=%v reuse=%d", until, w.stopUntil, w.reuse.Load())
	}
	childLimit := time.NewTimer(time.Until(until.Add(time.Second)))
	defer childLimit.Stop()
	select {
	case <-w.exited:
	case <-childLimit.C:
		t.Fatal("direct child outlived short HTTP stop deadline")
	}
	if w.cmd.ProcessState == nil || w.reuse.Load() != workerRetired {
		t.Fatalf("unresolved callback regained capacity or kept child: process=%v reuse=%d", w.cmd.ProcessState, w.reuse.Load())
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before entered callback drained: %v", err)
	default:
	}
	if err := w.endpoint.checkAlive(); err != nil {
		t.Fatalf("endpoint stopped before entered callback drained: %v", err)
	}
	close(release)
	select {
	case got := <-done:
		if !got.result.matchesJob(job) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("stopped callback changed unknown outcome: result=%+v error=%v", got.result, got.err)
		}
	case <-wait.Done():
		t.Fatal("entered callback did not drain")
	}
	select {
	case <-closed:
	case <-wait.Done():
		t.Fatal("Close did not join callback and endpoint")
	}
}

func TestLocalWorkerCloseStopsHTTPSBeforeDelayedChildWait(t *testing.T) {
	executor, err := NewHTTPExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := ListenLocalExecution(executor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	holdAuthenticatedUnfinishedLocalRequest(t, endpoint)
	until := time.Now().Add(150 * time.Millisecond)
	exited := make(chan struct{})
	stopTimer := time.NewTimer(time.Hour)
	w := &localWorker{
		executor: executor, endpoint: endpoint, exited: exited,
		cancel: func() {}, stopGrace: 2 * time.Second,
		stopUntil: until, stopTimer: stopTimer,
	}
	// Model a transaction that already began stopping while Cmd.Wait is delayed.
	w.stopOnce.Do(func() {})
	release := func() {
		select {
		case <-exited:
		default:
			close(exited)
		}
	}
	t.Cleanup(func() {
		release()
		stopTimer.Stop()
	})
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	limit := time.NewTimer(2 * time.Second)
	defer limit.Stop()
	select {
	case <-endpoint.serveDone:
	case <-limit.C:
		t.Fatal("HTTPS listener stayed open while direct child wait was delayed")
	}
	deadline := time.NewTimer(time.Until(until))
	defer deadline.Stop()
	<-deadline.C
	select {
	case err := <-closed:
		t.Fatalf("Close returned before direct child wait was published: %v", err)
	default:
	}
	release()
	select {
	case err := <-closed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("held HTTPS request did not consume first stop deadline: %v", err)
		}
	case <-limit.C:
		t.Fatal("Close did not join the delayed direct child wait")
	}
}

func TestLocalWorkerCloseKeepsHTTPSForRepullBetweenJobs(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "repull")
	t.Setenv("MINION_HTTPS_REPULL_GATE", gate)
	w := startHTTPSWorkerTest(t, "paused-repull")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	job := testJob(command.Comment)
	if result, err := w.Execute(ctx, job, runnerExecutionCheckpoints{}); err != nil || !result.matchesJob(job) {
		t.Fatalf("first assignment failed: result=%+v error=%v", result, err)
	}
	release := func() { _ = os.WriteFile(gate+".release", nil, 0600) }
	t.Cleanup(release)
	for {
		if _, err := os.Stat(gate + ".paused"); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			t.Fatal("worker did not pause between assignments")
		}
	}
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	for {
		w.executor.mu.Lock()
		stopped, changed := w.executor.closed, w.executor.changed
		w.executor.mu.Unlock()
		if stopped {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("worker close did not stop assignments")
		}
	}
	observe := time.NewTimer(50 * time.Millisecond)
	defer observe.Stop()
	select {
	case <-w.endpoint.serveDone:
		t.Fatal("HTTPS stopped before the worker could repull")
	case <-observe.C:
	case <-ctx.Done():
		t.Fatal("worker close did not leave time to repull")
	}
	release()
	select {
	case err := <-closed:
		if err != nil || w.cmd.ProcessState == nil || !w.cmd.ProcessState.Success() || w.stopSent.Load() {
			t.Fatalf("worker did not receive clean shutdown: error=%v state=%v direct-stop=%v", err, w.cmd.ProcessState, w.stopSent.Load())
		}
	case <-ctx.Done():
		t.Fatal("worker did not finish clean repull shutdown")
	}
}

func TestLocalWorkerIgnoresReapedProcessBeforeExitPublication(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestLocalWorkerHTTPSHelper$")
	cmd.Env = append(os.Environ(), "MINION_HTTPS_WORKER_TEST=", "GORACE=atexit_sleep_ms=0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	state, err := cmd.Process.Wait()
	if err != nil || !state.Success() {
		t.Fatalf("direct child did not exit cleanly: state=%v error=%v", state, err)
	}
	// Process.Wait has reaped the child, but Cmd.Wait can still be copying stderr.
	if err := cmd.Process.Kill(); err == nil || !localWorkerProcessDone(err) {
		t.Fatalf("reaped child was not recognized: %v", err)
	}
	w := &localWorker{cmd: cmd, exited: make(chan struct{}), cancel: func() {}}
	w.stopDirectChild()
	if w.stopErr != nil || w.stopSent.Load() {
		t.Fatalf("reaped child became a stop failure: error=%v signal=%v", w.stopErr, w.stopSent.Load())
	}
	if localWorkerProcessDone(errors.New("other kill failure")) {
		t.Fatal("unrelated kill failure was hidden")
	}
}

func TestLocalWorkerClosePreservesExitDuringCallbackDrain(t *testing.T) {
	w := startHTTPSWorkerTest(t, "drain")
	w.executor.outcomeGrace = 6 * time.Second // The manual crash must precede automatic stop, even on a loaded host.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}
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
		result, err := w.Execute(t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{after: func(CallCheckpoint) error {
			close(entered)
			<-release
			return nil
		}})
		done <- httpServerTestResult{result, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("child did not enter paid callback")
	}
	tx := w.executor.transaction()
	if tx == nil {
		t.Fatal("paid callback lost its transaction")
	}
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	select {
	case <-tx.budget.Done():
	case <-ctx.Done():
		t.Fatal("Close did not begin callback drain")
	}
	select {
	case <-tx.stop:
	case <-ctx.Done():
		t.Fatal("Close did not start the stop deadline")
	}
	if err := w.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.exited:
	case <-ctx.Done():
		t.Fatal("exiting child was not reaped")
	}
	tx.mu.Lock()
	stopCancel := tx.stopCancel
	tx.mu.Unlock()
	if stopCancel == nil {
		t.Fatal("stop deadline was not available")
	}
	stopCancel() // Avoid waiting for the long automatic grace after proving the manual crash.
	select {
	case err := <-closed:
		t.Fatalf("Close returned before callback drained: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-closed:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !errors.Is(err, w.waitErr) || exit.ExitCode() == 0 || w.stopSent.Load() {
			t.Fatalf("spontaneous exit during drain was lost: close=%v wait=%v stop=%v", err, w.waitErr, w.stopSent.Load())
		}
	case <-ctx.Done():
		t.Fatal("Close did not finish after callback release")
	}
	select {
	case got := <-done:
		if !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) {
			t.Fatalf("missing outcome was not quarantined: result=%+v error=%v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("Execute did not finish after child exit")
	}
	if w.cmd.ProcessState == nil || w.cmd.ProcessState.Success() {
		t.Fatalf("nonzero child exit was not reaped: %v", w.cmd.ProcessState)
	}
}

func TestLocalWorkerListenerLossCannotSupplyCapacity(t *testing.T) {
	w := startHTTPSWorkerTest(t, "idle")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := w.WaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.endpoint.listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.endpoint.serveDone:
	case <-ctx.Done():
		t.Fatal("listener loss did not stop serving")
	}
	if err := w.WaitCapacity(ctx); err == nil || !errors.Is(err, ErrLocalWorkerUnavailable) {
		t.Fatalf("lost listener supplied capacity: %v", err)
	}
}

func TestLocalWorkerStartupFailureClosesListener(t *testing.T) {
	var address string
	configurationFailure := errors.New("fixture configuration failure")
	if worker, err := startLocalWorker("local", os.Args[0], nil, time.Second, WorkerCredentials{}, func(_ *exec.Cmd, access LocalEndpointAccess) error {
		address = strings.TrimPrefix(access.BaseURL, "https://")
		return configurationFailure
	}); worker != nil || !errors.Is(err, configurationFailure) {
		t.Fatalf("invalid startup continued: worker=%v error=%v", worker, err)
	}
	if connection, err := net.DialTimeout("tcp4", address, 100*time.Millisecond); err == nil {
		connection.Close()
		t.Fatal("failed startup retained its listener")
	}
	address = ""
	missingExecutable := filepath.Join(t.TempDir(), "missing-minion-executable")
	if worker, err := startLocalWorker("local", missingExecutable, nil, time.Second, WorkerCredentials{}, func(_ *exec.Cmd, access LocalEndpointAccess) error {
		address = strings.TrimPrefix(access.BaseURL, "https://")
		return nil
	}); worker != nil || err == nil {
		t.Fatalf("missing executable continued: worker=%v error=%v", worker, err)
	}
	if address == "" {
		t.Fatal("missing executable did not reach worker start")
	}
	if connection, err := net.DialTimeout("tcp4", address, 100*time.Millisecond); err == nil {
		connection.Close()
		t.Fatal("failed child start retained its listener")
	}
}

func TestLocalWorkerHTTPSHelper(t *testing.T) {
	mode := os.Getenv("MINION_HTTPS_WORKER_TEST")
	if mode == "" {
		return
	}
	var access LocalEndpointAccess
	if err := json.NewDecoder(os.Stdin).Decode(&access); err != nil {
		t.Fatal(err)
	}
	client, err := NewLocalExecutionClient(access)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if mode == "crash-before-pull" {
		os.Exit(7)
	}
	if mode == "hold" {
		// Keep a timer live so the test process cannot report a false runtime
		// deadlock before the parent checks canceled-pull readiness.
		time.Sleep(30 * time.Second)
		t.Fatal("held worker was not stopped")
	}
	if mode == "idle" || mode == "ignore-idle-cancel" {
		if err := RunHTTPLoop(t.Context(), client, access.BaseURL, "local", &Runner{}); err != nil {
			t.Fatal(err)
		}
		if mode == "ignore-idle-cancel" {
			time.Sleep(30 * time.Second)
			t.Fatal("idle worker was not stopped")
		}
		return
	}
	pull, err := httpEncode(httpPull{Version: ProtocolVersion, ExecutorID: "local"}, httpControlBytes)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, access.BaseURL+"/next", bytes.NewReader(pull))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("worker pull: response=%v error=%v", response, err)
	}
	var assignment httpAssignment
	if err := httpDecode(response.Body, httpJobBytes, &assignment); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if mode == "crash-after-job" {
		os.Exit(7)
	}
	if mode == "stall-after-job" {
		time.Sleep(30 * time.Second)
		t.Fatal("stalled direct worker was not stopped")
	}
	result := Result{Version: ProtocolVersion, Identity: assignment.Job.Identity, SourceRef: assignment.Job.Source.Head, Answer: "known"}
	sequence := uint64(1)
	var failure *httpFailure
	if mode == "drain" {
		job := assignment.Job
		call := CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: 1, Tokens: usage.Tokens{Input: 7, Output: 2}, Complete: true}
		checkpoint := CallCheckpoint{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Call: callMetadata(call)}
		localTestAck(t, client, access.BaseURL, "/before", httpCall{Sequence: 1, Checkpoint: checkpoint}, 1)
		checkpoint.Call = call
		localTestAck(t, client, access.BaseURL, "/after", httpCall{Sequence: 2, Checkpoint: checkpoint}, 2)
		result.Calls = []CallUsage{call}
		sequence, failure = 3, newHTTPFailure(context.Canceled)
	}
	if mode == "cleanup-failed" {
		result.Diagnostics = []Diagnostic{{Code: "cleanup_failed", Message: "fixture cleanup failed"}}
	}
	outcome, err := httpEncode(httpOutcome{Sequence: sequence, Result: result, Failure: failure}, httpOutcomeBytes)
	if err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, access.BaseURL+"/outcome", bytes.NewReader(outcome))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err = client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("worker outcome: response=%v error=%v", response, err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if mode == "drain" {
		return
	}
	if mode == "known-then-exit" {
		os.Exit(7)
	}
	if mode == "paused-repull" {
		gate := os.Getenv("MINION_HTTPS_REPULL_GATE")
		if err := os.WriteFile(gate+".paused", nil, 0600); err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := os.Stat(gate + ".release"); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
		}
		if err := RunHTTPLoop(t.Context(), client, access.BaseURL, "local", &Runner{}); err != nil {
			t.Fatal(err)
		}
		return
	}
	select {}
}
