package monitor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func TestRevalidatePublicationChecksAssignmentAndLiveAuthority(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		m, store, job, gh, _ := assignmentValidationFixture(t)
		store.Data.Accepted.Assigned = assigned
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		if assigned {
			m.SelectedExecutorID = "" // Recovery may publish before worker preflight.
		}
		if err := m.RevalidatePublication(t.Context(), store, job, assigned); err != nil ||
			gh.repoChecks != 1 || gh.currentChecks != 1 || gh.checks != 1 ||
			len(store.Data.TerminalDecisions) != 0 {
			t.Fatalf("valid publication was rejected: assigned=%t err=%v state=%+v", assigned, err, store.Data)
		}
		if !assigned {
			if err := m.RevalidatePublication(t.Context(), store, job, true); !errors.Is(err, ErrAcceptanceState) || gh.repoChecks != 1 {
				t.Fatalf("unassigned model result reached live checks: err=%v", err)
			}
		}
	}
}

func TestRevalidateFixedRefusalChecksLiveAuthorityAfterOriginalDeadline(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*fakeGitHub, Job)
		valid  bool
	}{
		{name: "valid expired action", valid: true},
		{name: "edited command", change: func(gh *fakeGitHub, job Job) {
			item := selectedItem(job.Source.UpdatedAt)
			item.Body = "@linkdataminion fix"
			gh.currentItem = func(github.Item) (github.Item, error) { return item, nil }
		}},
		{name: "revoked requester", change: func(gh *fakeGitHub, _ Job) { gh.permission.Permission = "read" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, store, job, gh, _ := assignmentValidationFixture(t)
			job.AcceptedAt = time.Now().UTC().Add(-2 * time.Minute)
			job.Deadline = job.AcceptedAt.Add(time.Minute)
			store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = job.AcceptedAt, job.Deadline
			if err := store.WithLock(func() error {
				_, err := store.DecideTerminalLocked(job.ID, job.AttemptID, "expired", job.Deadline)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if test.change != nil {
				test.change(gh, job)
			}
			err := m.RevalidateFixedRefusal(t.Context(), store, job)
			if test.valid && err != nil || !test.valid && !errors.Is(err, ErrTerminalJob) {
				t.Fatalf("fixed refusal authority: valid=%t error=%v", test.valid, err)
			}
			if got := store.Data.TerminalDecisions[job.ID].Outcome; got != "expired" {
				t.Fatalf("fixed refusal changed first outcome to %q", got)
			}
		})
	}
	m, store, job, _, _ := assignmentValidationFixture(t)
	job.AttemptID = ""
	job.Deadline = time.Now().Add(-time.Minute)
	if err := m.RevalidateFixedRefusal(t.Context(), store, job); !errors.Is(err, ErrAcceptanceState) {
		t.Fatalf("fixed refusal without a selected attempt was accepted: %v", err)
	}
	m, store, job, _, _ = assignmentValidationFixture(t)
	if err := store.WithLock(func() error {
		_, err := store.DecideTerminalLocked(job.ID, job.AttemptID, "succeeded", time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.RevalidateFixedRefusal(t.Context(), store, job); !errors.Is(err, ErrTerminalJob) {
		t.Fatalf("already published action received another fixed refusal: %v", err)
	}
}

func TestRevalidateFixedRefusalRechecksSelectedReservation(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	gh.beforeRepository = func() {
		if err := store.WithLock(func() error {
			store.Data.Accepted.AttemptID = "changed-at-lookup"
			return store.Save()
		}); err != nil {
			t.Error(err)
		}
	}
	if err := m.RevalidateFixedRefusal(t.Context(), store, job); !errors.Is(err, ErrAcceptanceState) {
		t.Fatalf("changed reservation passed fixed refusal validation: %v", err)
	}
}

func TestRevalidatePublicationPersistsLostAuthority(t *testing.T) {
	m, store, job, gh, path := assignmentValidationFixture(t)
	store.Data.Accepted.Assigned = true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh.permission.Permission = "read"
	if err := m.RevalidatePublication(t.Context(), store, job, true); !errors.Is(err, ErrTerminalJob) {
		t.Fatalf("revoked authority was accepted: %v", err)
	}
	decision := store.Data.TerminalDecisions[job.ID]
	if decision.Outcome != "lost" || decision.AttemptID != job.AttemptID || !store.Data.Accepted.Terminal {
		t.Fatalf("lost decision was not saved: %+v", store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, time.Time{})
	if reopened.Data.TerminalDecisions[job.ID] != decision {
		t.Fatalf("lost decision was not durable: %+v", reopened.Data)
	}
}

func TestRevalidatePublicationRetriesTransientLookup(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	store.Data.Accepted.Assigned = true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	lookupErr := &github.StatusError{StatusCode: http.StatusServiceUnavailable}
	gh.repositoryErr = lookupErr
	if err := m.RevalidatePublication(t.Context(), store, job, true); !errors.Is(err, lookupErr) ||
		errors.Is(err, ErrTerminalJob) || len(store.Data.TerminalDecisions) != 0 || store.Data.Accepted.Terminal {
		t.Fatalf("transient lookup became terminal: err=%v state=%+v", err, store.Data)
	}
}

func TestRevalidatePublicationRechecksStateAfterLiveLookup(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	store.Data.Accepted.Assigned = true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh.beforeRepository = func() {
		if err := store.WithLock(func() error {
			store.Data.Accepted.AttemptID = "other"
			return store.Save()
		}); err != nil {
			t.Error(err)
		}
	}
	if err := m.RevalidatePublication(t.Context(), store, job, true); !errors.Is(err, ErrAcceptanceState) ||
		len(store.Data.TerminalDecisions) != 0 {
		t.Fatalf("changed acceptance passed publication check: err=%v state=%+v", err, store.Data)
	}
}

func TestRevalidatePublicationExpiresBeforeLiveLookup(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	deadline := time.Now().Add(-time.Second)
	acceptedAt := deadline.Add(-time.Minute)
	store.Data.Accepted.Assigned = true
	store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = acceptedAt, deadline
	store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: acceptedAt, Deadline: deadline}
	job.AcceptedAt, job.Deadline = acceptedAt, deadline
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := m.RevalidatePublication(t.Context(), store, job, true); !errors.Is(err, context.DeadlineExceeded) ||
		!errors.Is(err, ErrTerminalJob) || gh.repoChecks != 0 ||
		store.Data.TerminalDecisions[job.ID].Outcome != "expired" {
		t.Fatalf("expired action reached live lookup: err=%v state=%+v", err, store.Data)
	}
}

func TestRevalidatePublicationKeepsEarlierCancellationAfterSlowLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, store, job, gh, _ := assignmentValidationFixture(t)
		now := time.Now().UTC()
		job.AcceptedAt, job.Deadline = now, now.Add(time.Second)
		store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = job.AcceptedAt, job.Deadline
		store.Data.Accepted.Assigned = true
		store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: job.Deadline}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithDeadline(t.Context(), job.Deadline)
		defer cancel()
		gh.beforeRepository = func() {
			cancel()
			time.Sleep(2 * time.Second)
		}
		err := m.RevalidatePublication(ctx, store, job, true)
		if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			len(store.Data.TerminalDecisions) != 0 || store.Data.Accepted.Terminal {
			t.Fatalf("earlier cancellation was replaced by expiry: err=%v state=%+v", err, store.Data)
		}
	})
}

type deadlineProbeAction struct {
	store    *state.Store
	entered  chan struct{}
	stopped  chan struct{}
	release  chan struct{}
	observed chan string
}

func (h *deadlineProbeAction) Execute(ctx context.Context, job Job) error {
	if err := h.store.WithLock(func() error {
		h.store.Data.Accepted.Assigned = true
		return h.store.Save()
	}); err != nil {
		return err
	}
	h.entered <- struct{}{}
	<-ctx.Done()
	h.stopped <- struct{}{}
	<-h.release
	_ = h.store.WithLock(func() error {
		h.observed <- h.store.Data.TerminalDecisions[job.ID].Outcome
		return nil
	})
	return nil // A late successful return cannot replace the terminal decision.
}

func (*deadlineProbeAction) WaitCapacity(context.Context) error { return nil }

func TestSelectedDeadlineIsDurableBeforeWorkerCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, store, job, gh, _ := assignmentValidationFixture(t)
		now := time.Now().UTC()
		store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = now, now.Add(time.Minute)
		store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: now.Add(time.Minute)}
		job.AcceptedAt, job.Deadline = now, now.Add(time.Minute)
		store.Data.NextInviteCheckAt = now.Add(time.Hour)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh.currentItem = func(github.Item) (github.Item, error) { return selectedItem(job.Source.UpdatedAt), nil }
		handler := &deadlineProbeAction{store: store, entered: make(chan struct{}, 1), stopped: make(chan struct{}, 1),
			release: make(chan struct{}), observed: make(chan string, 1)}
		released := false
		defer func() {
			if !released {
				close(handler.release)
			}
		}()
		m.Handler = handler
		finished := make(chan error, 1)
		go func() { finished <- m.executeSelected(t.Context(), store, job, now) }()
		<-handler.entered
		time.Sleep(time.Minute)
		synctest.Wait()
		<-handler.stopped
		if decision := store.Data.TerminalDecisions[job.ID]; decision.Outcome != "expired" {
			t.Fatalf("expiry was not saved while worker cleanup was blocked: %+v", store.Data)
		}
		close(handler.release)
		released = true
		if outcome := <-handler.observed; outcome != "expired" {
			t.Fatalf("worker cleanup preceded expiry decision: %q", outcome)
		}
		if err := <-finished; !errors.Is(err, context.DeadlineExceeded) ||
			store.Data.TerminalDecisions[job.ID].Outcome != "expired" || store.Data.Accepted != nil {
			t.Fatalf("late worker result replaced expiry: err=%v state=%+v", err, store.Data)
		}
	})
}

func TestSelectedCancellationIsDurableBeforeWorkerCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, store, job, gh, _ := assignmentValidationFixture(t)
		gh.currentItem = func(github.Item) (github.Item, error) { return selectedItem(job.Source.UpdatedAt), nil }
		handler := &deadlineProbeAction{store: store, entered: make(chan struct{}, 1), stopped: make(chan struct{}, 1),
			release: make(chan struct{}), observed: make(chan string, 1)}
		released := false
		defer func() {
			if !released {
				close(handler.release)
			}
		}()
		m.Handler = handler
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		finished := make(chan error, 1)
		go func() { finished <- m.executeSelected(ctx, store, job, time.Now().UTC()) }()
		<-handler.entered
		cancel()
		synctest.Wait()
		<-handler.stopped
		if decision := store.Data.TerminalDecisions[job.ID]; decision.Outcome != "canceled" {
			t.Fatalf("cancellation was not saved while worker cleanup was blocked: %+v", store.Data)
		}
		close(handler.release)
		released = true
		if outcome := <-handler.observed; outcome != "canceled" {
			t.Fatalf("worker cleanup preceded cancellation decision: %q", outcome)
		}
		if err := <-finished; !errors.Is(err, context.Canceled) ||
			store.Data.TerminalDecisions[job.ID].Outcome != "canceled" || store.Data.Accepted != nil {
			t.Fatalf("late worker result replaced cancellation: err=%v state=%+v", err, store.Data)
		}
	})
}

func TestSelectedStopOutcomePreservesFirstCause(t *testing.T) {
	deadline := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name           string
		cause          error
		stoppedAt      time.Time
		parentDeadline time.Time
		want           string
	}{
		{"user cancellation survives late lock", context.Canceled, deadline.Add(time.Second), time.Time{}, "canceled"},
		{"original deadline", context.DeadlineExceeded, deadline, time.Time{}, "expired"},
		{"deadline fires after stop timestamp capture", context.DeadlineExceeded, deadline.Add(-time.Nanosecond), time.Time{}, "expired"},
		{"parent deadline before original", context.DeadlineExceeded, deadline.Add(time.Second), deadline.Add(-time.Second), "canceled"},
		{"control stop before deadline", errors.New("control failure"), deadline.Add(-time.Second), time.Time{}, "canceled"},
		{"control stop after deadline", errors.New("control failure"), deadline.Add(time.Second), time.Time{}, "expired"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := selectedStopOutcome(test.cause, test.stoppedAt, deadline, test.parentDeadline); got != test.want {
				t.Fatalf("stop outcome = %q; want %q", got, test.want)
			}
		})
	}
}

func TestSelectedSuccessBeforeDeadlineSurvivesSlowInvitationCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, store, job, gh, _ := assignmentValidationFixture(t)
		now := time.Now().UTC()
		store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = now, now.Add(time.Second)
		store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: now.Add(time.Second)}
		store.Data.NextInviteCheckAt = now
		job.AcceptedAt, job.Deadline = now, now.Add(time.Second)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		startRun := make(chan struct{})
		var decidedBeforeExpiry bool
		gh.beforeList = func() {
			close(startRun)
			synctest.Wait()
			_ = store.WithLock(func() error {
				decidedBeforeExpiry = store.Data.TerminalDecisions[job.ID].Outcome == "succeeded"
				return nil
			})
			time.Sleep(2 * time.Second)
		}
		run := func(context.Context, Job) error {
			<-startRun
			return store.WithLock(func() error {
				store.Data.Accepted.Assigned = true
				return store.Save()
			})
		}
		err := m.runSelectedWithControls(t.Context(), store, job, "", nil, run,
			func(err error) error { return m.completeSelected(t.Context(), store, job, now, err) })
		if err != nil || !decidedBeforeExpiry || store.Data.TerminalDecisions[job.ID].Outcome != "succeeded" ||
			store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() || len(gh.comments) != 0 {
			t.Fatalf("published success was replaced by expiry during invitation check: err=%v state=%+v comments=%v", err, store.Data, gh.comments)
		}
	})
}

func TestSelectedTerminalResultBeforeDeadlineSurvivesSlowInvitationCheck(t *testing.T) {
	for _, test := range []struct {
		name    string
		result  error
		outcome string
	}{
		{"failed", ErrTerminalJob, "failed"},
		{"unknown", ErrExecutionOutcomeUnknown, "lost"},
		{"canceled", context.Canceled, "canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, store, job, gh, _ := assignmentValidationFixture(t)
				now := time.Now().UTC()
				job.AcceptedAt, job.Deadline = now, now.Add(time.Second)
				store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = job.AcceptedAt, job.Deadline
				store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: job.Deadline}
				store.Data.NextInviteCheckAt = now
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				startRun := make(chan struct{})
				var observed string
				listed := false
				gh.beforeList = func() {
					if listed {
						return
					}
					listed = true
					close(startRun)
					synctest.Wait()
					_ = store.WithLock(func() error {
						observed = store.Data.TerminalDecisions[job.ID].Outcome
						return nil
					})
					time.Sleep(2 * time.Second)
				}
				run := func(context.Context, Job) error {
					<-startRun
					if err := store.WithLock(func() error {
						store.Data.Accepted.Assigned = true
						return store.Save()
					}); err != nil {
						return err
					}
					return test.result
				}
				err := m.runSelectedWithControls(t.Context(), store, job, "", nil, run,
					func(err error) error { return m.completeSelected(t.Context(), store, job, now, err) })
				if err != nil || observed != test.outcome || store.Data.TerminalDecisions[job.ID].Outcome != test.outcome ||
					store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() || len(gh.comments) != 1 {
					t.Fatalf("early %s became expiry after invitation check: err=%v observed=%q state=%+v comments=%v", test.name, err, observed, store.Data, gh.comments)
				}
			})
		})
	}
}

func TestSelectedNilResultAfterEarlyParentStopStaysCanceled(t *testing.T) {
	for _, earlyParentDeadline := range []bool{false, true} {
		name := "user cancellation"
		if earlyParentDeadline {
			name = "earlier parent deadline"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, store, job, gh, _ := assignmentValidationFixture(t)
				now := time.Now().UTC()
				job.AcceptedAt, job.Deadline = now, now.Add(time.Second)
				store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = job.AcceptedAt, job.Deadline
				store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: job.Deadline}
				store.Data.NextInviteCheckAt = now
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				parent := t.Context()
				var stop context.CancelFunc
				if earlyParentDeadline {
					parent, stop = context.WithDeadline(parent, now.Add(500*time.Millisecond))
				} else {
					parent, stop = context.WithCancel(parent)
				}
				defer stop()
				startRun := make(chan struct{})
				var observed string
				listed := false
				gh.beforeList = func() {
					if listed {
						return
					}
					listed = true
					close(startRun)
					time.Sleep(700 * time.Millisecond)
					synctest.Wait()
					_ = store.WithLock(func() error {
						observed = store.Data.TerminalDecisions[job.ID].Outcome
						return nil
					})
					time.Sleep(2 * time.Second)
				}
				run := func(context.Context, Job) error {
					<-startRun
					if err := store.WithLock(func() error {
						store.Data.Accepted.Assigned = true
						return store.Save()
					}); err != nil {
						return err
					}
					if earlyParentDeadline {
						time.Sleep(600 * time.Millisecond)
					} else {
						stop()
					}
					return nil // Cleanup returned success after the stop was already requested.
				}
				err := m.runSelectedWithControls(parent, store, job, "", nil, run,
					func(err error) error { return m.completeSelected(parent, store, job, now, err) })
				if err != nil || observed != "canceled" || store.Data.TerminalDecisions[job.ID].Outcome != "canceled" ||
					store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() || len(gh.comments) != 1 {
					t.Fatalf("earlier parent stop became expiry: err=%v observed=%q state=%+v comments=%v", err, observed, store.Data, gh.comments)
				}
			})
		})
	}
}

func TestSelectedSuccessBeforeLateParentStopStaysSucceeded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, store, job, _, _ := assignmentValidationFixture(t)
		now := time.Now().UTC()
		job.AcceptedAt, job.Deadline = now, now.Add(time.Minute)
		store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = job.AcceptedAt, job.Deadline
		store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: job.Deadline}
		store.Data.NextInviteCheckAt = now.Add(time.Hour)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		m.afterSelectedRunReturn = func() {
			cancel()
			synctest.Wait() // Let the stop path race with the saved success decision.
		}
		run := func(context.Context, Job) error {
			return store.WithLock(func() error {
				store.Data.Accepted.Assigned = true
				return store.Save()
			})
		}
		_ = m.runSelectedWithControls(parent, store, job, "", nil, run, func(error) error { return nil })
		if got := store.Data.TerminalDecisions[job.ID].Outcome; got != "succeeded" {
			t.Fatalf("late parent cancellation changed published success to %q", got)
		}
	})
}

func TestSelectedTerminalErrorBeforeLateParentStopKeepsItsOutcome(t *testing.T) {
	for _, test := range []struct {
		name    string
		result  error
		outcome string
	}{
		{"failed", ErrTerminalJob, "failed"},
		{"lost", ErrExecutionOutcomeUnknown, "lost"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, store, job, _, _ := assignmentValidationFixture(t)
				now := time.Now().UTC()
				job.AcceptedAt, job.Deadline = now, now.Add(time.Minute)
				store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = job.AcceptedAt, job.Deadline
				store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: job.Deadline}
				store.Data.NextInviteCheckAt = now.Add(time.Hour)
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				m.afterSelectedRunReturn = func() {
					cancel()
					synctest.Wait()
				}
				run := func(context.Context, Job) error {
					if err := store.WithLock(func() error {
						store.Data.Accepted.Assigned = true
						return store.Save()
					}); err != nil {
						return err
					}
					return test.result
				}
				_ = m.runSelectedWithControls(parent, store, job, "", nil, run, func(error) error { return nil })
				if got := store.Data.TerminalDecisions[job.ID].Outcome; got != test.outcome {
					t.Fatalf("late parent cancellation changed %s to %q", test.outcome, got)
				}
			})
		})
	}
}

func TestSelectedSavedResultRetrySurvivesSlowInvitationCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, store, job, gh, _ := assignmentValidationFixture(t)
		now := time.Now().UTC()
		job.AcceptedAt, job.Deadline = now, now.Add(10*time.Minute)
		store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = job.AcceptedAt, job.Deadline
		store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: job.Deadline}
		store.Data.NextInviteCheckAt = now
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		startRun := make(chan struct{})
		listed := false
		gh.beforeList = func() {
			if listed {
				return
			}
			listed = true
			close(startRun)
			synctest.Wait()
			time.Sleep(2 * time.Second)
		}
		publishErr := errors.New("publication response unavailable")
		run := func(context.Context, Job) error {
			<-startRun
			if err := store.WithLock(func() error {
				store.Data.Accepted.Assigned = true
				if err := store.Save(); err != nil {
					return err
				}
				if err := store.WriteAcceptedResult([]byte(`{"result":"saved-before-timeout"}`)); err != nil {
					return err
				}
				store.Data.Accepted.ResultReady = true
				return store.Save()
			}); err != nil {
				return err
			}
			return errors.Join(ErrPostModelPublication, publishErr)
		}
		err := m.runSelectedWithControls(t.Context(), store, job, "", nil, run,
			func(err error) error { return m.completeSelected(t.Context(), store, job, now, err) })
		if !errors.Is(err, ErrPostModelPublication) || !errors.Is(err, publishErr) ||
			len(store.Data.TerminalDecisions) != 0 || store.Data.Accepted == nil ||
			store.Data.Processed[job.ID] != (time.Time{}) || len(gh.comments) != 0 {
			t.Fatalf("saved result lost retry after invitation check: err=%v state=%+v comments=%v", err, store.Data, gh.comments)
		}
		if present, err := store.HasAcceptedResult(); err != nil || !present {
			t.Fatalf("publication retry lost its saved result: present=%t err=%v", present, err)
		}
	})
}

func TestDecideAcceptedUsesPreDeadlineCompletionTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, store, job, _, _ := assignmentValidationFixture(t)
		now := time.Now().UTC()
		store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = now, now.Add(time.Second)
		job.AcceptedAt, job.Deadline = now, now.Add(time.Second)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		decision, err := decideAccepted(store, job, "succeeded", now)
		if err != nil || decision.Outcome != "succeeded" {
			t.Fatalf("completion before deadline changed while saving its decision: decision=%+v err=%v", decision, err)
		}
	})
}

func TestTerminalAcceptedUsesOneObservationForOutcomeAndDecision(t *testing.T) {
	m, store, job, _, _ := assignmentValidationFixture(t)
	job.AcceptedAt = time.Now().UTC().Add(-10 * time.Minute)
	job.Deadline = job.AcceptedAt.Add(7 * time.Minute)
	store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = job.AcceptedAt, job.Deadline
	store.Data.Accepted.Assigned = true
	failure := state.JobFailure{Attempts: 1, AcceptedAt: job.AcceptedAt, Deadline: job.Deadline}
	store.Data.JobFailures[job.ID] = failure
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	observations := 0
	m.Now = func() time.Time {
		observations++
		if observations == 1 {
			return job.Deadline.Add(-time.Nanosecond)
		}
		return job.Deadline.Add(time.Second)
	}
	if err := m.terminalAccepted(t.Context(), store, job, failure); err != nil {
		t.Fatal(err)
	}
	if decision := store.Data.TerminalDecisions[job.ID]; decision.Outcome != "lost" ||
		!decision.DecidedAt.Equal(job.Deadline.Add(-time.Nanosecond)) {
		t.Fatalf("terminal outcome changed between observations: decision=%+v", decision)
	}
}

func TestDecideAcceptedPreservesCancellationAfterLateCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, store, job, _, path := assignmentValidationFixture(t)
		now := time.Now().UTC()
		job.AcceptedAt, job.Deadline = now, now.Add(time.Second)
		store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = job.AcceptedAt, job.Deadline
		store.Data.Accepted.Assigned = true
		store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: job.Deadline}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		parent, cancel := context.WithCancel(t.Context())
		cancel()
		time.Sleep(2 * time.Second) // The worker finishes cleanup after the original deadline.
		completedAt := time.Now().UTC()
		outcome, err := selectedCompletedOutcome(store, job, context.Canceled, parent.Err(), completedAt, time.Time{})
		if err != nil || outcome != "canceled" {
			t.Fatalf("late worker cleanup outcome = %q, %v; want canceled", outcome, err)
		}
		decision, err := decideAccepted(store, job, outcome, completedAt)
		if err != nil || decision.Outcome != "canceled" {
			t.Fatalf("persisted decision = %+v, %v; want canceled", decision, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened := openStateAt(t, path, completedAt)
		if got := reopened.Data.TerminalDecisions[job.ID].Outcome; got != "canceled" {
			t.Fatalf("reopened decision = %q; want canceled", got)
		}
	})
}

func TestSelectedCompletedOutcomeUsesRunTimeBeforeLaterDeadlineSignal(t *testing.T) {
	_, store, job, _, _ := assignmentValidationFixture(t)
	store.Data.Accepted.Assigned = true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	completedAt := job.Deadline.Add(-time.Nanosecond)
	outcome, err := selectedCompletedOutcome(store, job, nil, context.DeadlineExceeded, completedAt, time.Time{})
	if err != nil || outcome != "succeeded" {
		t.Fatalf("pre-deadline completion lost to later deadline signal: outcome=%q err=%v", outcome, err)
	}
	outcome, err = selectedCompletedOutcome(store, job, nil, context.DeadlineExceeded, completedAt, completedAt.Add(-time.Second))
	if err != nil || outcome != "canceled" {
		t.Fatalf("earlier parent deadline lost to completion: outcome=%q err=%v", outcome, err)
	}
	outcome, err = selectedCompletedOutcome(store, job, context.Canceled, context.DeadlineExceeded, job.Deadline.Add(time.Second), time.Time{})
	if err != nil || outcome != "expired" {
		t.Fatalf("original deadline lost to later worker cancellation: outcome=%q err=%v", outcome, err)
	}
	parentDeadline := job.Deadline.Add(-time.Second)
	outcome, err = selectedCompletedOutcome(store, job, context.DeadlineExceeded, context.DeadlineExceeded, parentDeadline, parentDeadline)
	if err != nil || outcome != "canceled" {
		t.Fatalf("earlier parent deadline with worker error became failure: outcome=%q err=%v", outcome, err)
	}
	outcome, err = selectedCompletedOutcome(store, job, context.DeadlineExceeded, nil, parentDeadline, parentDeadline)
	if err != nil || outcome != "failed" {
		t.Fatalf("inner deadline with live action context became cancellation: outcome=%q err=%v", outcome, err)
	}
}

func TestAssignedRecoveryRecordsLostWorkerOutcome(t *testing.T) {
	m, store, job, gh, path := assignmentValidationFixture(t)
	store.Data.Accepted.Assigned = true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	m.Handler = selectedHandler(func(Job) error {
		t.Fatal("lost worker was redispatched")
		return nil
	})
	if _, err := m.RunOnce(t.Context(), store); err != nil ||
		store.Data.TerminalDecisions[job.ID].Outcome != "lost" ||
		store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() || len(gh.comments) != 1 {
		t.Fatalf("lost worker was not finalized: err=%v state=%+v comments=%v", err, store.Data, gh.comments)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, time.Time{})
	if reopened.Data.TerminalDecisions[job.ID].Outcome != "lost" {
		t.Fatalf("lost worker decision was not durable: %+v", reopened.Data)
	}
}

func TestCompleteSelectedClassifiesUnknownAndDeadline(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		outcome string
	}{
		{"unknown", ErrExecutionOutcomeUnknown, "lost"},
		{"unknown with canceled transport", errors.Join(ErrExecutionOutcomeUnknown, context.Canceled), "lost"},
		{"inner deadline error before action deadline", context.DeadlineExceeded, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, store, job, gh, _ := assignmentValidationFixture(t)
			store.Data.Accepted.Assigned = true
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			if err := m.completeSelected(t.Context(), store, job, time.Now().UTC(), test.failure); err != nil ||
				store.Data.TerminalDecisions[job.ID].Outcome != test.outcome ||
				store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() || len(gh.comments) != 1 {
				t.Fatalf("terminal error was misclassified: err=%v state=%+v comments=%v", err, store.Data, gh.comments)
			}
		})
	}
}

func TestCompleteSelectedKeepsSavedResultAfterInnerRequestDeadline(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	store.Data.Accepted.Assigned = true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAcceptedResult([]byte(`{"result":"saved-before-timeout"}`)); err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted.ResultReady = true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	err := m.completeSelected(t.Context(), store, job, time.Now().UTC(), context.DeadlineExceeded)
	if !errors.Is(err, ErrPostModelPublication) || !errors.Is(err, context.DeadlineExceeded) ||
		len(store.Data.TerminalDecisions) != 0 || store.Data.Accepted == nil ||
		store.Data.Processed[job.ID] != (time.Time{}) || len(gh.comments) != 0 {
		t.Fatalf("inner request timeout ended an action before its deadline: err=%v state=%+v comments=%v", err, store.Data, gh.comments)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("saved result was lost after inner request timeout: present=%t err=%v", present, err)
	}
}

func TestCompleteSelectedDoesNotReplyBeforeTerminalDecisionIsDurable(t *testing.T) {
	m, store, job, gh, path := assignmentValidationFixture(t)
	store.Data.Accepted.Assigned = true
	failure := store.Data.JobFailures[job.ID]
	failure.Attempts = maxJobAttempts
	store.Data.JobFailures[job.ID] = failure
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.WithLock(func() error {
		_, err := store.DecideTerminalLocked(job.ID, job.AttemptID, "canceled", time.Now().UTC())
		return err
	}); err == nil {
		t.Fatal("blocked state path allowed terminal decision save")
	}
	if err := m.completeSelected(t.Context(), store, job, time.Now().UTC(), context.Canceled); !errors.Is(err, ErrAcceptanceState) ||
		len(gh.comments) != 0 || store.Data.Processed[job.ID] != (time.Time{}) || store.Data.Accepted == nil {
		t.Fatalf("unconfirmed terminal decision produced a reply: err=%v state=%+v comments=%v", err, store.Data, gh.comments)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if err := m.completeSelected(t.Context(), store, job, time.Now().UTC(), context.Canceled); err != nil ||
		len(gh.comments) != 1 || store.Data.TerminalDecisions[job.ID].Outcome != "canceled" ||
		store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() {
		t.Fatalf("confirmed decision did not finish: err=%v state=%+v comments=%v", err, store.Data, gh.comments)
	}
}
