package monitor

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/state"
)

func selectedAbortFixture(t *testing.T) (*Monitor, *state.Store, Job, *fakeGitHub, time.Time) {
	t.Helper()
	m, store, job, gh, _ := assignmentValidationFixture(t)
	now := time.Now().UTC()
	job.AcceptedAt, job.Deadline = now, now.Add(time.Second)
	store.Data.Accepted.AcceptedAt = job.AcceptedAt
	store.Data.Accepted.Deadline = job.Deadline
	store.Data.Accepted.Assigned = true
	store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: job.Deadline}
	m.Now = time.Now
	return m, store, job, gh, now
}

func TestSelectedDeadlineDuringEitherInvitationCheckIsNotControlFailure(t *testing.T) {
	for _, check := range []string{"initial", "after terminal notification"} {
		t.Run(check, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, store, job, gh, now := selectedAbortFixture(t)
				repositoryChecks := 0
				if check == "initial" {
					store.Data.NextInviteCheckAt = now
				} else {
					store.Data.NextInviteCheckAt = now.Add(time.Hour)
					queueTerminalControl(store, now.Add(-time.Second), 8,
						state.JobFailure{Attempts: maxJobAttempts}, time.Time{})
					gh.beforeRepository = func() {
						repositoryChecks++
						_ = store.WithLock(func() error {
							store.Data.NextInviteCheckAt = now
							return nil
						})
					}
				}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				listed := make(chan struct{})
				listCalls := 0
				gh.beforeList = func() {
					listCalls++
					close(listed)
					time.Sleep(2 * time.Second)
				}
				release := make(chan struct{})
				run := func(ctx context.Context, _ Job) error {
					<-ctx.Done()
					<-release // Keep abort on the unfinished-worker path.
					return ctx.Err()
				}
				finished := make(chan error, 1)
				go func() {
					finished <- m.runSelectedWithControls(t.Context(), store, job, "", nil, run,
						func(err error) error { return m.completeSelected(t.Context(), store, job, now, err) })
				}()
				<-listed
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if got := store.Data.TerminalDecisions[job.ID].Outcome; got != "expired" {
					t.Fatalf("deadline did not decide expiry before worker cleanup: %q", got)
				}
				close(release)
				err := <-finished
				if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrAcceptanceState) ||
					store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() || listCalls != 1 ||
					(check == "after terminal notification" && repositoryChecks == 0) {
					t.Fatalf("deadline became control corruption: err=%v state=%+v", err, store.Data)
				}
			})
		})
	}
}

func TestSelectedDiscardDuringInvitationDeadlineDoesNotBecomeStateFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, store, job, gh, now := selectedAbortFixture(t)
		store.Data.NextInviteCheckAt = now
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		discarded := make(chan struct{})
		release := make(chan struct{})
		gh.beforeList = func() {
			<-discarded
			time.Sleep(2 * time.Second)
		}
		run := func(context.Context, Job) error {
			if err := store.WithLock(func() error { return m.discardAccepted(store) }); err != nil {
				return err
			}
			close(discarded)
			<-release // Keep the discarded worker in flight until stopWorker runs.
			return ErrAcceptedDiscarded
		}
		finished := make(chan error, 1)
		go func() {
			finished <- m.runSelectedWithControls(t.Context(), store, job, "", nil, run,
				func(err error) error { return m.completeSelected(t.Context(), store, job, now, err) })
		}()
		<-discarded
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if decision := store.Data.TerminalDecisions[job.ID]; decision.AttemptID != job.AttemptID || decision.Outcome != "lost" ||
			store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() {
			t.Fatalf("discard was not durable before stop: %+v", store.Data)
		}
		close(release)
		err := <-finished
		if !errors.Is(err, ErrAcceptedDiscarded) || errors.Is(err, ErrAcceptanceState) {
			t.Fatalf("durable discard became state failure: %v", err)
		}
	})
}

func TestSelectedCompletedRetryDuringInvitationDeadlineExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, store, job, gh, now := selectedAbortFixture(t)
		store.Data.NextInviteCheckAt = now
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		listed := make(chan struct{})
		gh.beforeList = func() {
			close(listed)
			synctest.Wait() // Let the worker return without a terminal decision.
			time.Sleep(2 * time.Second)
		}
		run := func(context.Context, Job) error {
			<-listed
			return ErrAcceptedRetry
		}
		err := m.runSelectedWithControls(t.Context(), store, job, "", nil, run,
			func(err error) error { return m.completeSelected(t.Context(), store, job, now, err) })
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrAcceptanceState) ||
			store.Data.TerminalDecisions[job.ID].Outcome != "expired" || store.Data.Accepted != nil {
			t.Fatalf("completed retry after deadline became control corruption: err=%v state=%+v", err, store.Data)
		}
	})
}

func TestSelectedInvitationStateFailureRemainsFatal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, store, job, _, now := selectedAbortFixture(t)
		store.Data.NextInviteCheckAt = now
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		// Invitation persistence fails while the action deadline is still future.
		store.Data.Cursor = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		run := func(ctx context.Context, _ Job) error {
			<-ctx.Done()
			return ctx.Err()
		}
		err := m.runSelectedWithControls(t.Context(), store, job, "", nil, run,
			func(err error) error { return m.completeSelected(t.Context(), store, job, now, err) })
		if !errors.Is(err, ErrAcceptanceState) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("invitation state failure was hidden: %v", err)
		}
	})
}
