package monitor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func TestInProcessFailureReplyRechecksLiveAuthority(t *testing.T) {
	lookupErr := errors.New("permission lookup unavailable")
	for _, test := range []struct {
		name      string
		change    func(*fakeGitHub, Job)
		wantReply bool
		wantRetry bool
	}{
		{name: "valid after action deadline", wantReply: true},
		{name: "edited command", change: func(gh *fakeGitHub, job Job) {
			changed := job.Source
			changed.Edited = true
			gh.currentItem = func(github.Item) (github.Item, error) { return changed, nil }
		}},
		{name: "revoked permission", change: func(gh *fakeGitHub, _ Job) {
			gh.permission.Permission = "read"
		}},
		{name: "transient lookup", change: func(gh *fakeGitHub, _ Job) {
			gh.permissionErr = lookupErr
		}, wantRetry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			accepted := selectedAcceptance(t, now)
			job := jobFromAccepted(accepted)
			job.Source = selectedItem(now)
			job.AttemptID = "" // In-process actions have no selected reservation.
			job.AcceptedAt, job.Deadline = now.Add(-2*time.Minute), now.Add(-time.Minute)
			failure := state.JobFailure{Attempts: maxJobAttempts, AcceptedAt: job.AcceptedAt, Deadline: job.Deadline}
			store := openState(t, now)
			store.Data.JobFailures[job.ID] = failure
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			gh := &fakeGitHub{items: []github.Item{job.Source}, permission: github.Permission{Permission: "write"}}
			gh.currentItem = func(github.Item) (github.Item, error) { return job.Source, nil }
			if test.change != nil {
				test.change(gh, job)
			}
			m := selectedMonitor(t, now, gh, nil)
			err := m.finishFailedJob(t.Context(), store, job, now, failure)
			if test.wantRetry {
				if !errors.Is(err, lookupErr) || len(gh.comments) != 0 || !store.Data.Processed[job.ID].IsZero() ||
					store.Data.JobFailures[job.ID] != failure {
					t.Fatalf("transient authority failure was not retried: err=%v comments=%v state=%+v", err, gh.comments, store.Data)
				}
				return
			}
			if err != nil || (len(gh.comments) == 1) != test.wantReply || store.Data.Processed[job.ID].IsZero() ||
				len(store.Data.JobFailures) != 0 {
				t.Fatalf("failure reply ignored live authority: err=%v comments=%v state=%+v", err, gh.comments, store.Data)
			}
			if test.wantReply && !strings.Contains(gh.comments[0].Body, job.Marker()) {
				t.Fatalf("valid expired request lost its marked fixed reply: %+v", gh.comments)
			}
		})
	}
}
