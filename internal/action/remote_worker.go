package action

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"slices"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
)

// remoteWorker binds one authenticated HTTPS endpoint to the selected executor.
type remoteWorker struct {
	executor *HTTPExecutor
	endpoint *RemoteEndpoint
}

// AttachRemoteWorker starts the configured listener. Capacity becomes available
// only while an authenticated worker holds a pull at that listener.
func (e *Orchestrator) AttachRemoteWorker(cfg config.Config, certificate tls.Certificate) (map[string]bool, *RemoteEndpoint, error) {
	if e.Executor != nil || cfg.RemoteExecutor == nil {
		return nil, nil, errors.New("remote executor is already selected or not configured")
	}
	if cmp.Or(cfg.Main, "codex") != e.mainProvider() || cfg.Adversarial != e.Adversarial {
		return nil, nil, errors.New("remote executor provider selection differs from config")
	}
	commands := make(map[string]bool)
	for _, repo := range cfg.Repositories {
		for _, name := range repo.Actions {
			configured, ok := cfg.Commands[name]
			used, present := e.Commands[name]
			if !ok || !present || configured.Action != used.Action || !slices.Equal(configured.Skills, used.Skills) {
				return nil, nil, fmt.Errorf("remote command %q differs from orchestrator mapping", name)
			}
			if len(configured.Skills) != 0 && configured.SupportsProvider(e.mainProvider()) {
				commands[name] = true
			}
		}
	}
	if len(commands) == 0 {
		return nil, nil, errors.New("remote executor has no enabled commands")
	}
	remote := cfg.RemoteExecutor
	executor, err := NewHTTPExecutor(remote.ExecutorID)
	if err != nil {
		return nil, nil, err
	}
	credentials := WorkerCredentials{}
	uses := func(provider string) bool {
		return e.mainProvider() == provider || e.Adversarial == provider
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
	if err := executor.SetWorkerCredentials(credentials); err != nil {
		return nil, nil, err
	}
	endpoint, err := ListenRemoteExecution(executor, remote.Listen, certificate, remote.Token)
	if err != nil {
		return nil, nil, err
	}
	e.Executor = &remoteWorker{executor: executor, endpoint: endpoint}
	e.ExecutorID = remote.ExecutorID
	e.remoteSelected = true
	e.assignmentRequired = true
	return commands, endpoint, nil
}

// WaitCapacity checks the live held pull, not a prior worker announcement.
func (w *remoteWorker) WaitCapacity(ctx context.Context) error {
	for {
		w.executor.mu.Lock()
		pull, pullDone, active := w.executor.pull, w.executor.pullDone, w.executor.active
		closed, unavailable, changed := w.executor.closed, w.executor.unavailable, w.executor.changed
		w.executor.mu.Unlock()
		select {
		case <-w.endpoint.serveDone:
			return errors.Join(ErrRemoteWorkerUnavailable, w.endpoint.serveErr)
		default:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if closed || unavailable {
			return ErrRemoteWorkerUnavailable
		}
		if active == nil && pull && pullDone != nil {
			select {
			case <-pullDone:
			default:
				return nil
			}
		}
		select {
		case <-w.endpoint.serveDone:
		case <-changed:
		case <-ctx.Done():
		}
	}
}

func (w *remoteWorker) heldPull() <-chan struct{} {
	select {
	case <-w.endpoint.serveDone:
		return nil
	default:
	}
	w.executor.mu.Lock()
	defer w.executor.mu.Unlock()
	if !w.executor.pull || w.executor.pullDone == nil || w.executor.closed || w.executor.unavailable || w.executor.active != nil {
		return nil
	}
	select {
	case <-w.executor.pullDone:
		return nil
	default:
		return w.executor.pullDone
	}
}

func (w *remoteWorker) Execute(ctx context.Context, job Job, sink Checkpoints) (Result, error) {
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head}
	checkpoint, hasCheckpoint := sink.(assignmentCheckpoint)
	hasCheckpoint = hasCheckpoint && checkpoint.AssignmentRequired()
	pullDone := w.heldPull()
	if pullDone == nil {
		err := fmt.Errorf("%w: remote worker has no live pull", monitor.ErrRequestTransport)
		if hasCheckpoint {
			err = errors.Join(err, checkpoint.AssignmentNotDelivered())
		}
		return result, err
	}
	state := httpExecutionState{pullDone: pullDone}
	if hasCheckpoint {
		state.beforeDelivery = checkpoint.BeforeAssignment
	}
	result, err := w.executor.execute(ctx, 0, job, sink, &state)
	if hasCheckpoint && !state.delivered {
		err = errors.Join(err, checkpoint.AssignmentNotDelivered())
	}
	w.executor.mu.Lock()
	closed, unavailable := w.executor.closed, w.executor.unavailable
	w.executor.mu.Unlock()
	select {
	case <-w.endpoint.serveDone:
		closed = true
	default:
	}
	if !state.delivered && (closed || unavailable) {
		err = errors.Join(err, fmt.Errorf("%w: remote endpoint closed before assignment", monitor.ErrRequestTransport))
	}
	cleanupFailed := false
	for _, diagnostic := range result.Diagnostics {
		cleanupFailed = cleanupFailed || diagnostic.Code == "cleanup_failed"
	}
	if closed || unavailable || cleanupFailed {
		closeErr := w.endpoint.Close()
		if !state.outcome {
			err = errors.Join(err, closeErr)
		}
	}
	return result, err
}
