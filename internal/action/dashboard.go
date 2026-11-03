package action

import (
	"cmp"
	"errors"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/config"
)

// DashboardSnapshot exposes only the current action and worker observations.
// TrackedAccepted counts acceptances saved since the counter was introduced.
type DashboardSnapshot struct {
	TrackedAccepted   uint64
	InProgress        int
	Action            *DashboardAction
	Worker            DashboardWorker
	MaxRuntime        time.Duration
	InactivityTimeout time.Duration
}

type DashboardAction struct {
	Repository, Requester, Action string
	AcceptedAt, Deadline          time.Time
	Remaining                     time.Duration
	Phase                         string
}

type DashboardWorker struct {
	Kind, Readiness, State string
	Phase, ReviewPhase     string
	Round                  int
	LastContact            time.Time
	LastProgress           time.Time
}

type dashboardWorkerSource interface{ dashboardObservation() workerObservation }

func (e *Orchestrator) setSelectedExecutor(worker Executor) {
	if e.Store == nil {
		e.Executor = worker
		return
	}
	_ = e.Store.WithLock(func() error {
		e.Executor = worker
		return nil
	})
}

type workerObservation struct {
	kind, readiness string
	status          Status
	statusAt        time.Time
	active          bool
	heldPull        bool
	closed          bool
	unavailable     bool
}

func (e *HTTPExecutor) dashboardObservation() workerObservation {
	e.mu.Lock()
	defer e.mu.Unlock()
	observation := workerObservation{status: e.status, statusAt: e.statusAt, active: e.active != nil,
		closed: e.closed, unavailable: e.unavailable}
	if e.pull && !e.pullStale && e.pullDone != nil && e.active == nil {
		select {
		case <-e.pullDone:
		default:
			observation.heldPull = true
		}
	}
	return observation
}

func (w *localWorker) dashboardObservation() workerObservation {
	observation := w.executor.dashboardObservation()
	observation.kind = "local"
	if w.unclean.Load() {
		observation.readiness = "cleanup failed"
		return observation
	}
	select {
	case <-w.exited:
		observation.readiness = "unreachable"
		return observation
	default:
	}
	select {
	case <-w.endpoint.serveDone:
		observation.readiness = "unreachable"
		return observation
	default:
	}
	reuse := w.reuse.Load()
	switch {
	case observation.closed || observation.unavailable || reuse == workerRetired:
		observation.readiness = "unreachable"
	case reuse == workerReusable && observation.heldPull:
		observation.readiness = "ready"
	case observation.active || reuse == workerRunning:
		observation.readiness = "busy"
	default:
		observation.readiness = "waiting for clean pull"
	}
	return observation
}

func (w *remoteWorker) dashboardObservation() workerObservation {
	observation := w.executor.dashboardObservation()
	observation.kind = "remote"
	if w.unclean.Load() {
		observation.readiness = "cleanup failed"
		return observation
	}
	select {
	case <-w.endpoint.serveDone:
		observation.readiness = "unreachable"
		return observation
	default:
	}
	switch {
	case observation.closed || observation.unavailable:
		observation.readiness = "unreachable"
	case observation.active:
		observation.readiness = "busy"
	case observation.heldPull:
		observation.readiness = "ready"
	default:
		observation.readiness = "waiting for worker"
	}
	return observation
}

// DashboardSnapshot joins a committed state file with a live worker observation.
// A Status is displayed only for the action saved in that state file.
func (e *Orchestrator) DashboardSnapshot(now time.Time) (DashboardSnapshot, error) {
	if e == nil || e.Store == nil {
		return DashboardSnapshot{}, errors.New("dashboard state is unavailable")
	}
	data, err := e.Store.ReadSaved()
	if err != nil {
		return DashboardSnapshot{}, err
	}
	snapshot := DashboardSnapshot{TrackedAccepted: data.AcceptedTotal,
		MaxRuntime:        cmp.Or(e.MaxRuntime, config.DefaultMaxRuntime),
		InactivityTimeout: cmp.Or(e.InactivityTimeout, config.DefaultInactivityTimeout),
		Worker:            DashboardWorker{Readiness: "starting", State: "not observed"}}
	var selected Executor
	_ = e.Store.WithLock(func() error {
		selected = e.Executor
		return nil
	})
	var observation workerObservation
	if worker, ok := selected.(dashboardWorkerSource); ok {
		observation = worker.dashboardObservation()
		snapshot.Worker.Kind = observation.kind
		snapshot.Worker.Readiness = observation.readiness
	}
	accepted := data.Accepted
	if accepted == nil {
		return snapshot, nil
	}
	snapshot.InProgress = 1
	remaining := accepted.Deadline.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	action := &DashboardAction{Repository: accepted.Repository, Requester: accepted.Requester, Action: accepted.Action,
		AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline, Remaining: remaining,
		Phase: "preparing"}
	if accepted.Assigned {
		action.Phase = "assigned"
	}
	if accepted.ResultReady {
		action.Phase = "publishing"
	}
	if accepted.Terminal {
		action.Phase = "terminal cleanup"
	}
	if decision, ok := data.TerminalDecisions[accepted.JobID]; ok && decision.AttemptID == accepted.AttemptID {
		action.Phase = "terminal cleanup: " + decision.Outcome
	}
	snapshot.Action = action
	status := observation.status
	if observation.statusAt.IsZero() || status.Identity.JobID != accepted.JobID ||
		status.Identity.AttemptID != accepted.AttemptID || status.Identity.ExecutorID != accepted.ExecutorID ||
		status.Identity.Repository != accepted.Repository || status.Identity.RepositoryID != accepted.RepositoryID ||
		status.Identity.OwnerID != accepted.OwnerID || !strings.EqualFold(status.Identity.Requester, accepted.Requester) ||
		status.Identity.RequesterID != accepted.RequesterID || status.Identity.SubjectType != accepted.SubjectType ||
		status.Identity.Number != accepted.SubjectNumber {
		return snapshot, nil
	}
	snapshot.Worker.State = status.WorkerState
	snapshot.Worker.Phase = status.Phase
	snapshot.Worker.Round = status.Round
	snapshot.Worker.ReviewPhase = status.ReviewPhase
	snapshot.Worker.LastContact = observation.statusAt
	snapshot.Worker.LastProgress = observation.statusAt.Add(-status.ProgressAge)
	if !accepted.ResultReady && !accepted.Terminal {
		action.Phase = status.Phase
	}
	return snapshot, nil
}
