package monitor

import (
	"errors"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

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
