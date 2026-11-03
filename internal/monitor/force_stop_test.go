package monitor

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type stopCleanupHandler struct {
	fakeHandler
	clean bool
}

func (h *stopCleanupHandler) StopCleanupVerified() bool { return h.clean }

type cleanRecoveredStopHandler struct {
	fakeHandler
	entered chan struct{}
}

func (*cleanRecoveredStopHandler) StopCleanupVerified() bool { return true }

func (h *cleanRecoveredStopHandler) PublishRecovered(ctx context.Context, _ Job) error {
	close(h.entered)
	<-ctx.Done()
	return ctx.Err()
}

type heldStopReactionGitHub struct {
	*fakeGitHub
	entered chan struct{}
	once    sync.Once
}

func (g *heldStopReactionGitHub) ReactEyes(ctx context.Context, _ string) error {
	g.once.Do(func() { close(g.entered) })
	<-ctx.Done()
	return nil // The remote reaction can succeed as cancellation arrives.
}

func TestForceStopDuringAcceptanceReactionNeverDispatches(t *testing.T) {
	at := time.Now().UTC()
	store := openStateAt(t, filepath.Join(t.TempDir(), "state.json"), at.Add(-time.Minute))
	gh := &heldStopReactionGitHub{fakeGitHub: &fakeGitHub{
		notifications: []github.Notification{testNotification(at)}, items: []github.Item{selectedItem(at)},
		permission: github.Permission{Permission: "write"},
	}, entered: make(chan struct{})}
	handler := &fakeHandler{}
	m := selectedMonitor(t, at, gh, handler)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := m.RunOnce(ctx, store); finished <- err }()
	select {
	case <-gh.entered:
	case <-ctx.Done():
		t.Fatal("accepted reaction did not start")
	}
	var selected state.AcceptedAction
	_ = store.WithLock(func() error { selected = *store.Data.Accepted; return nil })
	if !store.SelectedStopActive(selected.JobID, selected.AttemptID) {
		t.Fatal("accepted preparation was not stoppable")
	}
	if _, err := store.RequestStop(selected.JobID, "stale-attempt", "operator", time.Now().UTC()); !errors.Is(err, state.ErrStopConflict) {
		t.Fatalf("stale preparation stop = %v", err)
	}
	request, err := store.RequestStop(selected.JobID, selected.AttemptID, "operator", time.Now().UTC())
	if err != nil || request.Status != "requested" {
		t.Fatalf("preparation stop = %+v, %v", request, err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("stopped preparation: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("stopped preparation did not finish")
	}
	decision := store.Data.TerminalDecisions[selected.JobID]
	if decision.Outcome != "canceled" || decision.StopRequest.Status != "confirmed" ||
		store.Data.Accepted != nil || store.Data.Processed[selected.JobID].IsZero() ||
		len(handler.jobs) != 0 || gh.commentCalls != 0 {
		t.Fatalf("preparation stop dispatched or failed cleanup: decision=%+v state=%+v jobs=%d comments=%d",
			decision, store.Data, len(handler.jobs), gh.commentCalls)
	}
}

func TestForceStopDuringRecoveryCapacityNeverDispatches(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	store.Data.NextInviteCheckAt = time.Now().Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	handler := &heldCapacityHandler{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(handler.release)
	m.Handler = handler
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- m.recoverAccepted(ctx, store) }()
	select {
	case <-handler.entered:
	case <-ctx.Done():
		t.Fatal("recovery capacity wait did not start")
	}
	request, err := store.RequestStop(job.ID, job.AttemptID, "operator", time.Now().UTC())
	if err != nil || request.Status != "requested" {
		t.Fatalf("recovery stop = %+v, %v", request, err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("stopped recovery: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("stopped recovery did not finish")
	}
	decision := store.Data.TerminalDecisions[job.ID]
	if decision.Outcome != "canceled" || decision.StopRequest.Status != "confirmed" ||
		store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() ||
		len(handler.jobs) != 0 || handler.cancels.Load() != 1 || gh.commentCalls != 0 {
		t.Fatalf("recovery stop dispatched or failed cleanup: decision=%+v state=%+v jobs=%d cancellations=%d comments=%d",
			decision, store.Data, len(handler.jobs), handler.cancels.Load(), gh.commentCalls)
	}
}

func TestExpiredSelectedHandoffOrdersStopAndFinish(t *testing.T) {
	expired := func(t *testing.T) (*Monitor, *state.Store, Job) {
		t.Helper()
		m, store, job, _, _ := assignmentValidationFixture(t)
		job.AcceptedAt = time.Now().UTC().Add(-2 * time.Minute)
		job.Deadline = job.AcceptedAt.Add(time.Minute)
		store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = job.AcceptedAt, job.Deadline
		failure := store.Data.JobFailures[job.ID]
		failure.AcceptedAt, failure.Deadline = job.AcceptedAt, job.Deadline
		store.Data.JobFailures[job.ID] = failure
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		return m, store, job
	}
	t.Run("release before request", func(t *testing.T) {
		m, store, job := expired(t)
		unregisterPrep := store.RegisterSelectedStop(job.ID, job.AttemptID, func() {})
		defer unregisterPrep()
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		finished := make(chan error, 1)
		go func() {
			finished <- m.runSelectedWithControls(t.Context(), store, job, "", nil,
				func(context.Context, Job) error { t.Error("expired job dispatched"); return nil },
				func(err error) error {
					close(entered)
					<-release
					return m.completeSelected(t.Context(), store, job, time.Now().UTC(), err)
				})
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("expired finish did not start")
		}
		if _, err := store.RequestStop(job.ID, job.AttemptID, "operator", time.Now().UTC()); !errors.Is(err, state.ErrStopConflict) {
			t.Fatalf("first stop after finish began = %v, want conflict", err)
		}
		unblock()
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("expired finish did not complete")
		}
		if decision := store.Data.TerminalDecisions[job.ID]; decision.Outcome != "expired" || decision.StopRequest != (state.StopRequest{}) {
			t.Fatalf("late stop changed expired decision: %+v", decision)
		}
	})
	t.Run("request before transfer", func(t *testing.T) {
		m, store, job := expired(t)
		unregisterPrep := store.RegisterSelectedStop(job.ID, job.AttemptID, func() {})
		defer unregisterPrep()
		if _, err := store.RequestStop(job.ID, job.AttemptID, "operator", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := m.runSelectedWithControls(t.Context(), store, job, "", nil,
			func(context.Context, Job) error { t.Error("stopped expired job dispatched"); return nil },
			func(err error) error { return m.completeSelected(t.Context(), store, job, time.Now().UTC(), err) }); err != nil {
			t.Fatal(err)
		}
		if decision := store.Data.TerminalDecisions[job.ID]; decision.Outcome != "canceled" || decision.StopRequest.Status != "confirmed" {
			t.Fatalf("early stop did not resolve: %+v", decision)
		}
	})
}

func TestForceStopDuringRecoveredPublicationCannotConfirmOldWorker(t *testing.T) {
	store, path, accepted, _, m := storedRecoveryFixture(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	handler := &cleanRecoveredStopHandler{entered: make(chan struct{})}
	m.Handler = handler
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := m.RunOnce(ctx, reopened)
		finished <- err
	}()
	select {
	case <-handler.entered:
	case <-ctx.Done():
		t.Fatal("saved publication did not start")
	}
	if _, err := reopened.RequestStop(accepted.JobID, accepted.AttemptID, "operator", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("saved publication stop: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("saved publication did not stop")
	}
	decision := reopened.Data.TerminalDecisions[accepted.JobID]
	if decision.StopRequest.Status != "unverified" {
		t.Fatalf("new idle executor confirmed old worker cleanup: %+v", decision)
	}
}

func TestForceStopFencesPublicationAndWaitsForCleanupEvidence(t *testing.T) {
	for _, test := range []struct {
		name, wantStatus string
		clean, hook      bool
		workerErr        error
	}{
		{name: "clean cancellation", wantStatus: "confirmed", clean: true, hook: true, workerErr: context.Canceled},
		{name: "unknown outcome", wantStatus: "unverified", clean: true, hook: true, workerErr: errors.Join(context.Canceled, ErrExecutionOutcomeUnknown)},
		{name: "unclean worker", wantStatus: "unverified", clean: false, hook: true, workerErr: context.Canceled},
		{name: "missing cleanup hook", wantStatus: "unverified", workerErr: context.Canceled},
		{name: "completed at stop", wantStatus: "confirmed", clean: true, hook: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, store, job, gh, _ := assignmentValidationFixture(t)
			if test.hook {
				m.Handler = &stopCleanupHandler{clean: test.clean}
			} else {
				m.Handler = &fakeHandler{}
			}
			store.Data.Accepted.Assigned = true
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			finished := make(chan error, 1)
			go func() {
				finished <- m.runSelectedWithControls(t.Context(), store, job, "", nil,
					func(ctx context.Context, _ Job) error {
						close(started)
						<-ctx.Done()
						<-release
						return test.workerErr
					}, func(err error) error { return m.completeSelected(t.Context(), store, job, time.Now().UTC(), err) })
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("selected worker did not start")
			}
			request, err := store.RequestStop(job.ID, job.AttemptID, "operator", time.Now().UTC())
			if err != nil || request.Status != "requested" {
				t.Fatalf("request: %+v, %v", request, err)
			}
			var got string
			_ = store.WithLock(func() error { got = store.Data.Accepted.StopRequest.Status; return nil })
			if got != "requested" {
				t.Fatalf("termination was confirmed before worker returned: %s", got)
			}
			if err := m.RevalidatePublication(t.Context(), store, job, true); !errors.Is(err, ErrTerminalJob) {
				t.Fatalf("publication passed force-stop fence: error=%v", err)
			}
			if err := m.RevalidateAssignment(t.Context(), store, job); !errors.Is(err, ErrTerminalJob) || errors.Is(err, ErrAcceptanceState) {
				t.Fatalf("assignment passed force-stop fence: %v", err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				var decision state.TerminalDecision
				_ = store.WithLock(func() error { decision = store.Data.TerminalDecisions[job.ID]; return nil })
				if decision.Outcome != "" {
					if decision.Outcome != "canceled" || decision.StopRequest.Status != "requested" {
						t.Fatalf("early terminal decision falsely confirmed cleanup: %+v", decision)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("stop did not persist the early terminal decision")
				}
				time.Sleep(time.Millisecond)
			}
			close(release)
			select {
			case err := <-finished:
				if test.name == "clean cancellation" && err != nil {
					t.Fatalf("clean operator stop returned a poll error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("selected stop did not finish")
			}
			decision := store.Data.TerminalDecisions[job.ID]
			if decision.Outcome != "canceled" || decision.StopRequest.Status != test.wantStatus ||
				decision.StopRequest.RequestedBy != "operator" || store.Data.Accepted != nil ||
				store.Data.Processed[job.ID].IsZero() || gh.commentCalls != 0 {
				t.Fatalf("incorrect stop cleanup: decision=%+v state=%+v comments=%d", decision, store.Data, gh.commentCalls)
			}
		})
	}
}

func TestForceStopDuringAssignmentValidationKeepsMonitorRunning(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	store.Data.NextInviteCheckAt = time.Now().Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	gh.beforeRepository = func() { close(entered); <-release }
	m.Handler = selectedHandler(func(Job) error { return m.RevalidateAssignment(t.Context(), store, job) })
	finished := make(chan error, 1)
	go func() { finished <- m.executeSelected(t.Context(), store, job, time.Now().UTC()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("assignment validation did not start")
	}
	if _, err := store.RequestStop(job.ID, job.AttemptID, "operator", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("valid stop exited monitor: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stopped assignment did not finish")
	}
	if decision := store.Data.TerminalDecisions[job.ID]; decision.Outcome != "canceled" ||
		decision.StopRequest.Status != "confirmed" || store.Data.Accepted != nil ||
		store.Data.Processed[job.ID].IsZero() {
		t.Fatalf("stopped assignment was not cleaned up: %+v state=%+v", decision, store.Data)
	}
	gh.beforeRepository = nil
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatalf("monitor could not continue after valid stop: %v", err)
	}
}

func TestRecoverRequestedStopNeverDispatches(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		t.Run(map[bool]string{false: "unassigned", true: "assigned"}[assigned], func(t *testing.T) {
			m, store, job, gh, _ := assignmentValidationFixture(t)
			handler := &fakeHandler{}
			m.Handler = handler
			store.Data.Accepted.Assigned = assigned
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			liveCtx, liveCancel := context.WithCancel(t.Context())
			unregister := store.RegisterSelectedStop(job.ID, job.AttemptID, liveCancel)
			if _, err := store.RequestStop(job.ID, job.AttemptID, "operator", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if liveCtx.Err() != context.Canceled {
				t.Fatal("live selected run did not receive stop")
			}
			unregister()
			if err := m.recoverAccepted(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			decision := store.Data.TerminalDecisions[job.ID]
			wantStatus, wantOutcome := "confirmed", "canceled"
			if assigned {
				wantStatus, wantOutcome = "unverified", "lost"
			}
			if decision.StopRequest.Status != wantStatus || decision.Outcome != wantOutcome ||
				store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() || len(handler.jobs) != 0 || gh.commentCalls != 0 {
				t.Fatalf("recovery dispatched/stopped incorrectly: decision=%+v state=%+v jobs=%d comments=%d", decision, store.Data, len(handler.jobs), gh.commentCalls)
			}
		})
	}
}

func TestRecoverConfirmedAssignedStopBeforeTerminalDecision(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	store.Data.Accepted.Assigned = true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := store.RegisterSelectedStop(job.ID, job.AttemptID, cancel)
	if _, err := store.RequestStop(job.ID, job.AttemptID, "operator", time.Now().UTC()); err != nil || ctx.Err() != context.Canceled {
		t.Fatalf("request: %v canceled=%v", err, ctx.Err())
	}
	unregister()
	if err := store.ResolveStop(job.ID, job.AttemptID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	if err := m.recoverAccepted(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	decision := store.Data.TerminalDecisions[job.ID]
	if decision.Outcome != "canceled" || decision.StopRequest.Status != "confirmed" ||
		store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() || gh.commentCalls != 0 {
		t.Fatalf("confirmed stop became lost or published: decision=%+v state=%+v comments=%d", decision, store.Data, gh.commentCalls)
	}
}

func TestRecoverUndispatchedStopWithEarlyDecisionIsConfirmed(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unregister := store.RegisterSelectedStop(job.ID, job.AttemptID, cancel)
	if _, err := store.RequestStop(job.ID, job.AttemptID, "operator", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("undispatched selected run did not receive stop")
	}
	unregister()
	if _, err := decideAccepted(store, job, "canceled", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := m.recoverAccepted(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	decision := store.Data.TerminalDecisions[job.ID]
	if decision.Outcome != "canceled" || decision.StopRequest.Status != "confirmed" ||
		store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() || gh.commentCalls != 0 {
		t.Fatalf("undispatched stop was not confirmed: decision=%+v state=%+v comments=%d", decision, store.Data, gh.commentCalls)
	}
}

func TestForceStopDuringControlCancellationCompletesCleanup(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	store.Data.Accepted.Assigned = true
	store.Data.NextInviteCheckAt = time.Now().Add(-time.Second)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	controlEntered, controlRelease := make(chan struct{}), make(chan struct{})
	workerEntered, workerRelease := make(chan struct{}), make(chan struct{})
	var enteredOnce sync.Once
	gh.beforeList = func() {
		enteredOnce.Do(func() { close(controlEntered) })
		<-controlRelease
	}
	finished := make(chan error, 1)
	go func() {
		finished <- m.runSelectedWithControls(t.Context(), store, job, "", nil,
			func(ctx context.Context, _ Job) error {
				close(workerEntered)
				<-ctx.Done()
				<-workerRelease
				return ctx.Err()
			}, func(err error) error { return m.completeSelected(t.Context(), store, job, time.Now().UTC(), err) })
	}()
	select {
	case <-workerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	select {
	case <-controlEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("invitation control did not start")
	}
	if _, err := store.RequestStop(job.ID, job.AttemptID, "operator", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	close(controlRelease)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var decided bool
		_ = store.WithLock(func() error { _, decided = store.Data.TerminalDecisions[job.ID]; return nil })
		if decided {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control cancellation did not enter the stop path")
		}
		time.Sleep(time.Millisecond)
	}
	close(workerRelease)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("normal operator stop returned a poll error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control cancellation did not finish")
	}
	if store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() || gh.commentCalls != 0 {
		t.Fatalf("control cancellation left active action or published reply: state=%+v comments=%d", store.Data, gh.commentCalls)
	}
}

func TestStopAfterWorkerReturnCannotMissCleanupEvidence(t *testing.T) {
	m, store, job, _, _ := assignmentValidationFixture(t)
	store.Data.Accepted.Assigned = true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	returned, release := make(chan struct{}), make(chan struct{})
	m.afterSelectedRunReturn = func() {
		close(returned)
		<-release
	}
	finished := make(chan error, 1)
	go func() {
		finished <- m.runSelectedWithControls(t.Context(), store, job, "", nil,
			func(context.Context, Job) error { return nil }, func(error) error { return nil })
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach return handoff")
	}
	if _, err := store.RequestStop(job.ID, job.AttemptID, "operator", time.Now().UTC()); !errors.Is(err, state.ErrStopConflict) {
		t.Fatalf("completed run accepted late stop without cleanup evidence: %v", err)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("selected run did not finish")
	}
	if decision := store.Data.TerminalDecisions[job.ID]; decision.Outcome != "succeeded" || decision.StopRequest != (state.StopRequest{}) {
		t.Fatalf("late stop changed completed outcome: %+v", decision)
	}
}
