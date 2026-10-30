package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/state"
)

func selectedAbortFixture(t *testing.T) (*Monitor, *state.Store, Job, *fakeGitHub, time.Time, string) {
	t.Helper()
	m, store, job, gh, path := assignmentValidationFixture(t)
	now := time.Now().UTC()
	job.AcceptedAt, job.Deadline = now, now.Add(time.Second)
	store.Data.Accepted.AcceptedAt = job.AcceptedAt
	store.Data.Accepted.Deadline = job.Deadline
	store.Data.Accepted.Assigned = true
	store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: job.Deadline}
	m.Now = time.Now
	return m, store, job, gh, now, path
}

func TestSelectedCancelAfterAmbiguousResultSaveIsDurable(t *testing.T) {
	for _, stage := range []string{"result marker", "terminal decision"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, store, job, gh, path := assignmentValidationFixture(t)
				now := time.Now().UTC()
				job.AcceptedAt, job.Deadline = now, now.Add(time.Minute)
				store.Data.Accepted.AcceptedAt, store.Data.Accepted.Deadline = job.AcceptedAt, job.Deadline
				store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: now, Deadline: job.Deadline}
				store.Data.NextInviteCheckAt = now
				m.Now = time.Now
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				workerReturned := make(chan struct{})
				gh.beforeList = func() {
					<-workerReturned
					synctest.Wait() // Let the worker finish before the canceled invitation check returns.
				}
				run := func(context.Context, Job) error {
					defer close(workerReturned)
					if err := store.WithLock(func() error {
						store.Data.Accepted.Assigned = true
						if err := store.WriteAcceptedResult([]byte(`{"result":"saved"}`)); err != nil {
							return err
						}
						store.Data.Accepted.ResultReady = true
						return store.Save()
					}); err != nil {
						return err
					}
					cancel()
					if stage == "result marker" {
						_ = store.WithLock(func() error {
							store.Data.Accepted.ResultReady = false // A post-rename save error rolls back memory only.
							return nil
						})
						return errors.Join(ErrAcceptanceState, errors.New("result marker sync failed"))
					}
					backup := path + ".before-decision"
					if err := os.Rename(path, backup); err != nil {
						return err
					}
					if err := os.Mkdir(path, 0700); err != nil {
						return err
					}
					decisionErr := store.WithLock(func() error {
						_, err := store.DecideTerminalLocked(job.ID, job.AttemptID, "canceled", time.Now().UTC())
						return err
					})
					if err := os.Remove(path); err != nil {
						return err
					}
					if err := os.Rename(backup, path); err != nil {
						return err
					}
					if decisionErr == nil {
						return errors.New("terminal decision save unexpectedly succeeded")
					}
					return errors.Join(ErrAcceptanceState, decisionErr)
				}
				err := m.runSelectedWithControls(parent, store, job, "", nil, run,
					func(err error) error { return m.completeSelected(parent, store, job, now, err) })
				if !errors.Is(err, ErrAcceptanceState) ||
					store.Data.TerminalDecisions[job.ID].Outcome != "canceled" {
					t.Fatalf("canceled result left no terminal decision: err=%v state=%+v", err, store.Data)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				reopened := openStateAt(t, path, now)
				if decision := reopened.Data.TerminalDecisions[job.ID]; decision.AttemptID != job.AttemptID || decision.Outcome != "canceled" {
					t.Fatalf("restart lost canceled decision: %+v", reopened.Data)
				}
			})
		})
	}
}

func TestSelectedDeadlineDuringEitherInvitationCheckIsNotControlFailure(t *testing.T) {
	for _, check := range []string{"initial", "after terminal notification"} {
		t.Run(check, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, store, job, gh, now, path := selectedAbortFixture(t)
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
				released := false
				defer func() {
					if !released {
						close(release)
					}
				}()
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
				decision := store.Data.TerminalDecisions[job.ID]
				if decision.AttemptID != job.AttemptID || decision.Outcome != "expired" ||
					store.Data.Accepted == nil || store.Data.Accepted.JobID != job.ID ||
					store.Data.Accepted.AttemptID != job.AttemptID || !store.Data.Accepted.Terminal ||
					!store.Data.Processed[job.ID].IsZero() {
					t.Fatalf("deadline did not retain terminal reservation before worker cleanup: state=%+v", store.Data)
				}
				stateBytes, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var saved state.Data
				if err := json.Unmarshal(stateBytes, &saved); err != nil {
					t.Fatal(err)
				}
				if saved.TerminalDecisions[job.ID] != decision || saved.Accepted == nil ||
					saved.Accepted.JobID != job.ID || saved.Accepted.AttemptID != job.AttemptID || !saved.Accepted.Terminal {
					t.Fatalf("deadline decision was not durable before worker cleanup: state=%+v", saved)
				}
				select {
				case err := <-finished:
					t.Fatalf("selected monitor returned before worker cleanup: %v", err)
				default:
				}
				close(release)
				released = true
				err = <-finished
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
		m, store, job, gh, now, _ := selectedAbortFixture(t)
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
		m, store, job, gh, now, _ := selectedAbortFixture(t)
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
		m, store, job, _, now, _ := selectedAbortFixture(t)
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

func TestSelectedSavedResultDoesNotHideInvitationStateFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, store, job, gh, now, _ := selectedAbortFixture(t)
		store.Data.NextInviteCheckAt = now
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{})
		release := make(chan struct{})
		entered := make(chan struct{})
		gh.beforeList = func() {
			close(entered)
			<-ready
		}
		m.afterInvitationCheck = func() {
			close(release)
			synctest.Wait() // Let the saved-result worker finish after the context check.
			store.Data.Cursor = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		run := func(context.Context, Job) error {
			<-entered // Keep the invitation check in flight before saving the result.
			if err := store.WithLock(func() error {
				if err := store.WriteAcceptedResult([]byte(`{"result":"saved"}`)); err != nil {
					return err
				}
				store.Data.Accepted.ResultReady = true
				return store.Save()
			}); err != nil {
				return err
			}
			close(ready)
			<-release
			return ErrPostModelPublication
		}
		err := m.runSelectedWithControls(t.Context(), store, job, "", nil, run,
			func(err error) error { return m.completeSelected(t.Context(), store, job, now, err) })
		if !errors.Is(err, ErrPostModelPublication) || !errors.Is(err, ErrAcceptanceState) {
			t.Fatalf("saved result hid an independent invitation state failure: %v", err)
		}
	})
}
