package monitor

import (
	"context"
	"errors"
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
		clean            bool
		workerErr        error
	}{
		{name: "clean cancellation", wantStatus: "confirmed", clean: true, workerErr: context.Canceled},
		{name: "unknown outcome", wantStatus: "unverified", clean: true, workerErr: errors.Join(context.Canceled, ErrExecutionOutcomeUnknown)},
		{name: "unclean worker", wantStatus: "unverified", clean: false, workerErr: context.Canceled},
		{name: "completed at stop", wantStatus: "confirmed", clean: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, store, job, gh, _ := assignmentValidationFixture(t)
			m.Handler = &stopCleanupHandler{clean: test.clean}
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
			if err := m.RevalidateAssignment(t.Context(), store, job); !errors.Is(err, ErrAcceptanceState) {
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
			case <-finished:
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
