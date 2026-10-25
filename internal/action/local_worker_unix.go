//go:build !windows

package action

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/executil"
)

// localWorker owns one direct child and its private HTTP endpoint.
type localWorker struct {
	executor  *HTTPExecutor
	endpoint  *UnixEndpoint
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	exited    chan struct{}
	childMu   sync.Mutex
	stopSent  atomic.Bool
	unclean   atomic.Bool
	reuse     atomic.Int32
	stderr    *bounded.Tail
	waitErr   error
	closeOnce sync.Once
	closeErr  error
}

const (
	workerReusable int32 = iota
	workerRunning
	workerNeedsPull
	workerCheckingPull
	workerRetired
)

// startLocalWorker owns the direct child's context and command. configure sets
// environment and stdout; stderr is captured for a safe startup diagnostic.
// Job cancellation is handled by Execute.
func startLocalWorker(runtimeDir, executorID, path string, args []string, configure func(*exec.Cmd, string)) (*localWorker, error) {
	if path == "" {
		return nil, errors.New("local worker command path is required")
	}
	executor, err := NewHTTPExecutor(executorID)
	if err != nil {
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
		configure(cmd, endpoint.SocketPath())
	}
	stderr := &bounded.Tail{}
	cmd.Stderr = stderr
	cmd.Cancel = commandCancel
	executil.Prepare(cmd)
	w := &localWorker{executor: executor, endpoint: endpoint, cmd: cmd, cancel: cancel, exited: make(chan struct{}), stderr: stderr}
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

func (w *localWorker) Execute(ctx context.Context, job Job, sink Checkpoints) (Result, error) {
	if !w.reuse.CompareAndSwap(workerReusable, workerRunning) {
		return Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head}, errors.New("local worker is busy or not ready for another assignment")
	}
	var state httpExecutionState
	result, err := w.executor.execute(ctx, job, sink, &state)
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
	if closed || unavailable || cleanupFailed || state.admitted && (ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded)) {
		w.reuse.Store(workerRetired)
		closeErr := w.Close()
		// A received outcome remains the Job result; Close reports lifecycle errors separately.
		if !state.outcome {
			err = errors.Join(err, closeErr)
			if closed && state.admitted {
				err = errors.Join(err, errors.New("local worker stopped before a final outcome"))
			}
		}
	} else if state.outcome {
		w.reuse.Store(workerNeedsPull)
	} else {
		w.reuse.Store(workerReusable)
	}
	return result, err
}

// awaitReusable releases capacity only after the completed attempt's next pull
// is held by the same live child at the original endpoint.
func (w *localWorker) awaitReusable(ctx context.Context) (reuseErr error) {
	if !w.reuse.CompareAndSwap(workerNeedsPull, workerCheckingPull) {
		if w.unclean.Load() {
			return errors.New("local worker attempt cleanup failed; capacity is unavailable")
		}
		return errors.New("local worker has no completed attempt awaiting reuse")
	}
	defer func() {
		if reuseErr != nil {
			w.reuse.Store(workerRetired)
			reuseErr = errors.Join(reuseErr, w.Close())
		} else {
			w.reuse.Store(workerReusable)
		}
	}()
	if err := w.endpoint.checkIdentity(); err != nil {
		return fmt.Errorf("local worker reuse endpoint: %w", err)
	}
	if err := w.awaitHeldPull(ctx); err != nil {
		return fmt.Errorf("local worker reuse pull: %w", err)
	}
	if err := w.endpoint.checkIdentity(); err != nil {
		return fmt.Errorf("local worker reuse endpoint: %w", err)
	}
	return nil
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
			err := errors.Join(errors.New("local worker exited before holding a pull"), w.waitErr)
			if detail := w.startupDiagnostic(); detail != "" {
				err = errors.Join(err, fmt.Errorf("worker startup diagnostic: %s", detail))
			}
			return err
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

// Close drains accepted callbacks while HTTP serves, then stops and waits for
// the direct child once before the endpoint closes its owned socket.
func (w *localWorker) Close() error {
	w.closeOnce.Do(func() {
		coreErr := w.executor.Close()
		// A published exit before cancellation is spontaneous, including one
		// observed while accepted callbacks drain.
		w.childMu.Lock()
		exitedBeforeCancel := false
		select {
		case <-w.exited:
			exitedBeforeCancel = true
		default:
		}
		w.cancel()
		w.childMu.Unlock()
		<-w.exited
		// Cmd.Wait may have returned before cancellation but published afterward.
		// Keep every nonzero exit code and other unexpected Wait error.
		if w.waitErr != nil && (exitedBeforeCancel || !expectedLocalWorkerCancellation(w.waitErr, w.stopSent.Load())) {
			w.closeErr = fmt.Errorf("local worker exited: %w", w.waitErr)
		}
		w.closeErr = errors.Join(w.closeErr, coreErr, w.endpoint.Close())
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
