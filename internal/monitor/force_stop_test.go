package monitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/state"
)

type stopCleanupHandler struct {
	fakeHandler
	clean bool
}

func (h *stopCleanupHandler) StopCleanupVerified() bool { return h.clean }

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
