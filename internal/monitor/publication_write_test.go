package monitor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func TestReconcilePublicationWriteObservesVerifiedMarker(t *testing.T) {
	for _, kind := range []string{"comment", "review"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store := openState(t, now)
				jobID := "issued-job"
				write := state.PublicationWrite{
					AttemptID: "attempt-1", Kind: kind, BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 7,
					Marker: "<!-- linkdataminion:issued-job -->", Since: now,
					StartedAt: now, Deadline: now.Add(2 * time.Minute), Status: state.PublicationWritePending,
				}
				store.Data.PublicationWrites[jobID] = write
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				gh := &fakeGitHub{}
				item := github.Item{Body: "published\n\n" + write.Marker, Author: github.User{ID: 8, Login: "renamed-bot"}, UpdatedAt: now}
				if kind == "review" {
					gh.reviewReplies = []github.Item{item}
				} else {
					gh.items = []github.Item{item}
				}
				m := &Monitor{Config: config.Config{BotLogin: "replacement-bot"}, GitHub: gh}
				if err := m.reconcilePublicationWrites(t.Context(), store, "", ""); err != nil {
					t.Fatal(err)
				}
				got := store.Data.PublicationWrites[jobID]
				if got.Status != state.PublicationWriteObserved || got.ResolvedAt.IsZero() || len(gh.verifiedCalls) != 1 ||
					gh.verifiedCalls[0] != (kind == "review") || gh.commentCalls != 0 || !publicationWriteWake(store, now).IsZero() {
					t.Fatalf("reconciliation did not preserve observed effect: write=%+v reviews=%v comments=%d", got, gh.verifiedCalls, gh.commentCalls)
				}
			})
		})
	}
}

func TestReconcileIssuedReviewRequiresReviewMarker(t *testing.T) {
	now := time.Now().UTC()
	store := openState(t, now)
	marker := "<!-- linkdataminion:issued-job -->"
	store.Data.PublicationWrites["issued-job"] = state.PublicationWrite{
		AttemptID: "attempt-1", Kind: "review", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 7,
		Marker: marker, Since: now, StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{items: []github.Item{{Body: "comment\n\n" + marker, Author: github.User{ID: 8, Login: "bot"}, UpdatedAt: now}}}
	m := &Monitor{Config: config.Config{BotLogin: "bot"}, GitHub: gh}
	if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if got := store.Data.PublicationWrites["issued-job"]; got.Status != state.PublicationWritePending || len(gh.verifiedCalls) != 1 {
		t.Fatalf("issue comment satisfied issued review: write=%+v lookups=%d", got, len(gh.verifiedCalls))
	}
}

func TestReconcilePublicationWriteExpiresWithoutRepost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store := openState(t, now)
		jobID := "issued-job"
		store.Data.PublicationWrites[jobID] = state.PublicationWrite{
			AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 7,
			Marker: "<!-- linkdataminion:issued-job -->", Since: now,
			StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &fakeGitHub{}
		m := &Monitor{Config: config.Config{BotLogin: "bot"}, GitHub: gh}
		if err := m.reconcilePublicationWrites(t.Context(), store, "", ""); err != nil {
			t.Fatal(err)
		}
		if got := store.Data.PublicationWrites[jobID]; got.Status != state.PublicationWritePending ||
			!withPublicationWriteWake(now.Add(time.Hour), store, now).Equal(now.Add(publicationRecheckInterval)) {
			t.Fatalf("pending write lost its fixed wake: %+v", got)
		}
		time.Sleep(time.Minute)
		if err := m.reconcilePublicationWrites(t.Context(), store, "", ""); err != nil {
			t.Fatal(err)
		}
		got := store.Data.PublicationWrites[jobID]
		if got.Status != state.PublicationWriteUncertain || got.ResolvedAt.IsZero() || len(gh.verifiedCalls) != 2 ||
			gh.commentCalls != 0 || !publicationWriteWake(store, time.Now()).IsZero() {
			t.Fatalf("expired write was retried or not recorded: write=%+v checks=%d comments=%d", got, len(gh.verifiedCalls), gh.commentCalls)
		}
	})
}

func TestReconcileFixPushRequiresExactSHAUntilOriginalGraceExpires(t *testing.T) {
	for _, tc := range []struct {
		name, remoteSHA string
	}{
		{name: "matching", remoteSHA: strings.Repeat("b", 40)},
		{name: "mismatched", remoteSHA: strings.Repeat("c", 40)},
		{name: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				path := filepath.Join(t.TempDir(), "state.json")
				store := openStateAt(t, path, now)
				jobID := strings.Repeat("a", 64)
				branch := "minion/fix-7-aaaaaaaaaaaa"
				sha := strings.Repeat("b", 40)
				write := state.PublicationWrite{
					AttemptID: "attempt-1", Kind: "push", BotLogin: "bot", BotID: 8, Repository: "bot/repo", Number: 7,
					Marker: "<!-- linkdataminion:" + jobID + " -->", Since: now,
					Branch: branch, SHA: sha, StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
				}
				key := jobID + "-push"
				store.Data.PendingFixes[jobID] = state.PendingFix{SHA: sha}
				store.Data.PublicationWrites[key] = write
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				lookups := 0
				gh := &fakeGitHub{branch: func(_ context.Context, repo, name string) (github.Branch, error) {
					lookups++
					if repo != "bot/repo" || name != branch {
						t.Errorf("looked up wrong branch: %s %s", repo, name)
					}
					if tc.remoteSHA == "" {
						return github.Branch{}, &github.StatusError{StatusCode: 404}
					}
					return github.Branch{Name: branch, Commit: github.BranchCommit{SHA: tc.remoteSHA}}, nil
				}}
				m := &Monitor{Config: config.Config{BotLogin: "bot"}, GitHub: gh}
				if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store = openStateAt(t, path, now)
				got := store.Data.PublicationWrites[key]
				if tc.remoteSHA == sha {
					if got.Status != state.PublicationWriteObserved || got.ResolvedAt.IsZero() ||
						!got.Deadline.Equal(write.Deadline) || lookups != 1 || len(gh.verifiedCalls) != 0 {
						t.Fatalf("exact remote SHA was not observed after restart: write=%+v lookups=%d", got, lookups)
					}
					return
				}
				if got.Status != state.PublicationWritePending || !got.Deadline.Equal(write.Deadline) || lookups != 1 {
					t.Fatalf("unmatched branch changed grace before restart: write=%+v lookups=%d", got, lookups)
				}
				time.Sleep(time.Minute)
				if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
					t.Fatal(err)
				}
				got = store.Data.PublicationWrites[key]
				if got.Status != state.PublicationWriteUncertain || got.ResolvedAt.IsZero() ||
					got.Branch != branch || got.SHA != sha || !got.Deadline.Equal(write.Deadline) ||
					lookups != 2 || gh.commentCalls != 0 || len(gh.verifiedCalls) != 0 {
					t.Fatalf("unmatched branch did not become inspectable after grace: write=%+v lookups=%d", got, lookups)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store = openStateAt(t, path, now)
				if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil ||
					store.Data.PublicationWrites[key] != got || lookups != 2 {
					t.Fatalf("uncertain push was retried after restart: write=%+v lookups=%d error=%v",
						store.Data.PublicationWrites[key], lookups, err)
				}
			})
		})
	}
}

func TestReconcileFixPushRejectsChangedCommitCheckpoint(t *testing.T) {
	now := time.Now().UTC()
	store := openState(t, now)
	jobID := strings.Repeat("a", 64)
	store.Data.PendingFixes[jobID] = state.PendingFix{SHA: strings.Repeat("c", 40)}
	store.Data.PublicationWrites[jobID+"-push"] = state.PublicationWrite{
		AttemptID: "attempt-1", Kind: "push", BotLogin: "bot", BotID: 8, Repository: "bot/repo", Number: 7,
		Marker: "<!-- linkdataminion:" + jobID + " -->", Since: now,
		Branch: "minion/fix-7-aaaaaaaaaaaa", SHA: strings.Repeat("b", 40),
		StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	gh := &fakeGitHub{branch: func(context.Context, string, string) (github.Branch, error) {
		lookups++
		return github.Branch{}, nil
	}}
	m := &Monitor{GitHub: gh}
	if err := m.ReconcilePublicationWrites(t.Context(), store); !errors.Is(err, ErrAcceptanceState) || lookups != 0 {
		t.Fatalf("changed checkpoint reached remote observation: lookups=%d error=%v", lookups, err)
	}
}

func TestReconcilePublicationWriteChecksOnceAfterGraceOnNextPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store := openState(t, now)
		marker := "<!-- linkdataminion:issued-job -->"
		store.Data.PublicationWrites["issued-job"] = state.PublicationWrite{
			AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 7,
			Marker: marker, Since: now, StartedAt: now,
			Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &fakeGitHub{}
		m := &Monitor{Config: config.Config{BotLogin: "bot"}, GitHub: gh}
		time.Sleep(2 * time.Minute)
		gh.items = []github.Item{{Body: "published\n\n" + marker, Author: github.User{ID: 8, Login: "bot"}, UpdatedAt: now}}
		if _, err := m.RunOnce(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		if got := store.Data.PublicationWrites["issued-job"]; got.Status != state.PublicationWriteObserved ||
			len(gh.verifiedCalls) != 1 || gh.commentCalls != 0 {
			t.Fatalf("late one-pass check missed published marker: write=%+v checks=%d comments=%d", got, len(gh.verifiedCalls), gh.commentCalls)
		}
	})
}

func TestReconcileActiveTaskWriteUsesHalfRemainingLookup(t *testing.T) {
	for _, tc := range []struct {
		name, kind, attemptID string
		want                  time.Duration
	}{
		{name: "comment", kind: "comment", want: 2 * time.Second},
		{name: "review", kind: "review", want: 2 * time.Second},
		{name: "stale attempt", kind: "review", attemptID: "older-attempt", want: 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store, _, accepted, gh, m := storedRecoveryFixtureAt(t, now)
				m.Now = time.Now
				store.Data.Accepted.Deadline = now.Add(4 * time.Second)
				attemptID := accepted.AttemptID
				if tc.attemptID != "" {
					attemptID = tc.attemptID
				}
				write := state.PublicationWrite{
					AttemptID: attemptID, Kind: tc.kind, BotLogin: "linkdataminion", BotID: 8,
					Repository: accepted.Repository, Number: accepted.SubjectNumber,
					Marker: "<!-- linkdataminion:" + accepted.JobID + " -->", Since: accepted.SourceUpdatedAt,
					StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
				}
				store.Data.PublicationWrites[accepted.JobID] = write
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				gh.beforeVerify = func(ctx context.Context, _ bool) error {
					<-ctx.Done()
					return ctx.Err()
				}
				start := time.Now()
				if err := m.reconcilePublicationWrites(t.Context(), store, accepted.JobID, ""); err != nil {
					t.Fatal(err)
				}
				if elapsed := time.Since(start); elapsed != tc.want || len(gh.verifiedCalls) != 1 ||
					store.Data.PublicationWrites[accepted.JobID] != write {
					t.Fatalf("lookup held active task for %s, want %s; checks=%d write=%+v",
						elapsed, tc.want, len(gh.verifiedCalls), store.Data.PublicationWrites[accepted.JobID])
				}
			})
		})
	}
}

func TestActiveTaskWriteWakeUsesOriginalDeadline(t *testing.T) {
	for _, kind := range []string{"comment", "review"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store, _, accepted, _, _ := storedRecoveryFixtureAt(t, now)
				deadline := now.Add(500 * time.Millisecond)
				store.Data.Accepted.Deadline = deadline
				write := state.PublicationWrite{
					AttemptID: accepted.AttemptID, Kind: kind, BotLogin: "linkdataminion", BotID: 8,
					Repository: accepted.Repository, Number: accepted.SubjectNumber,
					Marker: "<!-- linkdataminion:" + accepted.JobID + " -->", Since: accepted.SourceUpdatedAt,
					StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
				}
				store.Data.PublicationWrites[accepted.JobID] = write
				if got := publicationWriteWake(store, now); !got.Equal(now.Add(250 * time.Millisecond)) {
					t.Fatalf("active %s wake = %s, want half remaining", kind, got)
				}
				if got := publicationWriteWake(store, deadline.Add(-50*time.Millisecond)); !got.Equal(deadline) {
					t.Fatalf("last %s wake = %s, want original deadline %s", kind, got, deadline)
				}
				if got := publicationWriteWake(store, deadline); !got.Equal(deadline.Add(publicationRecheckInterval)) {
					t.Fatalf("expired action %s wake = %s, want ordinary interval", kind, got)
				}
				write.AttemptID = "older-attempt"
				store.Data.PublicationWrites[accepted.JobID] = write
				if got := publicationWriteWake(store, now); !got.Equal(now.Add(publicationRecheckInterval)) {
					t.Fatalf("stale %s attempt got active wake %s", kind, got)
				}
			})
		})
	}
}

func TestObservedActiveTaskWriteClearsRetryOnce(t *testing.T) {
	for _, tc := range []struct {
		name, kind, attemptID string
		clears, notReady      bool
	}{
		{name: "comment", kind: "comment", clears: true},
		{name: "review", kind: "review", clears: true},
		{name: "stale attempt", kind: "review", attemptID: "older-attempt"},
		{name: "result not ready", kind: "review", notReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			store, path, accepted, _, _ := storedRecoveryFixtureAt(t, now)
			store.Data.Accepted.ResultReady = !tc.notReady
			retryAt := now.Add(30 * time.Second)
			store.Data.Accepted.RetryAt = retryAt
			attemptID := accepted.AttemptID
			if tc.attemptID != "" {
				attemptID = tc.attemptID
			}
			write := state.PublicationWrite{
				AttemptID: attemptID, Kind: tc.kind, BotLogin: "linkdataminion", BotID: 8,
				Repository: accepted.Repository, Number: accepted.SubjectNumber,
				Marker: "<!-- linkdataminion:" + accepted.JobID + " -->", Since: accepted.SourceUpdatedAt,
				StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
			}
			store.Data.PublicationWrites[accepted.JobID] = write
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			if changed, err := storePublicationWriteOutcome(store, accepted.JobID, write, state.PublicationWriteObserved, now); err != nil || !changed {
				t.Fatalf("observe %s: changed=%t err=%v", tc.name, changed, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openStateAt(t, path, now)
			gotRetry := store.Data.Accepted.RetryAt
			if got := store.Data.PublicationWrites[accepted.JobID]; got.Status != state.PublicationWriteObserved ||
				tc.clears && !gotRetry.IsZero() || !tc.clears && !gotRetry.Equal(retryAt) {
				t.Fatalf("observed %s did not retain correct retry: write=%+v retry=%s", tc.name, got, gotRetry)
			}
			store.Data.Accepted.RetryAt = retryAt
			if changed, err := storePublicationWriteOutcome(store, accepted.JobID, write, state.PublicationWriteObserved, now); err != nil || changed ||
				!store.Data.Accepted.RetryAt.Equal(retryAt) {
				t.Fatalf("observing %s twice changed retry again: changed=%t err=%v retry=%s",
					tc.name, changed, err, store.Data.Accepted.RetryAt)
			}
		})
	}
}

func TestReconcilePublicationWritesBoundsBacklogAndRotatesAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		path := filepath.Join(t.TempDir(), "state.json")
		store := openStateAt(t, path, now)
		ids := []string{"job-a", "job-b", "job-c"}
		for _, id := range ids {
			store.Data.PublicationWrites[id] = state.PublicationWrite{
				AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 7,
				Marker: "<!-- linkdataminion:" + id + " -->", Since: now.Add(-time.Minute),
				StartedAt: now.Add(-2 * time.Minute), Deadline: now.Add(-time.Minute), Status: state.PublicationWritePending,
			}
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &fakeGitHub{beforeVerify: func(ctx context.Context, _ bool) error {
			<-ctx.Done()
			return ctx.Err()
		}}
		m := &Monitor{Config: config.Config{BotLogin: "bot", Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2}}}, GitHub: gh}
		for pass, id := range ids {
			start := time.Now()
			if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed != publicationReconcileBudget {
				t.Fatalf("pass %d occupied monitor for %s, want %s", pass, elapsed, publicationReconcileBudget)
			}
			if len(gh.verifiedCalls) != pass+1 || store.Data.PublicationScanCursor != id ||
				store.Data.PublicationWrites[id].Status != state.PublicationWriteUncertain {
				t.Fatalf("pass %d did not advance one write: checks=%d cursor=%q write=%+v",
					pass, len(gh.verifiedCalls), store.Data.PublicationScanCursor, store.Data.PublicationWrites[id])
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openStateAt(t, path, time.Now())
		}
		if !publicationWriteWake(store, time.Now()).IsZero() {
			t.Fatal("resolved backlog still schedules publication checks")
		}
	})
}

func TestRunOnceWithoutWorkerWakesForIssuedPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store := openState(t, now)
		store.Data.PublicationWrites["issued-job"] = state.PublicationWrite{
			AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 7,
			Marker: "<!-- linkdataminion:issued-job -->", Since: now, StartedAt: now,
			Deadline: now.Add(2 * time.Minute), Status: state.PublicationWritePending,
		}
		store.Data.NextInviteCheckAt = now.Add(time.Hour)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &fakeGitHub{}
		m := &Monitor{Config: config.Config{BotLogin: "bot", PollIntervalSeconds: 600}, GitHub: gh}
		next, err := m.RunOnce(t.Context(), store)
		if err != nil || !next.Equal(now.Add(publicationRecheckInterval)) || len(gh.verifiedCalls) != 1 {
			t.Fatalf("workerless run skipped publication wake: next=%s err=%v checks=%d", next, err, len(gh.verifiedCalls))
		}
	})
}

func TestRunOncePendingTaskReplyPreemptsOlderWritesAndInvitations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedRecoveryFixtureAt(t, now)
		deadline := now.Add(41 * time.Second)
		store.Data.Accepted.Deadline = deadline
		store.Data.Accepted.RetryAt = deadline
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = deadline
		store.Data.JobFailures[accepted.JobID] = failure
		store.Data.PublicationWrites[accepted.JobID] = state.PublicationWrite{
			AttemptID: accepted.AttemptID, Kind: "review", BotLogin: "bot", BotID: 8, Repository: accepted.Repository, Number: accepted.SubjectNumber,
			Marker: "<!-- linkdataminion:" + accepted.JobID + " -->", Since: accepted.SourceUpdatedAt,
			StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 9,
			Marker: "<!-- linkdataminion:old-job -->", Since: now, StartedAt: now,
			Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
		store.Data.NextInviteCheckAt = now
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		invitationChecks := 0
		gh.beforeList = func() { invitationChecks++ }
		next, err := m.RunOnce(t.Context(), store)
		if err != nil || !next.Equal(now.Add(20500*time.Millisecond)) || len(gh.verifiedCalls) != 1 || !gh.verifiedCalls[0] || invitationChecks != 0 ||
			store.Data.Accepted == nil || store.Data.Accepted.JobID != accepted.JobID ||
			store.Data.PublicationWrites["old-job"].Status != state.PublicationWritePending {
			t.Fatalf("pending reply did not preempt unrelated checks: next=%s err=%v checks=%d invitations=%d accepted=%+v write=%+v",
				next, err, len(gh.verifiedCalls), invitationChecks, store.Data.Accepted, store.Data.PublicationWrites["old-job"])
		}
	})
}

func TestSelectedCapacityWaitRechecksIssuedPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store := openState(t, now)
		marker := "<!-- linkdataminion:issued-job -->"
		store.Data.PublicationWrites["issued-job"] = state.PublicationWrite{
			AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 7,
			Marker: marker, Since: now, StartedAt: now,
			Deadline: now.Add(2 * time.Minute), Status: state.PublicationWritePending,
		}
		store.Data.NextInviteCheckAt = now.Add(time.Hour)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &synchronizedRecoveryGitHub{fakeGitHub: &fakeGitHub{}}
		handler := &heldCapacityHandler{entered: make(chan struct{}), release: make(chan struct{})}
		m := &Monitor{Config: config.Config{BotLogin: "bot"}, GitHub: gh, Handler: handler, SelectedExecutorID: "local"}
		finished := make(chan error, 1)
		go func() { _, err := m.RunOnce(t.Context(), store); finished <- err }()
		<-handler.entered
		synctest.Wait()
		gh.mu.Lock()
		initialChecks := len(gh.verifiedCalls)
		gh.mu.Unlock()
		if initialChecks != 1 {
			t.Fatalf("initial issued write check count = %d, want 1", initialChecks)
		}
		time.Sleep(45 * time.Second)
		gh.mu.Lock()
		gh.items = []github.Item{{Body: "published\n\n" + marker, Author: github.User{ID: 8, Login: "bot"}, UpdatedAt: now}}
		gh.mu.Unlock()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		var got state.PublicationWrite
		_ = store.WithLock(func() error { got = store.Data.PublicationWrites["issued-job"]; return nil })
		gh.mu.Lock()
		checks, polls := len(gh.verifiedCalls), len(gh.notificationCalls)
		gh.mu.Unlock()
		if got.Status != state.PublicationWriteObserved || checks < 2 || polls != 0 {
			t.Fatalf("capacity wait skipped marker checks: write=%+v checks=%d polls=%d", got, checks, polls)
		}
		close(handler.release)
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	})
}

func TestReconcilePublicationWritesPrioritizesAcceptedJob(t *testing.T) {
	now := time.Now().UTC()
	store := openState(t, now)
	for _, id := range []string{"old-job", "active-job"} {
		store.Data.PublicationWrites[id] = state.PublicationWrite{
			AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 7,
			Marker: "<!-- linkdataminion:" + id + " -->", Since: now,
			StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{items: []github.Item{{Body: "published\n\n<!-- linkdataminion:active-job -->", Author: github.User{ID: 8, Login: "bot"}, UpdatedAt: now}}}
	m := &Monitor{Config: config.Config{BotLogin: "bot"}, GitHub: gh}
	if err := m.reconcilePublicationWrites(t.Context(), store, "active-job", ""); err != nil {
		t.Fatal(err)
	}
	if len(gh.verifiedCalls) != 1 || store.Data.PublicationWrites["active-job"].Status != state.PublicationWriteObserved ||
		store.Data.PublicationWrites["old-job"].Status != state.PublicationWritePending {
		t.Fatalf("unrelated marker lookup delayed active recovery: calls=%d writes=%+v", len(gh.verifiedCalls), store.Data.PublicationWrites)
	}
}

func TestIssuedTaskWriteSuppressesFailureReply(t *testing.T) {
	now := time.Now().UTC()
	store := openState(t, now)
	job := Job{
		ID: "issued-job", Repository: config.Repository{Name: "owner/repo"}, SubjectType: "Issue", Number: 7,
		Source: github.Item{Key: "source", UpdatedAt: now}, Command: command.Command{Name: "ask", Action: command.Comment},
	}
	store.Data.PublicationWrites[job.ID] = state.PublicationWrite{
		AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: job.Repository.Name, Number: job.Number,
		Marker: job.Marker(), Since: now, StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
	}
	failure := state.JobFailure{Attempts: maxJobAttempts}
	store.Data.JobFailures[job.ID] = failure
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{}
	m := &Monitor{Config: config.Config{BotLogin: "bot"}, GitHub: gh}
	if err := m.finishFailedJob(t.Context(), store, job, now, failure); err != nil {
		t.Fatal(err)
	}
	if gh.commentCalls != 0 || len(gh.verifiedCalls) != 0 || store.Data.Processed[job.ID].IsZero() ||
		store.Data.PublicationWrites[job.ID].Status != state.PublicationWritePending {
		t.Fatalf("issued write caused a second reply or was lost: comments=%d checks=%d processed=%v write=%+v",
			gh.commentCalls, len(gh.verifiedCalls), store.Data.Processed[job.ID], store.Data.PublicationWrites[job.ID])
	}
}

func TestIssuedReviewStillSuppressesReplyAfterCommandRemap(t *testing.T) {
	now := time.Now().UTC()
	store := openState(t, now)
	job := Job{ID: "issued-job", Repository: config.Repository{Name: "owner/repo"}, SubjectType: "PullRequest", Number: 7,
		Source: github.Item{Key: "source", UpdatedAt: now}, Command: command.Command{Name: "ask", Action: command.Comment}}
	store.Data.PublicationWrites[job.ID] = state.PublicationWrite{
		AttemptID: "old-attempt", Kind: "review", BotLogin: "bot", BotID: 8, Repository: job.Repository.Name, Number: job.Number,
		Marker: job.Marker(), Since: now, StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
	}
	failure := state.JobFailure{Attempts: maxJobAttempts}
	store.Data.JobFailures[job.ID] = failure
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{}
	m := &Monitor{Config: config.Config{BotLogin: "bot"}, GitHub: gh}
	if err := m.finishFailedJob(t.Context(), store, job, now, failure); err != nil {
		t.Fatal(err)
	}
	if gh.commentCalls != 0 || len(gh.verifiedCalls) != 0 || store.Data.Processed[job.ID].IsZero() ||
		store.Data.PublicationWrites[job.ID].Kind != "review" {
		t.Fatalf("remapped command duplicated or lost the old review: comments=%d checks=%d write=%+v",
			gh.commentCalls, len(gh.verifiedCalls), store.Data.PublicationWrites[job.ID])
	}
}

func TestRunOnceReconcilesWriteAfterTaskCleanup(t *testing.T) {
	now := time.Now().UTC()
	store := openState(t, now)
	jobID := "issued-job"
	marker := "<!-- linkdataminion:issued-job -->"
	store.Data.Processed[jobID] = now
	store.Data.PublicationWrites[jobID] = state.PublicationWrite{
		AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 7,
		Marker: marker, Since: now, StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
	}
	store.Data.NextPollAt = now.Add(time.Hour)
	store.Data.NextInviteCheckAt = now.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{items: []github.Item{{Body: "published\n\n" + marker, Author: github.User{ID: 8, Login: "bot"}, UpdatedAt: now}}}
	handler := &fakeHandler{}
	m := &Monitor{Config: config.Config{BotLogin: "bot"}, GitHub: gh, Handler: handler}
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if got := store.Data.PublicationWrites[jobID]; got.Status != state.PublicationWriteObserved ||
		len(handler.jobs) != 0 || gh.commentCalls != 0 || store.Data.Processed[jobID].IsZero() {
		t.Fatalf("processed action did not reconcile independently: write=%+v jobs=%d comments=%d", got, len(handler.jobs), gh.commentCalls)
	}
}

func TestSelectedSavedReplyDefersOlderWriteUntilPublisherFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedRecoveryFixtureAt(t, now)
		m.Now = time.Now
		synchronizedGitHub := &synchronizedRecoveryGitHub{fakeGitHub: gh}
		m.GitHub = synchronizedGitHub
		store.Data.PublicationWrites[accepted.JobID] = state.PublicationWrite{
			AttemptID: accepted.AttemptID, Kind: "review", BotLogin: "linkdataminion", BotID: 8,
			Repository: accepted.Repository, Number: accepted.SubjectNumber,
			Marker: "<!-- linkdataminion:" + accepted.JobID + " -->", Since: accepted.SourceUpdatedAt,
			StartedAt: now, Deadline: now.Add(2 * time.Minute), Status: state.PublicationWritePending,
		}
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 9,
			Marker: "<!-- linkdataminion:old-job -->", Since: now,
			StartedAt: now, Deadline: now.Add(2 * time.Minute), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		release := make(chan struct{})
		finished := make(chan error, 1)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		go func() {
			finished <- m.runSelectedWithControls(ctx, store, jobFromAccepted(accepted), "", nil,
				func(context.Context, Job) error { close(started); <-release; return nil }, func(err error) error { return err })
		}()
		select {
		case <-started:
		case err := <-finished:
			t.Fatalf("selected worker ended before it started: %v", err)
		}
		synctest.Wait()
		synchronizedGitHub.mu.Lock()
		initialChecks := len(gh.verifiedCalls)
		synchronizedGitHub.mu.Unlock()
		time.Sleep(publicationRecheckInterval + time.Second)
		synctest.Wait()
		synchronizedGitHub.mu.Lock()
		periodicChecks := len(gh.verifiedCalls)
		synchronizedGitHub.mu.Unlock()
		completedEarly := false
		var runErr error
		select {
		case runErr = <-finished:
			completedEarly = true
		default:
		}
		close(release)
		if !completedEarly {
			runErr = <-finished
		}
		if initialChecks != 0 || periodicChecks != 0 || completedEarly || runErr != nil ||
			store.Data.PublicationWrites["old-job"].Status != state.PublicationWritePending {
			t.Fatalf("saved reply waited for older write checks: initial=%d periodic=%d completed_early=%t err=%v write=%+v",
				initialChecks, periodicChecks, completedEarly, runErr, store.Data.PublicationWrites["old-job"])
		}
		if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		if len(gh.verifiedCalls) != 2 {
			t.Fatalf("older writes did not resume after the publisher finished: checks=%d", len(gh.verifiedCalls))
		}
	})
}

func TestSelectedReviewNearDeadlineReconcilesOverdueWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedRecoveryFixtureAt(t, now)
		accepted.Deadline = now.Add(5 * time.Second)
		store.Data.Accepted.Deadline = accepted.Deadline
		store.Data.Accepted.ResultReady = false // This test models a live worker, not a saved result.
		marker := "<!-- linkdataminion:old-job -->"
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "bot", BotID: 8,
			Repository: "owner/repo", Number: 9, Marker: marker, Since: now.Add(-time.Minute),
			StartedAt: now.Add(-time.Minute), Deadline: now.Add(-time.Second), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh.items = []github.Item{{Body: "published\n\n" + marker, Author: github.User{Login: "bot", ID: 8}, UpdatedAt: now}}
		gh.currentItem = func(item github.Item) (github.Item, error) { return item, nil }
		ready := make(chan struct{})
		var released, clockReads atomic.Int32
		release := func() {
			if released.CompareAndSwap(0, 1) {
				close(ready)
			}
		}
		gh.beforeVerify = func(context.Context, bool) error { release(); return nil }
		m.Now = func() time.Time {
			if clockReads.Add(1) == 100 {
				release() // Stop the worker if a regression skips the overdue write and spins.
			}
			return time.Now()
		}
		err := m.runSelectedWithControls(t.Context(), store, jobFromAccepted(accepted), "", nil,
			func(context.Context, Job) error { <-ready; return nil }, func(err error) error { return err })
		if err != nil || store.Data.PublicationWrites["old-job"].Status != state.PublicationWriteObserved ||
			len(gh.verifiedCalls) != 1 {
			t.Fatalf("selected review skipped overdue write: err=%v write=%+v checks=%d clock reads=%d",
				err, store.Data.PublicationWrites["old-job"], len(gh.verifiedCalls), clockReads.Load())
		}
	})
}

func TestSelectedFixPushWaitsForHandlerBeforeReconciliation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		m.Now = time.Now
		store.Data.Accepted.Deadline = now.Add(90 * time.Second)
		store.Data.Accepted.RetryAt = time.Time{}
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = store.Data.Accepted.Deadline
		store.Data.JobFailures[accepted.JobID] = failure
		key := accepted.JobID + "-push"
		push := store.Data.PublicationWrites[key]
		push.Deadline = now.Add(10 * time.Second)
		store.Data.PublicationWrites[key] = push
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 9,
			Marker: "<!-- linkdataminion:old-job -->", Since: now,
			StartedAt: now, Deadline: now.Add(90 * time.Second), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		var lookups atomic.Int32
		var commentChecks atomic.Int32
		gh.beforeVerify = func(context.Context, bool) error { commentChecks.Add(1); return nil }
		gh.branch = func(context.Context, string, string) (github.Branch, error) {
			lookups.Add(1)
			return github.Branch{}, &github.StatusError{StatusCode: 404}
		}
		started := make(chan struct{})
		release := make(chan struct{})
		finished := make(chan error, 1)
		job := jobFromAccepted(*store.Data.Accepted)
		go func() {
			finished <- m.runSelectedWithControls(t.Context(), store, job, "", nil,
				func(context.Context, Job) error { close(started); <-release; return nil },
				func(err error) error { return err })
		}()
		select {
		case <-started:
		case err := <-finished:
			t.Fatalf("selected handler ended before push started: %v", err)
		}
		synctest.Wait()
		initialChecks := commentChecks.Load()
		if err := store.WithLock(func() error {
			store.Data.PublicationWrites[accepted.JobID] = state.PublicationWrite{
				AttemptID: accepted.AttemptID, Kind: "comment", BotLogin: "linkdataminion", BotID: 8,
				Repository: accepted.Repository, Number: accepted.SubjectNumber,
				Marker: "<!-- linkdataminion:" + accepted.JobID + " -->", Since: accepted.SourceUpdatedAt,
				StartedAt: now, Deadline: now.Add(90 * time.Second), Status: state.PublicationWritePending,
			}
			return store.Save()
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(publicationRecheckInterval + time.Second)
		synctest.Wait()
		var current state.PublicationWrite
		var taskWrite, olderWrite state.PublicationWrite
		_ = store.WithLock(func() error {
			current = store.Data.PublicationWrites[key]
			taskWrite = store.Data.PublicationWrites[accepted.JobID]
			olderWrite = store.Data.PublicationWrites["old-job"]
			return nil
		})
		periodicChecks := commentChecks.Load()
		pushLookups := lookups.Load()
		close(release)
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
		if initialChecks != 1 || periodicChecks != 1 || pushLookups != 0 || current.Status != state.PublicationWritePending ||
			taskWrite.Status != state.PublicationWritePending || olderWrite.Status != state.PublicationWritePending {
			t.Fatalf("saved fix scanned older writes before its handler returned: initial=%d periodic=%d push lookups=%d push=%+v task=%+v old=%+v",
				initialChecks, periodicChecks, pushLookups, current, taskWrite, olderWrite)
		}
		if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		if lookups.Load() != 1 || commentChecks.Load() != 3 {
			t.Fatalf("deferred writes were not checked after the handler returned: push=%d comments=%d",
				lookups.Load(), commentChecks.Load())
		}
	})
}

func TestRecoveredIssuedReviewSkipsLiveSourceRevalidation(t *testing.T) {
	for _, change := range []string{"source edited", "command remapped"} {
		t.Run(change, func(t *testing.T) {
			now := time.Now().UTC()
			store, _, accepted, gh, m := storedRecoveryFixtureAt(t, now)
			marker := "<!-- linkdataminion:" + accepted.JobID + " -->"
			store.Data.PublicationWrites[accepted.JobID] = state.PublicationWrite{
				AttemptID: accepted.AttemptID, Kind: "review", BotLogin: "linkdataminion", BotID: 8,
				Repository: accepted.Repository, Number: accepted.SubjectNumber,
				Marker: marker, Since: accepted.SourceUpdatedAt, StartedAt: now,
				Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
			}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			gh.reviewReplies = []github.Item{{Key: "review:1", Body: "published\n\n" + marker,
				Author: github.User{ID: 8, Login: "linkdataminion"}, UpdatedAt: now}}
			var sourceRead bool
			gh.currentItem = func(item github.Item) (github.Item, error) {
				if item.Key == accepted.SourceKey {
					sourceRead = true
					item.Edited = true
				}
				return item, nil
			}
			if change == "command remapped" {
				m.Config.Commands[accepted.CommandName] = config.CommandMapping{Action: "comment"}
			}
			handler := &recoveredResultHandler{publish: func(Job) error { return nil }}
			m.Handler = handler
			if _, err := m.RunOnce(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			if sourceRead || handler.publications != 1 || store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWriteObserved ||
				store.Data.TerminalDecisions[accepted.JobID].Outcome == "lost" || store.Data.Processed[accepted.JobID].IsZero() {
				t.Fatalf("issued review was revalidated or discarded: sourceRead=%t publications=%d write=%+v decision=%+v processed=%v",
					sourceRead, handler.publications, store.Data.PublicationWrites[accepted.JobID],
					store.Data.TerminalDecisions[accepted.JobID], store.Data.Processed[accepted.JobID])
			}
		})
	}
}
