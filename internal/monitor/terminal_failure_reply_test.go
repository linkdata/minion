package monitor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func TestTerminalControlFailureReplyRechecksLiveAuthority(t *testing.T) {
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
			job.AttemptID = "" // Terminal controls may outlive the selected reservation.
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

func TestIssuedFixPushSuppressesTerminalFailureReplyAndKeepsCheckpoint(t *testing.T) {
	sha := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name        string
		change      func(*state.PublicationWrite)
		pendingSHA  string
		omitPending bool
		uncertain   bool
		valid       bool
	}{
		{name: "issued", valid: true},
		{name: "uncertain", uncertain: true, valid: true},
		{name: "attempt changed", change: func(w *state.PublicationWrite) { w.AttemptID = "other-attempt" }},
		{name: "marker changed", change: func(w *state.PublicationWrite) { w.Marker = "<!-- linkdataminion:other -->" }},
		{name: "number changed", change: func(w *state.PublicationWrite) { w.Number++ }},
		{name: "SHA changed", pendingSHA: strings.Repeat("c", 40)},
		{name: "SHA missing", omitPending: true},
		{name: "fork changed", change: func(w *state.PublicationWrite) { w.Repository = "linkdataminion/other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			accepted := selectedAcceptance(t, now.Add(-3*time.Minute))
			accepted.SubjectType, accepted.CommandName, accepted.Action = "Issue", "fix", "fix"
			accepted.SourceURL = "https://github.com/example-owner/example-repo/issues/7#issuecomment-1"
			accepted.JobID = jobID(accepted.SourceKey, accepted.CommandName)
			accepted.Deadline = now.Add(-time.Minute)
			accepted.Assigned = true
			job := jobFromAccepted(accepted)
			path := filepath.Join(t.TempDir(), "state.json")
			store := openStateAt(t, path, now.Add(-4*time.Minute))
			store.Data.Accepted = &accepted
			failure := state.JobFailure{Attempts: maxJobAttempts, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
			store.Data.JobFailures[job.ID] = failure
			if !tc.omitPending {
				pendingSHA := tc.pendingSHA
				if pendingSHA == "" {
					pendingSHA = sha
				}
				store.Data.PendingFixes[job.ID] = state.PendingFix{SHA: pendingSHA}
			}
			branch := fmt.Sprintf("minion/fix-%d-%s", job.Number, job.ID[:12])
			push := state.PublicationWrite{
				AttemptID: job.AttemptID, Kind: "push", BotLogin: "linkdataminion", BotID: 8,
				Repository: "linkdataminion/example-repo", Number: job.Number, Marker: job.Marker(), Since: job.Source.UpdatedAt,
				Branch: branch, SHA: sha, StartedAt: accepted.AcceptedAt.Add(time.Minute),
				Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
			}
			if tc.change != nil {
				tc.change(&push)
			}
			if tc.uncertain {
				push.Status, push.Deadline, push.ResolvedAt = state.PublicationWriteUncertain, now.Add(-time.Second), now
			}
			store.Data.PublicationWrites[job.ID+"-push"] = push
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			gh := &fakeGitHub{}
			m := selectedMonitor(t, now, gh, nil)
			err := m.finishFailedJob(t.Context(), store, job, now, failure)
			if !tc.valid {
				if !errors.Is(err, ErrAcceptanceState) || gh.commentCalls != 0 ||
					store.Data.Accepted == nil || !store.Data.Processed[job.ID].IsZero() {
					t.Fatalf("invalid push suppressed or published a failure reply: comments=%d state=%+v error=%v",
						gh.commentCalls, store.Data, err)
				}
				return
			}
			if err != nil || gh.commentCalls != 0 || store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() ||
				store.Data.PendingFixes[job.ID].SHA != sha || store.Data.PublicationWrites[job.ID+"-push"] != push {
				t.Fatalf("issued push caused failure reply or lost its checkpoint: comments=%d state=%+v error=%v",
					gh.commentCalls, store.Data, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openStateAt(t, path, now)
			lookups := 0
			gh.branch = func(_ context.Context, repo, name string) (github.Branch, error) {
				lookups++
				if repo != push.Repository || name != branch {
					t.Errorf("reconciliation checked wrong branch: %s %s", repo, name)
				}
				return github.Branch{Name: branch, Commit: github.BranchCommit{SHA: sha}}, nil
			}
			wantLookups, wantStatus := 1, state.PublicationWriteObserved
			if tc.uncertain {
				wantLookups, wantStatus = 0, state.PublicationWriteUncertain
			}
			if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil || lookups != wantLookups ||
				store.Data.PendingFixes[job.ID].SHA != sha ||
				store.Data.PublicationWrites[job.ID+"-push"].Status != wantStatus {
				t.Fatalf("startup could not reconcile saved push: lookups=%d state=%+v error=%v", lookups, store.Data, err)
			}
		})
	}
}
