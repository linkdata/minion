//go:build !windows

package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/executil"
	"github.com/linkdata/minion/internal/monitor"
)

// localWorker owns one direct child and its private HTTP endpoint.
type localWorker struct {
	executor     *HTTPExecutor
	endpoint     *UnixEndpoint
	cmd          *exec.Cmd
	cancel       context.CancelFunc
	exited       chan struct{}
	childMu      sync.Mutex
	stopSent     atomic.Bool
	unclean      atomic.Bool
	reuse        atomic.Int32
	stderr       *bounded.Tail
	waitErr      error
	killErr      error
	stopUntil    time.Time
	stopTimer    *time.Timer
	closeOnce    sync.Once
	closeErr     error
	readyTimeout time.Duration
	stopGrace    time.Duration
}

const (
	workerReusable int32 = iota
	workerRunning
	workerNeedsPull
	workerCheckingPull
	workerRetired
)

// startLocalWorker owns the direct child's context and command. configure
// prepares the child command; stderr is captured for a safe startup diagnostic.
// Job cancellation is handled by Execute.
func startLocalWorker(runtimeDir, executorID, path string, args []string, readyTimeout time.Duration, credentials WorkerCredentials, configure func(*exec.Cmd, string) error) (*localWorker, error) {
	if path == "" {
		return nil, errors.New("local worker command path is required")
	}
	if readyTimeout <= 0 {
		return nil, errors.New("local worker readiness timeout must be positive")
	}
	executor, err := NewHTTPExecutor(executorID)
	if err != nil {
		return nil, err
	}
	if err := executor.SetWorkerCredentials(credentials); err != nil {
		return nil, err
	}
	endpoint, err := ListenUnixExecution(runtimeDir, executor)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, path, args...)
	commandCancel := cmd.Cancel
	if configure != nil {
		if err := configure(cmd, endpoint.SocketPath()); err != nil {
			cancel()
			return nil, errors.Join(fmt.Errorf("configure local worker: %w", err), endpoint.Close())
		}
	}
	stderr := &bounded.Tail{}
	cmd.Stderr = stderr
	cmd.Cancel = commandCancel
	executil.Prepare(cmd)
	w := &localWorker{executor: executor, endpoint: endpoint, cmd: cmd, cancel: cancel, exited: make(chan struct{}), stderr: stderr, readyTimeout: readyTimeout, stopGrace: 2 * time.Second}
	w.reuse.Store(workerNeedsPull)
	preparedCancel := cmd.Cancel
	cmd.Cancel = func() error {
		err := preparedCancel()
		if err == nil {
			w.stopSent.Store(true)
		}
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, errors.Join(fmt.Errorf("start local worker: %w", err), endpoint.Close())
	}
	go func() {
		waitErr := cmd.Wait()
		w.childMu.Lock()
		w.waitErr = waitErr
		close(w.exited)
		w.childMu.Unlock()
		_ = w.Close()
	}()
	go func() {
		<-endpoint.serveDone
		if endpoint.serveErr != nil {
			_ = w.Close()
		}
	}()
	return w, nil
}

// AttachLocalWorker connects one selected direct child without changing the
// default service mode. The caller watches its exit and owns the close function.
func (e *Orchestrator) AttachLocalWorker(ctx context.Context, cfg config.Config, runtimeDir, path string, args []string, readyTimeout time.Duration, configure func(*exec.Cmd, string, WorkerSelection, WorkerCredentials) error) (WorkerSelection, <-chan struct{}, func() error, error) {
	if e.Executor != nil {
		return WorkerSelection{}, nil, nil, errors.New("orchestrator already has an executor")
	}
	if configure == nil {
		return WorkerSelection{}, nil, nil, errors.New("local worker startup configuration is required")
	}
	selected, err := e.WorkerSelection(cfg)
	if err != nil {
		return WorkerSelection{}, nil, nil, err
	}
	credentials := WorkerCredentials{}
	uses := func(provider string) bool {
		return selected.Main.Provider == provider || selected.Critic != nil && selected.Critic.Provider == provider
	}
	if uses("codex") {
		credentials.CodexAPIKey = e.Runner.Codex.APIKey
		if credentials.CodexAPIKey == "" {
			credentials.CodexNativeAPIKey = e.Runner.Codex.NativeAPIKey
			credentials.CodexAccessToken = e.Runner.Codex.NativeAccessToken
		}
	}
	if uses("claude") {
		credentials.ClaudeAPIKey = e.Runner.Claude.APIKey
	}
	w, err := startLocalWorker(runtimeDir, "local", path, args, readyTimeout, credentials, func(cmd *exec.Cmd, socket string) error {
		return configure(cmd, socket, selected, credentials)
	})
	if err != nil {
		return WorkerSelection{}, nil, nil, redactActionError(err, e.Runner.secretValues()...)
	}
	if err := w.WaitCapacity(ctx); err != nil {
		// WaitCapacity closes the child and endpoint on failure.
		return WorkerSelection{}, nil, nil, redactActionError(err, e.Runner.secretValues()...)
	}
	e.Executor = w
	return selected, w.exited, w.Close, nil
}

func (w *localWorker) Execute(ctx context.Context, job Job, sink Checkpoints) (Result, error) {
	if !w.reuse.CompareAndSwap(workerReusable, workerRunning) {
		if w.reuse.Load() == workerRetired {
			return Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head}, fmt.Errorf("%w: local worker is closed or not ready for another assignment", monitor.ErrRequestTransport)
		}
		return Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head}, fmt.Errorf("%w: local worker is busy or not ready for another assignment", monitor.ErrRequestTransport)
	}
	state := httpExecutionState{onStop: w.beginStop}
	result, err := w.executor.execute(ctx, 0, job, sink, &state)
	w.executor.mu.Lock()
	closed, unavailable := w.executor.closed, w.executor.unavailable
	w.executor.mu.Unlock()
	cleanupFailed := false
	// RunHTTP sends the outcome after Runner cleanup; a reported failure retires this worker.
	for _, diagnostic := range result.Diagnostics {
		cleanupFailed = cleanupFailed || diagnostic.Code == "cleanup_failed"
	}
	if cleanupFailed {
		w.unclean.Store(true)
	}
	if closed || unavailable || cleanupFailed || w.reuse.Load() == workerRetired || state.admitted && (ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded)) {
		w.reuse.Store(workerRetired)
		closeErr := w.Close()
		// A received outcome remains the Job result; Close reports lifecycle errors separately.
		if !state.outcome {
			err = errors.Join(err, closeErr)
			if (closed || unavailable) && ctx.Err() == nil && !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) && !errors.Is(err, monitor.ErrTerminalJob) {
				err = errors.Join(err, monitor.ErrRequestTransport)
			}
			if closed && state.admitted {
				err = errors.Join(err, errors.New("local worker stopped before a final outcome"))
			}
		}
	} else if state.outcome {
		w.reuse.CompareAndSwap(workerRunning, workerNeedsPull)
	} else {
		w.reuse.CompareAndSwap(workerRunning, workerReusable)
	}
	return result, err
}

// WaitCapacity releases capacity only after a pull is held at the original
// endpoint while the direct child is live, initially and after each outcome.
func (w *localWorker) WaitCapacity(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, w.readyTimeout)
	defer cancel()
	err := w.awaitReusable(ctx)
	if err != nil && w.reuse.Load() == workerRetired {
		return fmt.Errorf("%w: %w", ErrLocalWorkerUnavailable, err)
	}
	return err
}

func (w *localWorker) awaitReusable(ctx context.Context) (reuseErr error) {
	if !w.reuse.CompareAndSwap(workerNeedsPull, workerCheckingPull) && !w.reuse.CompareAndSwap(workerReusable, workerCheckingPull) {
		if w.unclean.Load() {
			return errors.New("local worker attempt cleanup failed; capacity is unavailable")
		}
		select {
		case <-w.endpoint.serveDone:
			if w.endpoint.serveErr != nil {
				return errors.Join(errors.New("local worker endpoint stopped before a pull"), w.endpoint.serveErr, w.Close())
			}
		default:
		}
		select {
		case <-w.exited:
			return errors.Join(w.exitBeforePull(), w.Close())
		default:
		}
		if w.reuse.Load() == workerRetired {
			return errors.Join(errors.New("local worker closed before reuse proof"), w.Close())
		}
		return errors.New("local worker has no completed attempt awaiting reuse")
	}
	defer func() {
		if reuseErr != nil {
			w.reuse.Store(workerRetired)
			reuseErr = errors.Join(reuseErr, w.Close())
		} else if !w.reuse.CompareAndSwap(workerCheckingPull, workerReusable) {
			reuseErr = errors.Join(errors.New("local worker closed during reuse proof"), w.Close())
		}
	}()
	if err := w.checkCapacityEndpoint(); err != nil {
		return err
	}
	if err := w.awaitHeldPull(ctx); err != nil {
		return fmt.Errorf("local worker capacity pull: %w", err)
	}
	if err := w.checkCapacityEndpoint(); err != nil {
		return err
	}
	return nil
}

func (w *localWorker) checkCapacityEndpoint() error {
	err := w.endpoint.checkIdentity()
	if err == nil {
		return nil
	}
	select {
	case <-w.exited:
		err = errors.Join(err, w.exitBeforePull())
	default:
	}
	return fmt.Errorf("local worker capacity endpoint: %w", err)
}

func (w *localWorker) exitBeforePull() error {
	err := errors.Join(errors.New("local worker exited before holding a pull"), w.waitErr)
	if detail := w.startupDiagnostic(); detail != "" {
		err = errors.Join(err, fmt.Errorf("worker startup diagnostic: %s", detail))
	}
	return err
}

func (w *localWorker) startupDiagnostic() string {
	for _, line := range strings.Split(w.stderr.String(), "\n") {
		if message, ok := strings.CutPrefix(line, "minion preflight: "); ok && validPreflightMessage(message) {
			return message
		}
	}
	return ""
}

// awaitHeldPull proves only that this endpoint accepted a pull while its
// direct child was live; selected-tool preflight and capacity are separate.
func (w *localWorker) awaitHeldPull(ctx context.Context) (waitErr error) {
	defer func() {
		if waitErr != nil {
			waitErr = errors.Join(waitErr, w.Close())
		}
	}()
	for {
		w.executor.mu.Lock()
		pull, pullDone, closed, unavailable, changed := w.executor.pull, w.executor.pullDone, w.executor.closed, w.executor.unavailable, w.executor.changed
		w.executor.mu.Unlock()
		select {
		case <-w.exited:
			return w.exitBeforePull()
		default:
		}
		select {
		case <-w.endpoint.serveDone:
			return errors.Join(errors.New("local worker endpoint stopped before a pull"), w.endpoint.serveErr)
		default:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if closed || unavailable {
			return errors.New("local worker executor stopped before holding a pull")
		}
		if pull {
			select {
			case <-pullDone:
				// Wait for the canceled handler to release its pull slot.
			default:
				return nil
			}
		}
		select {
		case <-w.exited:
		case <-w.endpoint.serveDone:
		case <-changed:
		case <-ctx.Done():
		}
	}
}

// beginStop retires capacity and bounds the direct child to the HTTP stop deadline.
func (w *localWorker) beginStop(until time.Time) {
	w.reuse.Store(workerRetired)
	w.stopUntil = until
	w.stopTimer = time.AfterFunc(max(time.Until(until), 0), func() {
		w.childMu.Lock()
		defer w.childMu.Unlock()
		select {
		case <-w.exited:
			return
		default:
		}
		if err := w.cmd.Process.Kill(); err == nil {
			w.stopSent.Store(true)
		} else if !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
			w.killErr = fmt.Errorf("stop local worker at deadline: %w", err)
		}
	})
}

// Close drains accepted callbacks while HTTP serves, then joins the direct
// child and endpoint without starting another stop grace period.
func (w *localWorker) Close() error {
	w.reuse.Store(workerRetired)
	w.closeOnce.Do(func() {
		started := time.Now()
		coreErr := w.executor.Close()
		// Close joins the transaction, including its single stop notification.
		until := w.stopUntil
		if until.IsZero() {
			w.beginStop(started.Add(w.stopGrace))
			until = w.stopUntil
		}
		// Give an idle child a chance to consume the closed pull before SIGINT.
		signalUntil := started.Add(w.stopGrace - w.stopGrace/4)
		if until.Before(signalUntil) {
			signalUntil = until
		}
		grace := time.NewTimer(max(time.Until(signalUntil), 0))
		select {
		case <-w.exited:
		case <-grace.C:
		}
		grace.Stop()
		// A published exit before cancellation is spontaneous, including one
		// observed while accepted callbacks drain. Recheck under childMu so
		// the waiter cannot publish an exit between this check and the stop.
		w.childMu.Lock()
		exitedBeforeCancel := false
		select {
		case <-w.exited:
			exitedBeforeCancel = true
		default:
			w.cancel()
		}
		w.childMu.Unlock()
		<-w.exited
		w.stopTimer.Stop()
		if exitedBeforeCancel {
			w.cancel() // Release the command context after a graceful exit.
		}
		// Cmd.Wait may have returned before cancellation but published afterward.
		// Keep every nonzero exit code and other unexpected Wait error.
		if w.waitErr != nil && !expectedLocalWorkerCancellation(w.waitErr, w.stopSent.Load()) {
			w.closeErr = fmt.Errorf("local worker exited: %w", w.waitErr)
		}
		w.childMu.Lock()
		w.closeErr = errors.Join(w.closeErr, w.killErr)
		w.childMu.Unlock()
		stopContext, cancelStop := context.WithDeadline(context.Background(), until)
		w.closeErr = errors.Join(w.closeErr, coreErr, w.endpoint.closeContext(stopContext))
		cancelStop()
	})
	return w.closeErr
}

func expectedLocalWorkerCancellation(err error, directStopSent bool) bool {
	if !directStopSent {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true // Cmd.Wait can report context cancellation after a successful exit.
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && (status.Signal() == syscall.SIGINT || status.Signal() == syscall.SIGKILL)
}
