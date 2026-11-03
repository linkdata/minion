package action

import (
	"cmp"
	"errors"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
)

// DashboardSnapshot exposes only the current action and worker observations.
// TrackedAccepted counts acceptances saved since the counter was introduced.
type DashboardSnapshot struct {
	TrackedAccepted   uint64
	InProgress        int
	Action            *DashboardAction
	LastStop          *DashboardStop
	Worker            DashboardWorker
	MaxRuntime        time.Duration
	InactivityTimeout time.Duration
}

type DashboardAction struct {
	JobID, AttemptID              string
	Repository, Requester, Action string
	AcceptedAt, Deadline          time.Time
	Phase                         string
	Stoppable                     bool
}

type DashboardStop struct {
	JobID, AttemptID, RequestedBy, Status string
	RequestedAt                           time.Time
}

type DashboardWorker struct {
	Kind, Readiness, State string
	Phase, ReviewPhase     string
	Round                  int
	LastContact            time.Time
	ReportedProgressAge    time.Duration
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
	data, err := readDashboardState(e.Store.ReadSavedDurable, runtime.GOOS == "windows")
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
	if data.LastStopJobID != "" {
		var request state.StopRequest
		var attemptID string
		if accepted != nil && accepted.JobID == data.LastStopJobID && accepted.StopRequest != (state.StopRequest{}) {
			request, attemptID = accepted.StopRequest, accepted.AttemptID
		} else if decision := data.TerminalDecisions[data.LastStopJobID]; decision.StopRequest != (state.StopRequest{}) {
			request, attemptID = decision.StopRequest, decision.AttemptID
		}
		if request != (state.StopRequest{}) {
			snapshot.LastStop = &DashboardStop{JobID: data.LastStopJobID, AttemptID: attemptID,
				RequestedBy: request.RequestedBy, RequestedAt: request.RequestedAt, Status: request.Status}
		}
	}
	if accepted == nil {
		return snapshot, nil
	}
	decision, decided := data.TerminalDecisions[accepted.JobID]
	matchingDecision := decided && decision.AttemptID == accepted.AttemptID
	if observation.kind == "remote" && snapshot.Worker.Readiness == "ready" {
		// A repull can arrive before the accepted action is durably released.
		switch {
		case accepted.ResultReady || accepted.Terminal || matchingDecision:
			snapshot.Worker.Readiness = "waiting for accepted cleanup"
		case accepted.Assigned:
			snapshot.Worker.Readiness = "assigned action in progress"
		}
	}
	snapshot.InProgress = 1
	action := &DashboardAction{JobID: accepted.JobID, AttemptID: accepted.AttemptID,
		Repository: accepted.Repository, Requester: accepted.Requester, Action: accepted.Action,
		AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
		Phase: "preparing", Stoppable: !accepted.Terminal && e.Store.SelectedStopActive(accepted.JobID, accepted.AttemptID)}
	if _, decided := data.TerminalDecisions[accepted.JobID]; decided {
		action.Stoppable = false
	}
	if accepted.Assigned {
		action.Phase = "assigned"
	}
	if accepted.ResultReady {
		action.Phase = "publishing"
	}
	if accepted.Terminal {
		action.Phase = "terminal cleanup"
	}
	if matchingDecision {
		action.Phase = "terminal cleanup: " + decision.Outcome
	}
	retryPending := accepted.RetryAt.After(now) && !accepted.Terminal && !matchingDecision
	if retryPending {
		stage := "action"
		if accepted.ResultReady {
			stage = "publication"
		}
		action.Phase = stage + " deferred until " + accepted.RetryAt.UTC().Format(time.RFC3339)
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
	snapshot.Worker.ReportedProgressAge = status.ProgressAge
	if !accepted.ResultReady && !accepted.Terminal && !retryPending && status.Phase != "" {
		action.Phase = status.Phase
	}
	return snapshot, nil
}

// A Windows replacement can briefly deny opening state.json. Keep the retry
// specific to dashboard reads; durable ACK reads retain their fail-fast behavior.
func readDashboardState(read func() (state.Data, error), windows bool) (state.Data, error) {
	for attempt := 0; ; attempt++ {
		data, err := read()
		if err == nil || !windows || attempt == 2 ||
			!(errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33))) {
			return data, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}
