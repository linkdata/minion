package monitor

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func assignmentValidationFixture(t *testing.T) (*Monitor, *state.Store, Job, *fakeGitHub, string) {
	t.Helper()
	now := time.Now().UTC()
	accepted := selectedAcceptance(t, now)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, now.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	item := selectedItem(now)
	gh := &fakeGitHub{permission: github.Permission{Permission: "write"}, currentItem: func(github.Item) (github.Item, error) { return item, nil }}
	m := selectedMonitor(t, now, gh, nil)
	return m, store, jobFromAccepted(accepted), gh, path
}

func TestRevalidateAssignmentChecksCurrentSourceAndPermission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fakeGitHub, *Monitor)
		valid  bool
	}{
		{name: "valid", valid: true},
		{name: "edited command", change: func(gh *fakeGitHub, _ *Monitor) {
			item := selectedItem(time.Now().UTC())
			item.Body = "@linkdataminion fix"
			gh.currentItem = func(github.Item) (github.Item, error) { return item, nil }
		}},
		{name: "permission revoked", change: func(gh *fakeGitHub, _ *Monitor) { gh.permission.Permission = "read" }},
		{name: "repository replaced", change: func(gh *fakeGitHub, _ *Monitor) {
			gh.liveRepo = &github.Repository{ID: 9, FullName: "example-owner/example-repo", Owner: github.User{ID: 2}}
		}},
		{name: "repository archived", change: func(gh *fakeGitHub, _ *Monitor) {
			gh.liveRepo = &github.Repository{ID: 1, FullName: "example-owner/example-repo", Owner: github.User{ID: 2}, DefaultBranch: "main", Archived: true}
		}},
		{name: "repository has no default branch", change: func(gh *fakeGitHub, _ *Monitor) {
			gh.liveRepo = &github.Repository{ID: 1, FullName: "example-owner/example-repo", Owner: github.User{ID: 2}}
		}},
		{name: "allowlist removed", change: func(_ *fakeGitHub, m *Monitor) { m.Config.Repositories[0].Actions = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, store, job, gh, path := assignmentValidationFixture(t)
			if tc.change != nil {
				tc.change(gh, m)
			}
			err := m.RevalidateAssignment(t.Context(), store, job)
			if tc.valid {
				if err != nil || store.Data.Accepted == nil || store.Data.Accepted.Assigned || gh.checks != 1 {
					t.Fatalf("valid action did not remain unassigned: error=%v state=%+v permission_checks=%d", err, store.Data, gh.checks)
				}
				return
			}
			if !errors.Is(err, ErrAcceptedDiscarded) || store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() || len(store.Data.JobFailures) != 0 {
				t.Fatalf("invalid action was not discarded: error=%v state=%+v", err, store.Data)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openStateAt(t, path, time.Now())
			if reopened.Data.Accepted != nil || reopened.Data.Processed[job.ID].IsZero() {
				t.Fatalf("discard was not durable: %+v", reopened.Data)
			}
		})
	}
}

func TestRevalidateAssignmentRetainsAttemptOnTransientLookup(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	lookupErr := &github.StatusError{StatusCode: http.StatusServiceUnavailable}
	gh.repositoryErr = lookupErr
	accepted, failure := *store.Data.Accepted, store.Data.JobFailures[job.ID]
	err := m.RevalidateAssignment(t.Context(), store, job)
	if !errors.Is(err, ErrAcceptedRetry) || !errors.Is(err, lookupErr) || *store.Data.Accepted != accepted || store.Data.JobFailures[job.ID] != failure {
		t.Fatalf("transient lookup changed reservation: error=%v state=%+v", err, store.Data)
	}
	m.Handler = selectedHandler(func(Job) error { return err })
	if got := m.executeSelected(t.Context(), store, job, time.Now()); !errors.Is(got, ErrAcceptedRetry) ||
		*store.Data.Accepted != accepted || store.Data.JobFailures[job.ID] != failure {
		t.Fatalf("transient execution consumed attempt: error=%v state=%+v", got, store.Data)
	}
}

func TestRevalidateAssignmentRetainsAmbiguousMissingNode(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	lookupErr := &github.StatusError{StatusCode: http.StatusNotFound}
	gh.currentItem = func(github.Item) (github.Item, error) { return github.Item{}, lookupErr }
	gh.items = []github.Item{selectedItem(job.Source.UpdatedAt)}
	accepted, failure := *store.Data.Accepted, store.Data.JobFailures[job.ID]
	err := m.RevalidateAssignment(t.Context(), store, job)
	if !errors.Is(err, ErrAcceptedRetry) || !errors.Is(err, lookupErr) || gh.threadCalls != 1 ||
		*store.Data.Accepted != accepted || store.Data.JobFailures[job.ID] != failure {
		t.Fatalf("ambiguous missing node changed reservation: error=%v state=%+v", err, store.Data)
	}
}

func TestRevalidateAssignmentFailsClosedOnUncertainDiscardSave(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	gh.permission.Permission = "read"
	store.Data.Cursor = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	err := m.RevalidateAssignment(t.Context(), store, job)
	if !errors.Is(err, ErrAcceptanceState) || errors.Is(err, ErrAcceptedDiscarded) {
		t.Fatalf("failed discard save was treated as resolved: %v", err)
	}
}

func TestRevalidateAssignmentRejectsChangedReservationAndCancellation(t *testing.T) {
	for _, change := range []struct {
		name   string
		change func(*Job)
	}{
		{"attempt", func(job *Job) { job.AttemptID = "other" }},
		{"repository", func(job *Job) { job.Repository.ID++ }},
		{"source", func(job *Job) { job.Source.NodeID = "other" }},
		{"command", func(job *Job) { job.Command.Name = "fix" }},
		{"deadline", func(job *Job) { job.Deadline = job.Deadline.Add(time.Minute) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			m, store, job, gh, _ := assignmentValidationFixture(t)
			change.change(&job)
			if err := m.RevalidateAssignment(t.Context(), store, job); !errors.Is(err, ErrAcceptanceState) || gh.repoChecks != 0 {
				t.Fatalf("changed reservation reached live lookup: %v", err)
			}
		})
	}
	m, store, job, gh, _ := assignmentValidationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	gh.beforeRepository = cancel
	if err := m.RevalidateAssignment(ctx, store, job); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrAcceptedRetry) {
		t.Fatalf("canceled lookup changed classification: %v", err)
	}
	if store.Data.Accepted == nil || store.Data.Accepted.Assigned {
		t.Fatalf("canceled lookup changed reservation: %+v", store.Data.Accepted)
	}
	m, store, job, gh, _ = assignmentValidationFixture(t)
	gh.beforeRepository = func() { store.Data.Accepted.AttemptID = "other" }
	if err := m.RevalidateAssignment(t.Context(), store, job); !errors.Is(err, ErrAcceptanceState) {
		t.Fatalf("reservation changed during lookup was accepted: %v", err)
	}
}

func TestRevalidateAssignmentRejectsExpiredOriginalDeadline(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	deadline := time.Now().Add(-time.Second)
	acceptedAt := deadline.Add(-time.Minute)
	store.Data.Accepted.AcceptedAt = acceptedAt
	store.Data.Accepted.Deadline, store.Data.JobFailures[job.ID] = deadline, state.JobFailure{
		Attempts: 1, AcceptedAt: acceptedAt, Deadline: deadline,
	}
	job.AcceptedAt, job.Deadline = acceptedAt, deadline
	if err := m.RevalidateAssignment(t.Context(), store, job); !errors.Is(err, context.DeadlineExceeded) || gh.repoChecks != 0 {
		t.Fatalf("expired assignment reached live lookup: %v", err)
	}
}

func TestExecuteSelectedAcceptsFinalDiscard(t *testing.T) {
	m, store, job, gh, _ := assignmentValidationFixture(t)
	gh.permission.Permission = "read"
	m.Handler = selectedHandler(func(job Job) error { return m.RevalidateAssignment(t.Context(), store, job) })
	if err := m.executeSelected(t.Context(), store, job, time.Now()); !errors.Is(err, ErrAcceptedDiscarded) || store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() {
		t.Fatalf("final discard was not resolved: error=%v state=%+v", err, store.Data)
	}
}

func TestFinalDiscardStopsSelectedDiscoveryUntilWorkerRestarts(t *testing.T) {
	at := time.Now().UTC()
	first := selectedItem(at)
	second := first
	second.Key, second.NodeID = "comment:2", "node:2"
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{first, second},
		permission: github.Permission{Permission: "write"}}
	store := openState(t, at.Add(-time.Minute))
	store.Data.NextInviteCheckAt = at.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	m := selectedMonitor(t, at, gh, nil)
	calls := 0
	m.Handler = selectedHandler(func(Job) error {
		calls++
		if err := store.WithLock(func() error { return m.discardAccepted(store) }); err != nil {
			return err
		}
		return ErrAcceptedDiscarded
	})
	if _, err := m.RunOnce(t.Context(), store); err != nil || calls != 1 || store.Data.Accepted != nil || len(store.Data.Processed) != 1 {
		t.Fatalf("retired worker accepted another action: calls=%d error=%v state=%+v", calls, err, store.Data)
	}
}

func TestSelectedDiscardDoesNotHideControlStateFailure(t *testing.T) {
	at := time.Now().UTC()
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{selectedItem(at)},
		permission: github.Permission{Permission: "write"}}
	store := openState(t, at.Add(-time.Minute))
	store.Data.NextInviteCheckAt = at.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	monitor := selectedMonitor(t, at, gh, nil)
	controlErr := errors.New("control state unavailable")
	monitor.Handler = selectedHandler(func(Job) error {
		if err := store.WithLock(func() error { return monitor.discardAccepted(store) }); err != nil {
			return err
		}
		return errors.Join(ErrAcceptedDiscarded, ErrAcceptanceState, controlErr)
	})
	_, err := monitor.RunOnce(t.Context(), store)
	if !errors.Is(err, ErrAcceptanceState) || !errors.Is(err, controlErr) || store.Data.Accepted != nil {
		t.Fatalf("selected discard hid control failure: err=%v accepted=%+v", err, store.Data.Accepted)
	}
}
