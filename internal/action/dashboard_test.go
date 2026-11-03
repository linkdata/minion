package action

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/state"
)

func dashboardStore(t *testing.T, at time.Time) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), at)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func dashboardAccepted(job Job, at time.Time) *state.AcceptedAction {
	id := job.Identity
	return &state.AcceptedAction{JobID: id.JobID, AttemptID: id.AttemptID, ExecutorID: id.ExecutorID,
		Repository: id.Repository, RepositoryID: id.RepositoryID, OwnerID: id.OwnerID,
		SubjectType: id.SubjectType, SubjectNumber: id.Number, SourceKey: "source", SourceNodeID: "source-node",
		Requester: id.Requester, RequesterID: id.RequesterID, CommandName: job.Command, Action: string(job.Action),
		AcceptedAt: at, Deadline: at.Add(10 * time.Minute), Assigned: true, ResultReady: true}
}

func TestDashboardSnapshotBindsStatusAndUsesCommittedState(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	job := testJob(command.Comment)
	store := dashboardStore(t, at.Add(-time.Minute))
	store.Data.Accepted = dashboardAccepted(job, at)
	store.Data.AcceptedTotal = 4
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	executor, err := NewHTTPExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	executor.pull, executor.pullDone = true, make(chan struct{})
	executor.status = Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, ProgressSequence: 1, ProgressAge: 7 * time.Second, WorkerState: "ready", Phase: "cleanup"}
	executor.statusAt = at.Add(-2 * time.Second)
	worker := &localWorker{executor: executor, endpoint: &LocalEndpoint{serveDone: make(chan struct{})}, exited: make(chan struct{})}
	worker.reuse.Store(workerReusable)
	owner := &Orchestrator{Store: store, MaxRuntime: time.Hour, InactivityTimeout: 3 * time.Minute}
	owner.setSelectedExecutor(worker)
	snapshot, err := owner.DashboardSnapshot(at)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TrackedAccepted != 4 || snapshot.InProgress != 1 || snapshot.Action == nil || snapshot.Action.Phase != "publishing" ||
		snapshot.Action.Remaining != 10*time.Minute || !snapshot.Worker.Ready || snapshot.Worker.Readiness != "ready" ||
		snapshot.Worker.State != "ready" || snapshot.Worker.Phase != "cleanup" ||
		!snapshot.Worker.LastContact.Equal(at.Add(-2*time.Second)) || !snapshot.Worker.LastProgress.Equal(at.Add(-9*time.Second)) ||
		snapshot.MaxRuntime != time.Hour || snapshot.InactivityTimeout != 3*time.Minute {
		t.Fatalf("wrong dashboard snapshot: %+v", snapshot)
	}
	store.Data.Accepted.Requester = "other-requester"
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if mismatched, err := owner.DashboardSnapshot(at); err != nil || !mismatched.Worker.LastContact.IsZero() || mismatched.Action == nil || mismatched.Action.Requester != "other-requester" {
		t.Fatalf("Status crossed requester identity: %+v, %v", mismatched, err)
	}
	store.Data.Accepted.Requester = job.Identity.Requester
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted = nil // A concurrent monitor mutation is not yet committed.
	if stillSaved, err := owner.DashboardSnapshot(at); err != nil || stillSaved.InProgress != 1 {
		t.Fatalf("unsaved mutation leaked into dashboard: %+v, %v", stillSaved, err)
	}
	store.Data.Accepted = dashboardAccepted(job, at)
	store.Data.Accepted.AttemptID = "later-attempt"
	store.Data.Accepted.ResultReady = false
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	stale, err := owner.DashboardSnapshot(at)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Worker.Ready != true || !stale.Worker.LastContact.IsZero() || !stale.Worker.LastProgress.IsZero() ||
		stale.Worker.State != "not observed" || stale.Action == nil || stale.Action.Phase != "assigned" {
		t.Fatalf("old attempt Status leaked into new action: %+v", stale)
	}
	store.Data.Accepted.Assigned = false
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if preparing, err := owner.DashboardSnapshot(at); err != nil || preparing.InProgress != 1 || preparing.Action == nil || preparing.Action.Phase != "preparing" {
		t.Fatalf("preparation was not in progress: %+v, %v", preparing, err)
	}
	store.Data.Accepted.Assigned, store.Data.Accepted.Terminal = true, true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if terminal, err := owner.DashboardSnapshot(at); err != nil || terminal.InProgress != 1 || terminal.Action == nil || terminal.Action.Phase != "terminal cleanup" {
		t.Fatalf("terminal cleanup was not in progress: %+v, %v", terminal, err)
	}
	store.Data.Accepted = nil
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	finished, err := owner.DashboardSnapshot(at)
	if err != nil || finished.InProgress != 0 || finished.TrackedAccepted != 4 || finished.Action != nil {
		t.Fatalf("completed action view: %+v, %v", finished, err)
	}
}

func TestDashboardWorkerReadinessRequiresFreshPullAndCleanup(t *testing.T) {
	executor, err := NewHTTPExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	executor.pull, executor.pullDone = true, make(chan struct{})
	local := &localWorker{executor: executor, endpoint: &LocalEndpoint{serveDone: make(chan struct{})}, exited: make(chan struct{})}
	local.reuse.Store(workerReusable)
	if observed := local.dashboardObservation(); !observed.ready || observed.readiness != "ready" {
		t.Fatalf("fresh local pull is not ready: %+v", observed)
	}
	local.reuse.Store(workerNeedsPull)
	if observed := local.dashboardObservation(); observed.ready {
		t.Fatalf("local cleanup without reuse proof became ready: %+v", observed)
	}
	local.reuse.Store(workerReusable)
	executor.pullStale = true
	if observed := local.dashboardObservation(); observed.ready {
		t.Fatalf("stale local pull became ready: %+v", observed)
	}
	executor.pullStale = false
	local.unclean.Store(true)
	if observed := local.dashboardObservation(); observed.ready || observed.readiness != "cleanup failed" {
		t.Fatalf("unclean local worker: %+v", observed)
	}
	local.unclean.Store(false)
	close(local.exited)
	if observed := local.dashboardObservation(); observed.ready || observed.readiness != "unreachable" {
		t.Fatalf("exited local worker: %+v", observed)
	}
	remote := &remoteWorker{executor: executor, endpoint: &RemoteEndpoint{serveDone: make(chan struct{})}}
	if observed := remote.dashboardObservation(); !observed.ready || observed.readiness != "ready" {
		t.Fatalf("fresh remote pull is not ready: %+v", observed)
	}
	executor.pullStale = true
	if observed := remote.dashboardObservation(); observed.ready {
		t.Fatalf("stale remote pull became ready: %+v", observed)
	}
	executor.pullStale = false
	remote.unclean.Store(true)
	if observed := remote.dashboardObservation(); observed.ready || observed.readiness != "cleanup failed" {
		t.Fatalf("unclean remote worker: %+v", observed)
	}
	remote.unclean.Store(false)
	close(remote.endpoint.serveDone)
	if observed := remote.dashboardObservation(); observed.ready || observed.readiness != "unreachable" {
		t.Fatalf("closed remote endpoint: %+v", observed)
	}
}
