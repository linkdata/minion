//go:build !windows

package action

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"

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
	waitErr   error
	closeOnce sync.Once
	closeErr  error
}

// startLocalWorker owns the direct child's context and command. configure sets
// environment and stdio; job cancellation is handled by Execute.
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
	cmd.Cancel = commandCancel
	executil.Prepare(cmd)
	w := &localWorker{executor: executor, endpoint: endpoint, cmd: cmd, cancel: cancel, exited: make(chan struct{})}
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
	var state httpExecutionState
	result, err := w.executor.execute(ctx, job, sink, &state)
	w.executor.mu.Lock()
	closed, unavailable := w.executor.closed, w.executor.unavailable
	w.executor.mu.Unlock()
	if closed || unavailable || state.admitted && (ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded)) {
		closeErr := w.Close()
		// A received outcome remains the Job result; Close reports lifecycle errors separately.
		if !state.outcome {
			err = errors.Join(err, closeErr)
			if closed && state.admitted {
				err = errors.Join(err, errors.New("local worker stopped before a final outcome"))
			}
		}
	}
	return result, err
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
