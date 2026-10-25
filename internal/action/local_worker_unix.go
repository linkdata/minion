//go:build !windows

package action

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/executil"
	"github.com/linkdata/minion/internal/monitor"
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
	stderr    *bounded.Tail
	secrets   []string
	waitErr   error
	closeOnce sync.Once
	closeErr  error
}

type lockedWriter struct {
	mu     *sync.Mutex
	writer io.Writer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
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
	// stdout and stderr may share one caller-owned writer.
	outputMu := &sync.Mutex{}
	if cmd.Stdout != nil {
		cmd.Stdout = lockedWriter{outputMu, cmd.Stdout}
	}
	stderr := &bounded.Tail{}
	if cmd.Stderr == nil {
		cmd.Stderr = stderr
	} else {
		cmd.Stderr = io.MultiWriter(stderr, lockedWriter{outputMu, cmd.Stderr})
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Cancel = commandCancel
	executil.Prepare(cmd)
	w := &localWorker{executor: executor, endpoint: endpoint, cmd: cmd, cancel: cancel, exited: make(chan struct{}), stderr: stderr, secrets: workerCredentialValues(env)}
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
			if raw := w.stderr.String(); raw != "" {
				if w.stderr.Truncated {
					if end := strings.IndexByte(raw, '\n'); end >= 0 {
						raw = raw[end+1:]
					} else {
						raw = ""
					}
				}
				raw = strings.TrimSpace(raw)
				detail := redactActionText(raw, w.secrets...)
				if detail == "" {
					detail = "worker startup output could not be safely displayed"
				}
				err = errors.Join(err, fmt.Errorf("worker startup diagnostic: %s", monitor.InertLogText(detail)))
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
