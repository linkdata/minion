package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func TestTerminalControlFailureReplyRechecksLiveAuthority(t *testing.T) {
	lookupErr := errors.New("permission lookup unavailable")
	for _, test := range []struct {
		name      string
		change    func(*fakeGitHub, Job)
		wantReply bool
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
		}},
		{name: "404 lookup", change: func(gh *fakeGitHub, _ Job) {
			gh.permissionErr = &github.StatusError{StatusCode: 404}
		}},
		{name: "verified marker", change: func(gh *fakeGitHub, job Job) {
			gh.items = append(gh.items, github.Item{Body: "Already replied.\n\n" + job.Marker(),
				Author: github.User{Login: "linkdataminion", ID: 8}, UpdatedAt: job.Source.UpdatedAt})
			gh.currentItem = func(item github.Item) (github.Item, error) { return item, nil }
		}},
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
			err := m.finishFailedJob(t.Context(), store, job, now)
			if err != nil || (len(gh.comments) == 1) != test.wantReply || store.Data.Processed[job.ID].IsZero() ||
				len(store.Data.JobFailures) != 0 {
				t.Fatalf("failure reply ignored live authority: err=%v comments=%v state=%+v", err, gh.comments, store.Data)
			}
			if test.wantReply && (!strings.Contains(gh.comments[0].Body, job.Marker()) ||
				store.Data.PublicationWrites[job.ID].Status != state.PublicationWriteObserved) {
				t.Fatalf("valid expired request lost its marked fixed reply or checkpoint: %+v", store.Data)
			}
		})
	}
}

func TestSelectedFixedRefusalUsesOneTerminalReply(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*fakeGitHub, Job)
		posted  bool
		expired bool
	}{
		{name: "valid", posted: true},
		{name: "expired action", posted: true, expired: true},
		{name: "edited command", change: func(gh *fakeGitHub, _ Job) {
			gh.currentItem = func(item github.Item) (github.Item, error) {
				item.Edited = true
				return item, nil
			}
		}},
		{name: "revoked permission", change: func(gh *fakeGitHub, _ Job) { gh.permission.Permission = "read" }},
		{name: "permission lookup failed", change: func(gh *fakeGitHub, _ Job) { gh.permissionErr = errors.New("lookup failed") }},
		{name: "permission not found", change: func(gh *fakeGitHub, _ Job) {
			gh.permissionErr = &github.StatusError{StatusCode: 404}
		}},
		{name: "marker check canceled", change: func(gh *fakeGitHub, _ Job) {
			gh.beforeVerify = func(context.Context, bool) error { return context.Canceled }
		}},
		{name: "marker already present", change: func(gh *fakeGitHub, job Job) {
			gh.items = append(gh.items, github.Item{Body: "Earlier reply.\n\n" + job.Marker(),
				Author: github.User{Login: "linkdataminion", ID: 8}, UpdatedAt: job.Source.UpdatedAt})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			acceptedAt := now
			if tc.expired {
				acceptedAt = now.Add(-8 * time.Minute)
			}
			accepted := selectedAcceptance(t, acceptedAt)
			accepted.FixedRefusal = "`review` applies to pull requests."
			job := jobFromAccepted(accepted)
			store := openState(t, now.Add(-time.Minute))
			store.Data.Accepted = &accepted
			store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			gh := &fakeGitHub{items: []github.Item{selectedItem(acceptedAt)}, permission: github.Permission{Permission: "write"},
				currentItem: func(item github.Item) (github.Item, error) {
					if item.Key == job.Source.Key {
						return selectedItem(acceptedAt), nil
					}
					return item, nil
				}}
			if tc.change != nil {
				tc.change(gh, job)
			}
			m := selectedMonitor(t, now, gh, nil)
			if _, ok := m.acceptedRepository(accepted); !ok {
				t.Fatal("fixture selected repository is ineligible")
			}
			outcome := "failed"
			if tc.expired {
				outcome = "expired"
			}
			if err := m.completeSelected(t.Context(), store, job, now, ErrTerminalJob); err != nil ||
				store.Data.Processed[job.ID].IsZero() || store.Data.Accepted != nil ||
				store.Data.TerminalDecisions[job.ID].Outcome != outcome ||
				(len(gh.comments) == 1) != tc.posted || gh.commentCalls > 1 {
				t.Fatalf("fixed refusal terminal cleanup: err=%v comments=%v state=%+v", err, gh.comments, store.Data)
			}
			if tc.posted {
				want := accepted.FixedRefusal + "\n\n" + job.Marker()
				if gh.comments[0].Body != want || store.Data.PublicationWrites[job.ID].Status != state.PublicationWriteObserved {
					t.Fatalf("wrong fixed reply or checkpoint: comments=%v state=%+v", gh.comments, store.Data)
				}
			} else if len(store.Data.PublicationWrites) != 0 {
				t.Fatalf("precheck failure issued a POST: %+v", store.Data.PublicationWrites)
			}
		})
	}
}

func TestSelectedFixedRefusalDoesNotRetryRejectedPOST(t *testing.T) {
	now := time.Now().UTC()
	accepted := selectedAcceptance(t, now)
	accepted.FixedRefusal = "Fixed refusal."
	job := jobFromAccepted(accepted)
	store := openState(t, now.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{items: []github.Item{selectedItem(now)}, permission: github.Permission{Permission: "write"},
		commentErr: errors.New("rejected POST"), currentItem: func(item github.Item) (github.Item, error) {
			if item.Key == job.Source.Key {
				return selectedItem(now), nil
			}
			return item, nil
		}}
	m := selectedMonitor(t, now, gh, nil)
	if err := m.completeSelected(t.Context(), store, job, now, ErrTerminalJob); err != nil ||
		gh.commentCalls != 1 || store.Data.Processed[job.ID].IsZero() ||
		store.Data.TerminalDecisions[job.ID].Outcome != "failed" ||
		store.Data.PublicationWrites[job.ID].Status != state.PublicationWritePending {
		t.Fatalf("rejected fixed reply was retried or lost its checkpoint: err=%v calls=%d state=%+v", err, gh.commentCalls, store.Data)
	}
	if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil || gh.commentCalls != 1 {
		t.Fatalf("reconciliation reposted fixed reply: err=%v calls=%d", err, gh.commentCalls)
	}
}

func TestSelectedFixedRefusalRecoversBeforeAndAfterUncertainPOST(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		accepted := selectedAcceptance(t, now)
		accepted.FixedRefusal = "Fixed refusal."
		job := jobFromAccepted(accepted)
		path := filepath.Join(t.TempDir(), "state.json")
		store := openStateAt(t, path, now.Add(-time.Minute))
		store.Data.Accepted = &accepted
		store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = openStateAt(t, path, now)
		var gh *fakeGitHub
		gh = &fakeGitHub{items: []github.Item{selectedItem(now)}, permission: github.Permission{Permission: "write"},
			currentItem: func(item github.Item) (github.Item, error) {
				if item.Key == job.Source.Key {
					return selectedItem(now), nil
				}
				return item, nil
			},
			commentErr: errors.New("response lost"), beforeComment: func() {
				gh.items = append(gh.items, github.Item{Body: accepted.FixedRefusal + "\n\n" + job.Marker(),
					Author: github.User{Login: "linkdataminion", ID: 8}, UpdatedAt: now})
			}}
		m := selectedMonitor(t, now, gh, nil)
		m.Now = time.Now
		if err := m.recoverAccepted(t.Context(), store); err != nil || gh.commentCalls != 1 ||
			store.Data.TerminalDecisions[job.ID].Outcome != "failed" ||
			store.Data.PublicationWrites[job.ID].Status != state.PublicationWritePending {
			t.Fatalf("restart failed to finish one fixed reply: err=%v calls=%d state=%+v", err, gh.commentCalls, store.Data)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = openStateAt(t, path, now)
		gh.commentErr = nil
		if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil || gh.commentCalls != 1 ||
			store.Data.PublicationWrites[job.ID].Status != state.PublicationWriteObserved ||
			store.Data.TerminalDecisions[job.ID].Outcome != "failed" {
			t.Fatalf("lost response was reposted or changed the original outcome: err=%v calls=%d state=%+v", err, gh.commentCalls, store.Data)
		}
	})
}

func TestFailureReplySkipsIncompleteSourceIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*github.Item)
	}{
		{name: "missing key", change: func(item *github.Item) { item.Key = "" }},
		{name: "missing node ID", change: func(item *github.Item) { item.NodeID = "" }},
		{name: "missing author ID", change: func(item *github.Item) { item.Author.ID = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			item := selectedItem(now)
			test.change(&item)
			cfg := testConfig(t)
			job := Job{Repository: cfg.Repositories[0], SubjectType: "PullRequest", Number: 7, Source: item,
				Command: command.Command{Name: "review", Action: command.Review}, ID: jobID(item.Key, "review")}
			store := openState(t, now.Add(-time.Minute))
			store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: maxJobAttempts}
			gh := &fakeGitHub{items: []github.Item{item}, permission: github.Permission{Permission: "write"}}
			m := &Monitor{Config: cfg, BotID: 8, GitHub: gh}
			if err := m.finishFailedJobWithNotification(t.Context(), store, job, now, true); err != nil ||
				gh.commentCalls != 0 || store.Data.Processed[job.ID].IsZero() || len(store.Data.JobFailures) != 0 ||
				len(store.Data.PublicationWrites) != 0 {
				t.Fatalf("incomplete source identity allowed a fixed reply: err=%v comments=%d state=%+v",
					err, gh.commentCalls, store.Data)
			}
		})
	}
}

func TestTerminalFailureReplyLostResponseReconcilesMarkerAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		cfg := testConfig(t)
		item := selectedItem(now)
		job := Job{Repository: cfg.Repositories[0], SubjectType: "PullRequest", Number: 7, Source: item,
			Command: command.Command{Name: "review", Action: command.Review}, ID: jobID(item.Key, "review")}
		path := filepath.Join(t.TempDir(), "state.json")
		store := openStateAt(t, path, now.Add(-time.Minute))
		store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: maxJobAttempts}
		decision := state.TerminalDecision{AttemptID: "original-attempt", Outcome: "failed", DecidedAt: now}
		store.Data.TerminalDecisions[job.ID] = decision
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		var gh *fakeGitHub
		gh = &fakeGitHub{items: []github.Item{item}, permission: github.Permission{Permission: "write"},
			commentErr: errors.New("response lost"), beforeComment: func() {
				gh.items = append(gh.items, github.Item{Body: failureReply + "\n\n" + job.Marker(),
					Author: github.User{Login: cfg.BotLogin, ID: 8}, UpdatedAt: now})
			}}
		m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Now: time.Now}
		if err := m.finishFailedJob(t.Context(), store, job, now); err != nil ||
			gh.commentCalls != 1 || store.Data.Processed[job.ID].IsZero() ||
			store.Data.PublicationWrites[job.ID].Status != state.PublicationWritePending {
			t.Fatalf("lost response was not checkpointed: err=%v calls=%d state=%+v", err, gh.commentCalls, store.Data)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = openStateAt(t, path, now)
		gh.commentErr = nil
		if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil ||
			store.Data.PublicationWrites[job.ID].Status != state.PublicationWriteObserved ||
			store.Data.TerminalDecisions[job.ID] != decision {
			t.Fatalf("restart did not observe original reply and outcome: err=%v state=%+v", err, store.Data)
		}
		if err := m.finishFailedJob(t.Context(), store, job, now); err != nil ||
			gh.commentCalls != 1 {
			t.Fatalf("verified reply was posted again: err=%v calls=%d", err, gh.commentCalls)
		}
	})
}

func TestSelectedTerminalCleanupUsesDetachedContextAfterExecutorCancel(t *testing.T) {
	now := time.Now().UTC()
	accepted := selectedAcceptance(t, now)
	job := jobFromAccepted(accepted)
	store := openState(t, now.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: maxJobAttempts,
		AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{items: []github.Item{selectedItem(now)}, permission: github.Permission{Permission: "write"},
		currentItem: func(github.Item) (github.Item, error) { return selectedItem(now), nil }}
	m := selectedMonitor(t, now, gh, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.finishFailedJob(ctx, store, job, now); err != nil ||
		gh.commentCalls != 1 || store.Data.Processed[job.ID].IsZero() ||
		store.Data.PublicationWrites[job.ID].Status != state.PublicationWriteObserved {
		t.Fatalf("selected terminal cleanup did not finish after executor cancellation: err=%v state=%+v", err, store.Data)
	}
}

func TestFailureReplyCheckpointSaveFailurePreventsPOST(t *testing.T) {
	now := time.Now().UTC()
	cfg := testConfig(t)
	item := selectedItem(now)
	job := Job{Repository: cfg.Repositories[0], SubjectType: "PullRequest", Number: 7, Source: item,
		Command: command.Command{Name: "review", Action: command.Review}, ID: jobID(item.Key, "review")}
	path := filepath.Join(t.TempDir(), "state.json")
	backup := path + ".saved"
	store := openStateAt(t, path, now.Add(-time.Minute))
	store.Data.JobFailures[job.ID] = state.JobFailure{Attempts: maxJobAttempts}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{items: []github.Item{item}, permission: github.Permission{Permission: "write"}}
	m := &Monitor{Config: cfg, BotID: 8, GitHub: gh, Now: func() time.Time { return now }}
	if err := m.finishFailedJob(t.Context(), store, job, now); !errors.Is(err, ErrAcceptanceState) || gh.commentCalls != 0 ||
		!store.Data.Processed[job.ID].IsZero() || len(store.Data.PublicationWrites) != 0 {
		t.Fatalf("failed checkpoint allowed POST: err=%v calls=%d state=%+v", err, gh.commentCalls, store.Data)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStateAt(t, path, now)
	if err := m.finishFailedJob(t.Context(), store, job, now); err != nil ||
		gh.commentCalls != 1 || store.Data.PublicationWrites[job.ID].Status != state.PublicationWriteObserved {
		t.Fatalf("restored state could not make its first POST: err=%v calls=%d state=%+v", err, gh.commentCalls, store.Data)
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
			err := m.finishFailedJob(t.Context(), store, job, now)
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
