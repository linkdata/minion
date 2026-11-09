package action

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
)

// remoteWorker binds one authenticated HTTPS endpoint to the selected executor.
type remoteWorker struct {
	executor *HTTPExecutor
	endpoint *RemoteEndpoint
	unclean  atomic.Bool
}

// AttachRemoteWorker starts the configured listener. Capacity becomes available
// only while an authenticated worker holds a pull at that listener.
func (e *Orchestrator) AttachRemoteWorker(cfg config.Config, certificate tls.Certificate) (map[string]bool, *RemoteEndpoint, error) {
	if err := CheckNativeRolePair(e.mainProvider(), e.Adversarial); err != nil {
		return nil, nil, err
	}
	if e.Executor != nil || cfg.RemoteExecutor == nil {
		return nil, nil, errors.New("remote executor is already selected or not configured")
	}
	if cmp.Or(cfg.Main, "codex") != e.mainProvider() || cfg.Adversarial != e.Adversarial {
		return nil, nil, errors.New("remote executor provider selection differs from config")
	}
	if err := e.matchConfiguredRoleModels(cfg); err != nil {
		return nil, nil, err
	}
	commands := make(map[string]bool)
	for _, repo := range cfg.Repositories {
		for _, name := range repo.Actions {
			configured, ok := cfg.Commands[name]
			used, present := e.Commands[name]
			if !ok || !present || configured.Action != used.Action || !slices.Equal(configured.Skills, used.Skills) {
				return nil, nil, fmt.Errorf("remote command %q differs from orchestrator mapping", name)
			}
			if len(configured.Skills) != 0 && configured.SupportsProvider(e.mainProvider()) && configured.SupportedOnHost() {
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
	if e.WorkRoot != "" {
		if err := executor.SetWorkRoot(e.WorkRoot); err != nil {
			return nil, nil, err
		}
	}
	if err := executor.SetWorkerCredentials(e.workerCredentials(e.mainProvider(), e.Adversarial)); err != nil {
		return nil, nil, err
	}
	selection := remoteChoicePreflight{Version: ProtocolVersion, ExecutorID: remote.ExecutorID,
		Main: e.mainRoleSettings(nil), MainChoices: slices.Clone(cfg.UserChoices(false)), CriticChoices: slices.Clone(cfg.UserChoices(true))}
	selection.Main.Skills, selection.Main.AlwaysOnSkills = nil, nil
	if e.Adversarial != "" {
		critic := e.criticRoleSettings()
		critic.Skills, critic.AlwaysOnSkills = nil, nil
		selection.Critic = &critic
	}
	if err := validateRemoteChoices(selection); err != nil {
		return nil, nil, err
	}
	executor.remoteChoices = &selection
	if e.remotePriorAttempts == nil {
		prior := make(map[string]string)
		if e.Store != nil {
			saved, err := e.Store.ReadSavedDurable()
			if err != nil {
				return nil, nil, fmt.Errorf("read terminal decisions before remote attach: %w", err)
			}
			for jobID, decision := range saved.TerminalDecisions {
				prior[jobID] = decision.AttemptID
			}
			if accepted := saved.Accepted; accepted != nil && accepted.Assigned {
				if _, decided := prior[accepted.JobID]; !decided {
					prior[accepted.JobID] = accepted.AttemptID
				}
			}
		}
		e.remotePriorAttempts = prior
	}
	executor.lateOutcome = func(outcome httpOutcome) error { return e.receiveLateOutcome(remote.ExecutorID, outcome) }
	endpoint, err := ListenRemoteExecution(executor, remote.Listen, certificate, remote.Token)
	if err != nil {
		return nil, nil, err
	}
	w := &remoteWorker{executor: executor, endpoint: endpoint}
	e.setSelectedExecutor(w)
	e.ExecutorID = remote.ExecutorID
	e.remoteSelected = true
	e.assignmentRequired = true
	return commands, endpoint, nil
}

// UpdateRemoteWorkerCredentials validates changed credentials before the next assignment.
func (e *Orchestrator) UpdateRemoteWorkerCredentials(ctx context.Context) error {
	worker, ok := e.Executor.(*remoteWorker)
	if !ok {
		return errors.New("remote worker is not selected")
	}
	return worker.executor.updateIdleWorkerCredentials(ctx, e.workerCredentials(e.mainProvider(), e.Adversarial))
}

// WaitCapacity checks the live held pull, not a prior worker announcement.
func (w *remoteWorker) WaitCapacity(ctx context.Context) error {
	for {
		w.executor.mu.Lock()
		pull, pullDone, stale, active := w.executor.pull, w.executor.pullDone, w.executor.pullStale, w.executor.active
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
		if active == nil && pull && !stale && pullDone != nil {
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
	if !w.executor.pull || w.executor.pullDone == nil || w.executor.pullStale || w.executor.closed || w.executor.unavailable || w.executor.active != nil {
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
	if job.Action == command.SecurityAudit && !w.endpoint.AuditHostSupported() {
		err := errors.Join(ErrRemoteWorkerUnavailable, fmt.Errorf("%w: audit unsupported on remote host", monitor.ErrRequestTransport))
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
	if cleanupFailed {
		w.unclean.Store(true)
	}
	if closed || unavailable || cleanupFailed {
		closeErr := w.endpoint.Close()
		if !state.outcome {
			err = errors.Join(err, closeErr)
		}
	}
	return result, err
}

func (w *remoteWorker) stopCleanupVerified() bool {
	if w.unclean.Load() {
		return false
	}
	w.executor.mu.Lock()
	unavailable, active := w.executor.unavailable, w.executor.active != nil
	w.executor.mu.Unlock()
	if unavailable || active {
		return false
	}
	select {
	case <-w.endpoint.serveDone:
		return false
	default:
		return true
	}
}
