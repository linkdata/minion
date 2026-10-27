package monitor

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func TestTerminalIssueAndReplyLookupsHaveSeparateBudgets(t *testing.T) {
	accepted := selectedAcceptance(t, time.Now().UTC())
	accepted.SubjectType = "Issue"
	accepted.SourceURL = "https://github.com/example-owner/example-repo/issues/7#issuecomment-1"
	stop := errors.New("reply lookup reached")
	var remaining time.Duration
	var hasDeadline bool
	gh := &fakeGitHub{
		issueDetails: func(_ string, number int) (github.Issue, error) {
			time.Sleep(8 * time.Second)
			return github.Issue{Number: number}, nil
		},
		beforeVerify: func(ctx context.Context, _ bool) error {
			deadline, ok := ctx.Deadline()
			hasDeadline = ok
			remaining = time.Until(deadline)
			return stop
		},
	}
	m := selectedMonitor(t, accepted.AcceptedAt, gh, nil)
	store := &state.Store{Data: state.Data{Accepted: &accepted}}
	synctest.Test(t, func(t *testing.T) {
		if err := m.finishFailedJob(t.Context(), store, jobFromAccepted(accepted), time.Now(), state.JobFailure{}); !errors.Is(err, stop) {
			t.Fatalf("reply lookup did not follow issue lookup: %v", err)
		}
		if !hasDeadline || remaining < 9*time.Second {
			t.Fatalf("reply lookup inherited issue lookup's spent budget: remaining=%s", remaining)
		}
	})
}

func TestSelectedRecoveryReleasesDeletedSource(t *testing.T) {
	at := time.Now().UTC()
	accepted := selectedAcceptance(t, at)
	accepted.Assigned = true
	store := openState(t, at.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{
		Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{
		currentItem: func(github.Item) (github.Item, error) {
			return github.Item{}, errors.New("source provenance unavailable")
		},
		threadItems: func(_ string, _ int, since time.Time) ([]github.Item, error) {
			if !since.IsZero() {
				t.Errorf("source check did not list the complete thread: %v", since)
			}
			return nil, nil // The complete live thread no longer contains the source.
		},
	}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
		t.Fatal("deleted source was dispatched")
		return nil
	}))
	if _, err := m.RunOnce(t.Context(), store); err != nil || store.Data.Accepted != nil ||
		len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 || len(gh.comments) != 0 || gh.threadCalls != 1 {
		t.Fatalf("deleted source kept a reservation or received a reply: err=%v state=%+v", err, store.Data)
	}
}

func TestSelectedDeletedIssueHTTPStatusReleasesOnlyOnGone(t *testing.T) {
	for _, test := range []struct {
		name     string
		assigned bool
		status   int
	}{
		{name: "unassigned thread 410", status: http.StatusGone},
		{name: "unassigned thread 404", status: http.StatusNotFound},
		{name: "terminal reply lookup 410", assigned: true, status: http.StatusGone},
		{name: "terminal reply lookup 404", assigned: true, status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			at := time.Now().UTC()
			accepted := selectedAcceptance(t, at)
			accepted.SubjectType = "Issue"
			accepted.SourceURL = "https://github.com/example-owner/example-repo/issues/7#issuecomment-1"
			accepted.Assigned = test.assigned
			path := filepath.Join(t.TempDir(), "state.json")
			store := openStateAt(t, path, at.Add(-time.Minute))
			store.Data.Accepted = &accepted
			store.Data.JobFailures[accepted.JobID] = state.JobFailure{
				Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
			}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			statusErr := &github.StatusError{StatusCode: test.status}
			gh := &fakeGitHub{}
			if test.assigned {
				gh.beforeVerify = func(_ context.Context, includeReviews bool) error {
					if includeReviews {
						t.Fatal("issue reply lookup included pull request reviews")
					}
					return statusErr
				}
			} else {
				gh.currentItem = func(github.Item) (github.Item, error) {
					return github.Item{}, errors.New("source node unavailable")
				}
				gh.threadItems = func(_ string, _ int, since time.Time) ([]github.Item, error) {
					if !since.IsZero() {
						t.Fatalf("source check did not list the complete thread: %v", since)
					}
					return nil, statusErr
				}
			}
			m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
				t.Fatal("deleted issue was dispatched")
				return nil
			}))
			next, err := m.RunOnce(t.Context(), store)
			if test.assigned {
				if len(gh.verifiedCalls) != 1 || gh.threadCalls != 0 {
					t.Fatalf("unexpected recovery lookups: replies=%v thread=%d", gh.verifiedCalls, gh.threadCalls)
				}
			} else if gh.threadCalls != 1 || len(gh.verifiedCalls) != 0 {
				t.Fatalf("unexpected recovery lookups: replies=%v thread=%d", gh.verifiedCalls, gh.threadCalls)
			}
			if test.status == http.StatusGone {
				if err != nil || store.Data.Accepted != nil || len(store.Data.Processed) != 1 ||
					len(store.Data.JobFailures) != 0 || len(gh.comments) != 0 || len(gh.reactions) != 0 {
					t.Fatalf("deleted issue kept global reservation or received a reply: err=%v state=%+v", err, store.Data)
				}
			} else {
				failure := store.Data.JobFailures[accepted.JobID]
				wantAttempts := 1
				if test.assigned {
					wantAttempts = maxJobAttempts
				}
				if !errors.Is(err, statusErr) || store.Data.Accepted == nil ||
					store.Data.Accepted.AttemptID != accepted.AttemptID || !store.Data.Accepted.Deadline.Equal(accepted.Deadline) ||
					!store.Data.Accepted.RetryAt.Equal(next) || !next.After(time.Now()) ||
					failure.Attempts != wantAttempts || len(store.Data.Processed) != 0 || len(gh.comments) != 0 {
					t.Fatalf("404 did not retain scheduled retry: next=%s err=%v state=%+v", next, err, store.Data)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openStateAt(t, path, at.Add(-time.Minute))
			if test.status == http.StatusGone {
				if reopened.Data.Accepted != nil || len(reopened.Data.Processed) != 1 || len(reopened.Data.JobFailures) != 0 {
					t.Fatalf("deleted issue reservation survived restart: %+v", reopened.Data)
				}
			} else if reopened.Data.Accepted == nil || !reopened.Data.Accepted.RetryAt.Equal(next) {
				t.Fatalf("404 retry schedule was not durable: %+v", reopened.Data)
			}
		})
	}
}
