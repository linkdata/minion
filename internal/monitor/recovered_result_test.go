package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type recoveredResultHandler struct {
	executions   int
	publications int
	execute      func(Job) error
	publish      func(Job) error
}

type deadlineInvitationGitHub struct {
	*fakeGitHub
	checks  int
	entered chan struct{}
}

func (g *deadlineInvitationGitHub) ListRepositoryInvitations(ctx context.Context) ([]github.Invitation, error) {
	g.checks++
	if g.entered != nil {
		select {
		case g.entered <- struct{}{}:
		default:
		}
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type terminalUsageRecoveryHandler struct {
	recoveredResultHandler
	onTerminal func(Job, string, time.Time)
}

func (h *terminalUsageRecoveryHandler) CompleteTerminalUsage(job Job, outcome string, at time.Time) {
	h.onTerminal(job, outcome, at)
}

// fakeGitHub counts these reads without locking; recovery and terminal controls run concurrently.
type synchronizedRecoveryGitHub struct {
	*fakeGitHub
	mu sync.Mutex
}

func (g *synchronizedRecoveryGitHub) Repository(ctx context.Context, name string) (github.Repository, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeGitHub.Repository(ctx, name)
}

func (g *synchronizedRecoveryGitHub) CurrentItem(ctx context.Context, item github.Item) (github.Item, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeGitHub.CurrentItem(ctx, item)
}

func (g *synchronizedRecoveryGitHub) Permission(ctx context.Context, repo, login string) (github.Permission, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeGitHub.Permission(ctx, repo, login)
}

func (g *synchronizedRecoveryGitHub) HasVerifiedBotReply(ctx context.Context, repo string, number int, since time.Time, botID int64, marker string, includeReviews bool) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeGitHub.HasVerifiedBotReply(ctx, repo, number, since, botID, marker, includeReviews)
}

func (g *synchronizedRecoveryGitHub) HasVerifiedBotPublication(ctx context.Context, repo string, number int, since time.Time, bot github.User, marker, kind string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeGitHub.HasVerifiedBotPublication(ctx, repo, number, since, bot, marker, kind)
}

func (h *recoveredResultHandler) Execute(_ context.Context, job Job) error {
	h.executions++
	if h.execute != nil {
		return h.execute(job)
	}
	return errors.New("stored result was executed again")
}

func (h *recoveredResultHandler) PublishRecovered(_ context.Context, job Job) error {
	h.publications++
	if h.publish != nil {
		return h.publish(job)
	}
	return nil
}

func (*recoveredResultHandler) WaitCapacity(context.Context) error { return nil }

func storedRecoveryFixture(t *testing.T) (*state.Store, string, state.AcceptedAction, *fakeGitHub, *Monitor) {
	t.Helper()
	return storedRecoveryFixtureAt(t, time.Now().UTC())
}

func TestRecoveredTerminalDecisionReconcilesUsageBeforeCleanup(t *testing.T) {
	for _, outcome := range []string{"succeeded", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			store, path, accepted, _, monitor := storedRecoveryFixture(t)
			if err := store.WithLock(func() error {
				_, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, outcome, time.Now().UTC())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
			calls := 0
			handler := &terminalUsageRecoveryHandler{onTerminal: func(job Job, got string, at time.Time) {
				calls++
				if got != outcome || job.ID != accepted.JobID || at.IsZero() || reopened.Data.Accepted == nil {
					t.Errorf("terminal usage ran after cleanup or with wrong decision: job=%+v outcome=%q state=%+v", job, got, reopened.Data)
				}
			}}
			monitor.Handler = handler
			if _, err := monitor.RunOnce(t.Context(), reopened); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || handler.executions != 0 || handler.publications != 0 ||
				reopened.Data.Accepted != nil || reopened.Data.Processed[accepted.JobID].IsZero() {
				t.Fatalf("terminal decision reran work or skipped cleanup: usage=%d executions=%d publications=%d state=%+v",
					calls, handler.executions, handler.publications, reopened.Data)
			}
			if present, err := reopened.HasAcceptedResult(); err != nil || present {
				t.Fatalf("terminal decision retained accepted result: present=%t err=%v", present, err)
			}
		})
	}
}

func storedRecoveryFixtureAt(t *testing.T, at time.Time) (*state.Store, string, state.AcceptedAction, *fakeGitHub, *Monitor) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	accepted := selectedAcceptance(t, at)
	accepted.Assigned = true
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{
		Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
	}
	store.Data.NextPollAt = at.Add(time.Hour)
	store.Data.NextInviteCheckAt = at.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAcceptedResult([]byte(`{"result":"opaque-to-monitor"}`)); err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted.ResultReady = true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{
		permission:  github.Permission{Permission: "write"},
		currentItem: func(github.Item) (github.Item, error) { return selectedItem(at), nil },
	}
	return store, path, accepted, gh, selectedMonitor(t, at, gh, nil)
}

func storedTaskWriteFixtureAt(t *testing.T, now time.Time, remaining time.Duration, kind, status string) (
	*state.Store, string, state.AcceptedAction, *fakeGitHub, *Monitor, string,
) {
	t.Helper()
	store, path, accepted, gh, m := storedRecoveryFixtureAt(t, now)
	m.Now = time.Now
	accepted.Deadline = now.Add(remaining)
	store.Data.Accepted.Deadline = accepted.Deadline
	store.Data.Accepted.RetryAt = accepted.Deadline
	failure := store.Data.JobFailures[accepted.JobID]
	failure.Deadline = accepted.Deadline
	store.Data.JobFailures[accepted.JobID] = failure
	marker := "<!-- linkdataminion:" + accepted.JobID + " -->"
	write := state.PublicationWrite{
		AttemptID: accepted.AttemptID, Kind: kind, BotLogin: "linkdataminion", BotID: 8,
		Repository: accepted.Repository, Number: accepted.SubjectNumber,
		Marker: marker, Since: accepted.SourceUpdatedAt, StartedAt: now,
		Deadline: now.Add(time.Minute), Status: status,
	}
	if status == state.PublicationWriteObserved {
		write.ResolvedAt = now
	}
	store.Data.PublicationWrites[accepted.JobID] = write
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	return store, path, accepted, gh, m, marker
}

func TestObservedIssuedReviewBypassesAcceptedRetry(t *testing.T) {
	now := time.Now().UTC()
	store, _, accepted, gh, m := storedRecoveryFixtureAt(t, now)
	store.Data.Accepted.RetryAt = accepted.Deadline
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
	gh.currentItem = func(item github.Item) (github.Item, error) { return item, nil }
	handler := &recoveredResultHandler{publish: func(Job) error { return nil }}
	m.Handler = handler
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if handler.publications != 1 || store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWriteObserved ||
		store.Data.Processed[accepted.JobID].IsZero() || store.Data.Accepted != nil {
		t.Fatalf("observed task waited until accepted deadline: publications=%d write=%+v processed=%v accepted=%+v",
			handler.publications, store.Data.PublicationWrites[accepted.JobID], store.Data.Processed[accepted.JobID], store.Data.Accepted)
	}
}

func TestPendingTaskReplyObservedBeforeOriginalDeadline(t *testing.T) {
	for _, kind := range []string{"comment", "review"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store, _, accepted, gh, m, marker := storedTaskWriteFixtureAt(t, now, 500*time.Millisecond, kind, state.PublicationWritePending)
				deadline := accepted.Deadline
				gh.currentItem = func(item github.Item) (github.Item, error) { return item, nil }
				handler := &recoveredResultHandler{}
				m.Handler = handler
				next, err := m.RunOnce(t.Context(), store)
				if err != nil || !next.Equal(now.Add(250*time.Millisecond)) || len(gh.verifiedCalls) != 1 ||
					handler.publications != 0 || handler.executions != 0 {
					t.Fatalf("pending %s reply did not get a bounded check: next=%s checks=%d handler=%+v err=%v",
						kind, next, len(gh.verifiedCalls), handler, err)
				}
				time.Sleep(time.Until(next))
				published := github.Item{Body: "published\n\n" + marker,
					Author: github.User{ID: 8, Login: "linkdataminion"}, UpdatedAt: time.Now()}
				if kind == "review" {
					gh.reviewReplies = []github.Item{published}
				} else {
					gh.items = []github.Item{published}
				}
				if _, err := m.RunOnce(t.Context(), store); err != nil {
					t.Fatal(err)
				}
				if !time.Now().Before(deadline) || len(gh.verifiedCalls) != 2 || handler.executions != 0 ||
					handler.publications != 1 || gh.commentCalls != 0 || store.Data.Accepted != nil ||
					store.Data.Processed[accepted.JobID].IsZero() ||
					store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWriteObserved {
					t.Fatalf("pending %s reply did not finish once before deadline: elapsed=%s checks=%d handler=%+v comments=%d accepted=%+v processed=%v write=%+v",
						kind, time.Since(now), len(gh.verifiedCalls), handler, gh.commentCalls, store.Data.Accepted,
						store.Data.Processed[accepted.JobID], store.Data.PublicationWrites[accepted.JobID])
				}
			})
		})
	}
}

func TestObservedTaskReplyRetriesBeforeOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m, _ := storedTaskWriteFixtureAt(t, now, 2*time.Second, "review", state.PublicationWriteObserved)
		deadline := accepted.Deadline
		store.Data.Accepted.RetryAt = time.Time{}
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "linkdataminion", BotID: 8,
			Repository: accepted.Repository, Number: accepted.SubjectNumber,
			Marker: "<!-- linkdataminion:old-job -->", Since: now,
			StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh.beforeVerify = func(ctx context.Context, _ bool) error { <-ctx.Done(); return ctx.Err() }
		publishErr := errors.New("transient reply failure")
		handler := &recoveredResultHandler{publish: func(Job) error {
			if !time.Now().Before(deadline) {
				t.Error("retry started after original deadline")
			}
			if store.Data.Accepted != nil && store.Data.Accepted.RetryAt.IsZero() {
				return errors.Join(ErrPostModelPublication, publishErr)
			}
			return nil
		}}
		m.Handler = handler
		firstNext, err := m.RunOnce(t.Context(), store)
		wantRetry := now.Add(time.Second)
		if !errors.Is(err, ErrPostModelPublication) || !errors.Is(err, publishErr) || !firstNext.Equal(wantRetry) ||
			store.Data.Accepted == nil || !store.Data.Accepted.RetryAt.Equal(wantRetry) ||
			handler.publications != 1 || handler.executions != 0 || len(gh.verifiedCalls) != 0 {
			t.Fatalf("observed reply did not schedule bounded retry: next=%s accepted=%+v handler=%+v err=%v",
				firstNext, store.Data.Accepted, handler, err)
		}
		if next, err := m.RunOnce(t.Context(), store); err != nil || !next.Equal(wantRetry) ||
			handler.publications != 1 || len(gh.verifiedCalls) != 0 {
			t.Fatalf("observed reply bypassed retry or scanned backlog: next=%s handler=%+v old checks=%d err=%v",
				next, handler, len(gh.verifiedCalls), err)
		}
		time.Sleep(time.Until(wantRetry))
		if _, err := m.RunOnce(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		if handler.publications != 2 || handler.executions != 0 || gh.commentCalls != 0 || len(gh.verifiedCalls) != 0 ||
			store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || !time.Now().Before(deadline) {
			t.Fatalf("observed reply did not complete once before deadline: elapsed=%s handler=%+v comments=%d accepted=%+v processed=%v",
				time.Since(now), handler, gh.commentCalls, store.Data.Accepted, store.Data.Processed[accepted.JobID])
		}
	})
}

func TestSavedTaskReplySkipsOlderScanWithElevenSecondsLeft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m, _ := storedTaskWriteFixtureAt(t, now, 11*time.Second, "review", state.PublicationWriteObserved)
		store.Data.Accepted.RetryAt = time.Time{}
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "linkdataminion", BotID: 8,
			Repository: accepted.Repository, Number: accepted.SubjectNumber,
			Marker: "<!-- linkdataminion:old-job -->", Since: now,
			StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh.beforeVerify = func(ctx context.Context, _ bool) error { <-ctx.Done(); return ctx.Err() }
		handler := &recoveredResultHandler{publish: func(Job) error { return ErrPostModelPublication }}
		m.Handler = handler
		next, err := m.RunOnce(t.Context(), store)
		if !errors.Is(err, ErrPostModelPublication) || time.Since(now) != 0 ||
			!next.Equal(now.Add(5500*time.Millisecond)) || len(gh.verifiedCalls) != 0 ||
			handler.publications != 1 || handler.executions != 0 || store.Data.Accepted == nil {
			t.Fatalf("saved reply waited for older scan: elapsed=%s next=%s old checks=%d handler=%+v accepted=%+v err=%v",
				time.Since(now), next, len(gh.verifiedCalls), handler, store.Data.Accepted, err)
		}
	})
}

func TestSavedReplyWithoutIssuedWriteRetriesBeforeOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedRecoveryFixtureAt(t, now)
		m.Now = time.Now
		deadline := now.Add(2 * time.Second)
		store.Data.Accepted.Deadline = deadline
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = deadline
		store.Data.JobFailures[accepted.JobID] = failure
		store.Data.NextInviteCheckAt = now
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "linkdataminion", BotID: 8,
			Repository: accepted.Repository, Number: accepted.SubjectNumber,
			Marker: "<!-- linkdataminion:old-job -->", Since: now,
			StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh.beforeVerify = func(ctx context.Context, _ bool) error { <-ctx.Done(); return ctx.Err() }
		invitationChecks := 0
		gh.beforeList = func() { invitationChecks++ }
		publishCalls := 0
		handler := &recoveredResultHandler{publish: func(Job) error {
			publishCalls++
			if publishCalls == 1 {
				return ErrPostModelPublication
			}
			return nil
		}}
		m.Handler = handler
		next, err := m.RunOnce(t.Context(), store)
		if !errors.Is(err, ErrPostModelPublication) || time.Since(now) != 0 ||
			!next.Equal(now.Add(time.Second)) || len(gh.verifiedCalls) != 0 || invitationChecks != 0 ||
			handler.publications != 1 || handler.executions != 0 || store.Data.Accepted == nil ||
			!store.Data.Accepted.RetryAt.Equal(next) || store.Data.PublicationWrites[accepted.JobID].Status != "" {
			t.Fatalf("saved reply waited for unrelated checks before its first write: elapsed=%s next=%s old checks=%d invitations=%d handler=%+v accepted=%+v err=%v",
				time.Since(now), next, len(gh.verifiedCalls), invitationChecks, handler, store.Data.Accepted, err)
		}
		time.Sleep(time.Until(next))
		if _, err := m.RunOnce(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		if !time.Now().Before(deadline) || handler.publications != 2 || handler.executions != 0 ||
			store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || len(gh.verifiedCalls) != 0 {
			t.Fatalf("saved reply retry did not complete before deadline: elapsed=%s old checks=%d handler=%+v accepted=%+v processed=%v",
				time.Since(now), len(gh.verifiedCalls), handler, store.Data.Accepted, store.Data.Processed[accepted.JobID])
		}
	})
}

func TestLiveResultReturnCancelsOlderPublicationLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedRecoveryFixtureAt(t, now)
		m.Now = time.Now
		store.Data.Accepted.ResultReady = false // The receipt becomes ready while Execute runs.
		store.Data.Accepted.Deadline = now.Add(2 * time.Second)
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = store.Data.Accepted.Deadline
		store.Data.JobFailures[accepted.JobID] = failure
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "linkdataminion", BotID: 8,
			Repository: accepted.Repository, Number: accepted.SubjectNumber,
			Marker: "<!-- linkdataminion:old-job -->", Since: now,
			StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{}, 1)
		gh.beforeVerify = func(ctx context.Context, _ bool) error {
			entered <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		job := jobFromAccepted(*store.Data.Accepted)
		err := m.runSelectedWithControls(t.Context(), store, job, "", nil,
			func(context.Context, Job) error {
				<-entered
				return store.WithLock(func() error {
					store.Data.Accepted.ResultReady = true
					if err := store.Save(); err != nil {
						return err
					}
					return ErrPostModelPublication
				})
			}, func(err error) error { return err })
		if !errors.Is(err, ErrPostModelPublication) || errors.Is(err, ErrAcceptanceState) ||
			time.Since(now) != 0 || len(gh.verifiedCalls) != 1 || store.Data.Accepted == nil {
			t.Fatalf("completed live result waited for older lookup: elapsed=%s checks=%d accepted=%+v err=%v",
				time.Since(now), len(gh.verifiedCalls), store.Data.Accepted, err)
		}
	})
}

func TestObservedTaskRetryPreemptsUnrelatedChecksWithThirtyOneSecondsLeft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m, _ := storedTaskWriteFixtureAt(t, now, 31*time.Second, "review", state.PublicationWriteObserved)
		retryAt := now.Add(15500 * time.Millisecond)
		store.Data.Accepted.RetryAt = retryAt
		store.Data.NextInviteCheckAt = now
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "linkdataminion", BotID: 8,
			Repository: accepted.Repository, Number: accepted.SubjectNumber,
			Marker: "<!-- linkdataminion:old-job -->", Since: now,
			StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh.beforeVerify = func(ctx context.Context, _ bool) error { <-ctx.Done(); return ctx.Err() }
		invitationChecks := 0
		gh.beforeList = func() { invitationChecks++ }
		next, err := m.RunOnce(t.Context(), store)
		if err != nil || !next.Equal(retryAt) || time.Since(now) != 0 ||
			len(gh.verifiedCalls) != 0 || invitationChecks != 0 || store.Data.Accepted == nil {
			t.Fatalf("observed retry waited for unrelated checks: next=%s elapsed=%s old checks=%d invitations=%d accepted=%+v err=%v",
				next, time.Since(now), len(gh.verifiedCalls), invitationChecks, store.Data.Accepted, err)
		}
	})
}

func TestObservedTaskReplySaveFailureRetriesBeforeOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, path, accepted, gh, m, marker := storedTaskWriteFixtureAt(t, now, 500*time.Millisecond, "review", state.PublicationWritePending)
		deadline := accepted.Deadline
		gh.currentItem = func(item github.Item) (github.Item, error) { return item, nil }
		gh.reviewReplies = []github.Item{{Body: "published\n\n" + marker,
			Author: github.User{ID: 8, Login: "linkdataminion"}, UpdatedAt: now}}
		handler := &recoveredResultHandler{}
		m.Handler = handler
		backup := path + ".saved"
		if err := os.Rename(path, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "block"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		next, err := m.RunOnce(t.Context(), store)
		if !errors.Is(err, ErrPostModelPublication) || errors.Is(err, ErrAcceptanceState) ||
			!next.Equal(now.Add(250*time.Millisecond)) || handler.publications != 0 || handler.executions != 0 {
			t.Fatalf("observed reply save failure did not retry: next=%s handler=%+v err=%v", next, handler, err)
		}
		if err := os.Remove(filepath.Join(path, "block")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(backup, path); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Until(next))
		if _, err := m.RunOnce(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		if !time.Now().Before(deadline) || handler.publications != 1 || handler.executions != 0 ||
			gh.commentCalls != 0 || store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
			t.Fatalf("observed reply did not finish after save retry: elapsed=%s handler=%+v comments=%d accepted=%+v processed=%v",
				time.Since(now), handler, gh.commentCalls, store.Data.Accepted, store.Data.Processed[accepted.JobID])
		}
	})
}

func TestUnrelatedPublicationSaveFailureRemainsFatal(t *testing.T) {
	now := time.Now().UTC()
	store, path, _, gh, m := storedRecoveryFixtureAt(t, now)
	marker := "<!-- linkdataminion:old-job -->"
	store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
		AttemptID: "old-attempt", Kind: "comment", BotLogin: "linkdataminion", BotID: 8,
		Repository: "owner/repo", Number: 9, Marker: marker, Since: now,
		StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh.currentItem = func(item github.Item) (github.Item, error) { return item, nil }
	gh.items = []github.Item{{Body: "published\n\n" + marker,
		Author: github.User{ID: 8, Login: "linkdataminion"}, UpdatedAt: now}}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "block"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.ReconcilePublicationWrites(t.Context(), store); !errors.Is(err, ErrAcceptanceState) ||
		errors.Is(err, ErrPostModelPublication) {
		t.Fatalf("unrelated publication save failure lost fatal classification: %v", err)
	}
}

func storedIssuedFixPushFixtureAt(t *testing.T, now time.Time) (*state.Store, string, state.AcceptedAction, *fakeGitHub, *Monitor) {
	t.Helper()
	store, path, _, gh, m := storedRecoveryFixtureAt(t, now)
	accepted := *store.Data.Accepted
	delete(store.Data.JobFailures, accepted.JobID)
	accepted.JobID = jobID(accepted.SourceKey, "fix")
	accepted.SubjectType, accepted.CommandName, accepted.Action = "Issue", "fix", "fix"
	accepted.SourceURL = "https://github.com/example-owner/example-repo/issues/7#issuecomment-1"
	accepted.Deadline = now.Add(15 * time.Second)
	accepted.RetryAt = accepted.Deadline
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{
		Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
	}
	sha := strings.Repeat("b", 40)
	branch := fmt.Sprintf("minion/fix-%d-%s", accepted.SubjectNumber, accepted.JobID[:12])
	store.Data.PendingFixes[accepted.JobID] = state.PendingFix{SHA: sha}
	store.Data.PublicationWrites[accepted.JobID+"-push"] = state.PublicationWrite{
		AttemptID: accepted.AttemptID, Kind: "push", BotLogin: "linkdataminion", BotID: 8,
		Repository: "linkdataminion/example-repo", Number: accepted.SubjectNumber,
		Marker: "<!-- linkdataminion:" + accepted.JobID + " -->", Since: accepted.SourceUpdatedAt,
		Branch: branch, SHA: sha, StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	fixItem := selectedItem(now)
	fixItem.Body = "@linkdataminion fix"
	fixItem.HTMLURL = accepted.SourceURL
	gh.currentItem = func(github.Item) (github.Item, error) { return fixItem, nil }
	gh.branch = func(_ context.Context, repo, name string) (github.Branch, error) {
		if repo != "linkdataminion/example-repo" || name != branch {
			t.Errorf("checked wrong fix branch: %s %s", repo, name)
		}
		return github.Branch{Name: branch, Commit: github.BranchCommit{SHA: sha}}, nil
	}
	return store, path, accepted, gh, m
}

func TestObservedIssuedFixPushBypassesAcceptedRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, time.Now().UTC())
		branchChecks := 0
		lookup := gh.branch
		gh.branch = func(ctx context.Context, repo, name string) (github.Branch, error) {
			branchChecks++
			return lookup(ctx, repo, name)
		}
		handler := &recoveredResultHandler{publish: func(job Job) error {
			if !time.Now().Before(job.Deadline) {
				t.Errorf("recovered fix publication started after its original deadline")
			}
			return nil
		}}
		m.Handler = handler
		if _, err := m.RunOnce(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		if branchChecks != 1 || handler.publications != 1 || handler.executions != 0 ||
			store.Data.PublicationWrites[accepted.JobID+"-push"].Status != state.PublicationWriteObserved ||
			store.Data.Processed[accepted.JobID].IsZero() || store.Data.Accepted != nil || gh.commentCalls != 0 {
			t.Fatalf("confirmed fix push waited until accepted deadline: checks=%d handler=%+v write=%+v processed=%v accepted=%+v comments=%d",
				branchChecks, handler, store.Data.PublicationWrites[accepted.JobID+"-push"],
				store.Data.Processed[accepted.JobID], store.Data.Accepted, gh.commentCalls)
		}
	})
}

func TestSlowActivePushCheckLeavesTimeForSavedFixReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		m.Now = time.Now
		deadline := now.Add(500 * time.Millisecond)
		store.Data.Accepted.Deadline = deadline
		store.Data.Accepted.RetryAt = deadline
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = deadline
		store.Data.JobFailures[accepted.JobID] = failure
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		branchChecks := 0
		lookup := gh.branch
		gh.branch = func(ctx context.Context, repo, branch string) (github.Branch, error) {
			branchChecks++
			if branchChecks == 1 {
				<-ctx.Done()
				return github.Branch{}, ctx.Err()
			}
			return lookup(ctx, repo, branch)
		}
		handler := &recoveredResultHandler{publish: func(job Job) error {
			if !time.Now().Before(job.Deadline) {
				t.Error("saved fix reply started after its original deadline")
			}
			return nil
		}}
		m.Handler = handler
		next, err := m.RunOnce(t.Context(), store)
		if err != nil || !next.After(time.Now()) || !next.Before(deadline) ||
			branchChecks != 1 || handler.publications != 0 || store.Data.Accepted == nil {
			t.Fatalf("slow push check used the reply window: next=%s elapsed=%s checks=%d publications=%d accepted=%+v err=%v",
				next, time.Since(now), branchChecks, handler.publications, store.Data.Accepted, err)
		}
		time.Sleep(time.Until(next))
		if _, err := m.RunOnce(t.Context(), store); err != nil || branchChecks != 2 || handler.publications != 1 ||
			store.Data.PublicationWrites[accepted.JobID+"-push"].Status != state.PublicationWriteObserved ||
			store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
			t.Fatalf("exact push did not resume saved reply: elapsed=%s checks=%d publications=%d write=%+v accepted=%+v err=%v",
				time.Since(now), branchChecks, handler.publications, store.Data.PublicationWrites[accepted.JobID+"-push"],
				store.Data.Accepted, err)
		}
	})
}

func TestSavedFixKeepsReserveBeforeOlderWriteScan(t *testing.T) {
	for _, test := range []struct {
		name              string
		remaining         time.Duration
		maxOldWriteChecks int
		observedPush      bool
	}{
		{name: "pending inside scan budget", remaining: fixPublicationReserve + time.Second, maxOldWriteChecks: 1},
		{name: "observed inside scan budget", remaining: fixPublicationReserve + time.Second, maxOldWriteChecks: 1, observedPush: true},
		{name: "pending before scan budget", remaining: fixPublicationReserve + publicationReconcileBudget + time.Second, maxOldWriteChecks: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
				m.Now = time.Now
				deadline := now.Add(test.remaining)
				store.Data.Accepted.Deadline = deadline
				store.Data.Accepted.RetryAt = time.Time{}
				failure := store.Data.JobFailures[accepted.JobID]
				failure.Deadline = deadline
				store.Data.JobFailures[accepted.JobID] = failure
				store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
					AttemptID: "old-attempt", Kind: "comment", BotLogin: "bot", BotID: 8,
					Repository: "owner/repo", Number: 9, Marker: "<!-- linkdataminion:old-job -->", Since: now,
					StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
				}
				if test.observedPush {
					push := store.Data.PublicationWrites[accepted.JobID+"-push"]
					push.Status, push.ResolvedAt = state.PublicationWriteObserved, now
					store.Data.PublicationWrites[accepted.JobID+"-push"] = push
				}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				gh.branch = func(context.Context, string, string) (github.Branch, error) {
					return github.Branch{}, &github.StatusError{StatusCode: 404}
				}
				gh.beforeVerify = func(ctx context.Context, _ bool) error { <-ctx.Done(); return ctx.Err() }
				handler := &recoveredResultHandler{publish: func(Job) error { return ErrPostModelPublication }}
				m.Handler = handler
				next, err := m.RunOnce(t.Context(), store)
				if !errors.Is(err, ErrPostModelPublication) || len(gh.verifiedCalls) > test.maxOldWriteChecks ||
					handler.publications != 1 || !next.Before(deadline) ||
					deadline.Sub(time.Now()) < fixPublicationReserve || store.Data.Accepted == nil {
					t.Fatalf("saved fix lost its publication reserve: elapsed=%s remaining=%s next=%s deadline=%s old checks=%d publications=%d accepted=%+v err=%v",
						time.Since(now), deadline.Sub(time.Now()), next, deadline, len(gh.verifiedCalls), handler.publications, store.Data.Accepted, err)
				}
			})
		})
	}
}

func TestStartupObservedFixPushRespectsLaterPublicationBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, path, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		if err := m.ReconcilePublicationWrites(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		if got := store.Data.Accepted.RetryAt; !got.IsZero() ||
			store.Data.PublicationWrites[accepted.JobID+"-push"].Status != state.PublicationWriteObserved {
			t.Fatalf("startup observation did not release accepted retry: retry=%s write=%+v",
				got, store.Data.PublicationWrites[accepted.JobID+"-push"])
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = openStateAt(t, path, now)
		if store.Data.Accepted == nil || !store.Data.Accepted.RetryAt.IsZero() {
			t.Fatalf("startup observation was not durable: accepted=%+v", store.Data.Accepted)
		}
		replyErr := errors.New("reply temporarily unavailable")
		handler := &recoveredResultHandler{publish: func(Job) error { return replyErr }}
		m.Handler = handler
		wantRetry := now.Add(accepted.Deadline.Sub(now) / 2)
		if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, replyErr) || handler.publications != 1 ||
			store.Data.Accepted == nil || !store.Data.Accepted.RetryAt.Equal(wantRetry) {
			t.Fatalf("observed push did not schedule failed reply: err=%v handler=%+v accepted=%+v",
				err, handler, store.Data.Accepted)
		}
		if _, err := m.RunOnce(t.Context(), store); err != nil || handler.publications != 1 ||
			store.Data.Accepted == nil || !store.Data.Accepted.RetryAt.Equal(wantRetry) || gh.commentCalls != 0 {
			t.Fatalf("observed push bypassed later publication backoff: err=%v handler=%+v accepted=%+v comments=%d",
				err, handler, store.Data.Accepted, gh.commentCalls)
		}
	})
}

func TestPostHandlerObservedFixPushGetsOneImmediateRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		key := accepted.JobID + "-push"
		push := store.Data.PublicationWrites[key]
		pending := store.Data.PendingFixes[accepted.JobID]
		delete(store.Data.PublicationWrites, key)
		delete(store.Data.PendingFixes, accepted.JobID)
		store.Data.Accepted.RetryAt = time.Time{}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		calls := 0
		handler := &recoveredResultHandler{publish: func(Job) error {
			calls++
			if calls == 1 {
				if err := store.WithLock(func() error {
					store.Data.PendingFixes[accepted.JobID] = pending
					store.Data.PublicationWrites[key] = push
					store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
						AttemptID: "old-attempt", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 9,
						Marker: "<!-- linkdataminion:old-job -->", Since: now,
						StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
					}
					store.Data.PublicationScanCursor = key
					return store.Save()
				}); err != nil {
					return err
				}
				gh.beforeVerify = func(context.Context, bool) error {
					time.Sleep(publicationReconcileBudget)
					return nil
				}
				return ErrPostModelPublication
			}
			return nil
		}}
		m.Handler = handler
		_, err := m.RunOnce(t.Context(), store)
		if err != nil || time.Since(now) != 0 || calls != 2 ||
			store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || gh.commentCalls != 0 ||
			store.Data.PublicationWrites[key].Status != state.PublicationWriteObserved || len(gh.verifiedCalls) != 0 {
			t.Fatalf("observed push waited or lost same-pass reply: err=%v elapsed=%s calls=%d accepted=%+v processed=%v comments=%d old checks=%d",
				err, time.Since(now), calls, store.Data.Accepted, store.Data.Processed[accepted.JobID], gh.commentCalls, len(gh.verifiedCalls))
		}
	})
}

func TestObservedFixPushImmediateRecoveryFailureUsesBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		m.Now = time.Now
		store.Data.Accepted.RetryAt = time.Time{}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		push := store.Data.PublicationWrites[accepted.JobID+"-push"]
		branchChecks := 0
		gh.branch = func(context.Context, string, string) (github.Branch, error) {
			branchChecks++
			if branchChecks == 1 {
				return github.Branch{}, &github.StatusError{StatusCode: 404}
			}
			return github.Branch{Name: push.Branch, Commit: github.BranchCommit{SHA: push.SHA}}, nil
		}
		handler := &recoveredResultHandler{publish: func(Job) error { return ErrPostModelPublication }}
		m.Handler = handler
		wantRetry := now.Add(accepted.Deadline.Sub(now) / 2)
		if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, ErrPostModelPublication) ||
			handler.publications != 2 || branchChecks != 2 || store.Data.Accepted == nil ||
			!store.Data.Accepted.RetryAt.Equal(wantRetry) || gh.commentCalls != 0 {
			t.Fatalf("same-pass retry did not stop after one publication failure: err=%v publications=%d checks=%d accepted=%+v comments=%d",
				err, handler.publications, branchChecks, store.Data.Accepted, gh.commentCalls)
		}
		if _, err := m.RunOnce(t.Context(), store); err != nil || handler.publications != 2 {
			t.Fatalf("recovered publication bypassed backoff: err=%v publications=%d", err, handler.publications)
		}
	})
}

func TestPendingFixPushRechecksBeforeOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		m.Now = time.Now
		deadline := now.Add(11 * time.Second)
		store.Data.Accepted.Deadline = deadline
		store.Data.Accepted.RetryAt = deadline
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = deadline
		store.Data.JobFailures[accepted.JobID] = failure
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 9,
			Marker: "<!-- linkdataminion:old-job -->", Since: now,
			StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		push := store.Data.PublicationWrites[accepted.JobID+"-push"]
		branchChecks := 0
		gh.branch = func(context.Context, string, string) (github.Branch, error) {
			branchChecks++
			if time.Since(now) < 8*time.Second {
				return github.Branch{}, &github.StatusError{StatusCode: 404}
			}
			return github.Branch{Name: push.Branch, Commit: github.BranchCommit{SHA: push.SHA}}, nil
		}
		handler := &recoveredResultHandler{publish: func(Job) error {
			if !time.Now().Before(deadline) {
				t.Error("recovered fix reply started after original deadline")
			}
			return nil
		}}
		m.Handler = handler
		for handler.publications == 0 {
			next, err := m.RunOnce(t.Context(), store)
			if err != nil {
				t.Fatal(err)
			}
			if handler.publications != 0 {
				break
			}
			if !next.After(time.Now()) || !next.Before(deadline) || len(gh.verifiedCalls) != 0 {
				t.Fatalf("pending push waited for deadline or unrelated backlog: next=%s elapsed=%s old checks=%d",
					next, time.Since(now), len(gh.verifiedCalls))
			}
			time.Sleep(time.Until(next))
		}
		if branchChecks != 3 || handler.publications != 1 || !time.Now().Before(deadline) ||
			store.Data.PublicationWrites[accepted.JobID+"-push"].Status != state.PublicationWriteObserved ||
			store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || gh.commentCalls != 0 {
			t.Fatalf("delayed branch visibility missed action deadline: checks=%d publications=%d elapsed=%s accepted=%+v write=%+v",
				branchChecks, handler.publications, time.Since(now), store.Data.Accepted,
				store.Data.PublicationWrites[accepted.JobID+"-push"])
		}
	})
}

func TestPendingFixPushServicesDueSiblingBeforeReserve(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		m.Now = time.Now
		deadline := now.Add(7 * time.Minute)
		store.Data.Accepted.Deadline = deadline
		store.Data.Accepted.RetryAt = deadline
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = deadline
		store.Data.JobFailures[accepted.JobID] = failure
		item, terminalID := queueTerminalControl(store, now.Add(-time.Second), 8,
			state.JobFailure{Attempts: maxJobAttempts}, time.Time{})
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 9,
			Marker: "<!-- linkdataminion:old-job -->", Since: now,
			StartedAt: now, Deadline: now.Add(2 * time.Minute), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		currentItem := gh.currentItem
		gh.currentItem = func(item github.Item) (github.Item, error) {
			if item.Key == accepted.SourceKey {
				return currentItem(item)
			}
			return item, nil
		}
		gh.threadItems = func(string, int, time.Time) ([]github.Item, error) {
			time.Sleep(30 * time.Second)
			return []github.Item{item}, nil
		}
		branchChecks := 0
		gh.branch = func(context.Context, string, string) (github.Branch, error) {
			branchChecks++
			return github.Branch{}, &github.StatusError{StatusCode: 404}
		}
		next, err := m.RunOnce(t.Context(), store)
		if err != nil || !terminalReplyRecorded(gh, store, terminalID) || branchChecks != 1 ||
			store.Data.Accepted == nil || store.Data.PublicationWrites[accepted.JobID+"-push"].Status != state.PublicationWritePending ||
			store.Data.PublicationWrites["old-job"].Status != state.PublicationWritePending ||
			time.Since(now) != 30*time.Second || !next.Equal(now.Add(time.Minute)) {
			t.Fatalf("pending push did not service sibling or recompute wake: err=%v replies=%v checks=%d accepted=%+v next=%s elapsed=%s",
				err, gh.comments, branchChecks, store.Data.Accepted, next, time.Since(now))
		}
		for _, includeReviews := range gh.verifiedCalls {
			if !includeReviews {
				t.Fatal("pending push scanned older publication write")
			}
		}
	})
}

func TestPendingFixPushDefersDueControlsNearDeadline(t *testing.T) {
	for _, control := range []string{"invitation", "terminal reply"} {
		t.Run(control, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
				m.Now = time.Now
				deadline := now.Add(15 * time.Second)
				store.Data.Accepted.Deadline = deadline
				store.Data.Accepted.RetryAt = deadline
				failure := store.Data.JobFailures[accepted.JobID]
				failure.Deadline = deadline
				store.Data.JobFailures[accepted.JobID] = failure
				var publishedAt time.Time
				checks := 0
				terminalID := ""
				if control == "invitation" {
					store.Data.NextInviteCheckAt = now
					gh.beforeList = func() {
						checks++
						if publishedAt.IsZero() {
							time.Sleep(30 * time.Second)
						}
					}
				} else {
					item, id := queueTerminalControl(store, now.Add(-time.Second), 8,
						state.JobFailure{Attempts: maxJobAttempts}, time.Time{})
					terminalID = id
					activeCurrent := gh.currentItem
					gh.currentItem = func(current github.Item) (github.Item, error) {
						if current.Key == accepted.SourceKey {
							return activeCurrent(current)
						}
						return current, nil
					}
					gh.threadItems = func(string, int, time.Time) ([]github.Item, error) {
						checks++
						if publishedAt.IsZero() {
							time.Sleep(30 * time.Second)
						}
						return []github.Item{item}, nil
					}
				}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				push := store.Data.PublicationWrites[accepted.JobID+"-push"]
				branchChecks := 0
				gh.branch = func(context.Context, string, string) (github.Branch, error) {
					branchChecks++
					if branchChecks == 1 {
						return github.Branch{}, &github.StatusError{StatusCode: 404}
					}
					return github.Branch{Name: push.Branch, Commit: github.BranchCommit{SHA: push.SHA}}, nil
				}
				handler := &recoveredResultHandler{publish: func(Job) error { publishedAt = time.Now(); return nil }}
				m.Handler = handler
				next, err := m.RunOnce(t.Context(), store)
				if err != nil || checks != 0 || handler.publications != 0 || store.Data.Accepted == nil ||
					time.Since(now) != 0 || !next.After(now) || !next.Before(deadline) {
					t.Fatalf("pending push waited for %s: next=%s err=%v checks=%d publications=%d accepted=%+v elapsed=%s",
						control, next, err, checks, handler.publications, store.Data.Accepted, time.Since(now))
				}
				time.Sleep(time.Second)
				if _, err := m.RunOnce(t.Context(), store); err != nil || handler.publications != 1 ||
					!publishedAt.Before(deadline) || store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
					t.Fatalf("due %s consumed recovered reply deadline: err=%v checks=%d publications=%d reply=%s deadline=%s accepted=%+v processed=%v",
						control, err, checks, handler.publications, publishedAt, deadline, store.Data.Accepted, store.Data.Processed[accepted.JobID])
				}
				if control == "invitation" {
					if _, err := m.RunOnce(t.Context(), store); err != nil {
						t.Fatal(err)
					}
				} else if store.Data.Processed[terminalID].IsZero() {
					t.Fatal("terminal reply was not serviced after selected publication")
				}
				if checks == 0 {
					t.Fatalf("deferred %s was never checked", control)
				}
			})
		})
	}
}

func TestPendingFixPushDefersInvitationWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		m.Now = time.Now
		deadline := now.Add(21 * time.Second)
		store.Data.Accepted.Deadline = deadline
		store.Data.Accepted.RetryAt = deadline
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = deadline
		store.Data.JobFailures[accepted.JobID] = failure
		store.Data.NextInviteCheckAt = now
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		wrapped := &deadlineInvitationGitHub{fakeGitHub: gh}
		m.GitHub = wrapped
		push := store.Data.PublicationWrites[accepted.JobID+"-push"]
		branchChecks := 0
		gh.branch = func(context.Context, string, string) (github.Branch, error) {
			branchChecks++
			if branchChecks == 1 {
				return github.Branch{}, &github.StatusError{StatusCode: 404}
			}
			return github.Branch{Name: push.Branch, Commit: github.BranchCommit{SHA: push.SHA}}, nil
		}
		var publishedAt time.Time
		handler := &recoveredResultHandler{publish: func(Job) error { publishedAt = time.Now(); return nil }}
		m.Handler = handler
		next, err := m.RunOnce(t.Context(), store)
		if err != nil || time.Since(now) != 0 || wrapped.checks != 0 || branchChecks != 1 ||
			store.Data.Accepted == nil || !next.After(time.Now()) || !next.Before(deadline) {
			t.Fatalf("pending push checked invitation before reserve: next=%s err=%v elapsed=%s invitations=%d branches=%d accepted=%+v",
				next, err, time.Since(now), wrapped.checks, branchChecks, store.Data.Accepted)
		}
		time.Sleep(time.Until(next))
		if _, err := m.RunOnce(t.Context(), store); err != nil || handler.publications != 1 ||
			!publishedAt.Before(deadline) || wrapped.checks != 0 ||
			store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
			t.Fatalf("reserved push window lost reply: err=%v elapsed=%s invitations=%d publications=%d reply=%s deadline=%s accepted=%+v",
				err, time.Since(now), wrapped.checks, handler.publications, publishedAt, deadline, store.Data.Accepted)
		}
	})
}

func TestSelectedFixReservesDeadlineFromRunningInvitation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		m.Now = time.Now
		deadline := now.Add(31 * time.Second)
		store.Data.Accepted.Deadline = deadline
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = deadline
		store.Data.JobFailures[accepted.JobID] = failure
		store.Data.NextInviteCheckAt = now
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		wrapped := &deadlineInvitationGitHub{fakeGitHub: gh}
		m.GitHub = wrapped
		err := m.runSelectedWithControls(t.Context(), store, jobFromAccepted(*store.Data.Accepted), "", nil,
			func(context.Context, Job) error { time.Sleep(2 * time.Second); return ErrPostModelPublication },
			func(err error) error { return err })
		if !errors.Is(err, ErrPostModelPublication) || errors.Is(err, context.DeadlineExceeded) ||
			time.Since(now) != 2*time.Second || wrapped.checks != 1 {
			t.Fatalf("in-flight fix lost control reserve: err=%v elapsed=%s invitations=%d",
				err, time.Since(now), wrapped.checks)
		}
	})
}

func TestSelectedFixDefersUncancelableSiblingTerminalReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		m.Now = time.Now
		deadline := now.Add(90 * time.Second)
		store.Data.Accepted.Deadline = deadline
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = deadline
		store.Data.JobFailures[accepted.JobID] = failure
		item, terminalID := queueTerminalControl(store, now.Add(-time.Second), 8,
			state.JobFailure{Attempts: maxJobAttempts}, time.Time{})
		store.Data.NextInviteCheckAt = now.Add(time.Hour)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh.currentItem = func(item github.Item) (github.Item, error) { return item, nil }
		var completedAt time.Time
		terminalChecks := 0
		gh.threadItems = func(string, int, time.Time) ([]github.Item, error) {
			terminalChecks++
			if completedAt.IsZero() {
				time.Sleep(2 * time.Minute) // This terminal reply lookup ignores cancellation.
			}
			return []github.Item{item}, nil
		}
		err := m.runSelectedWithControls(t.Context(), store, jobFromAccepted(*store.Data.Accepted), "", nil,
			func(context.Context, Job) error { time.Sleep(time.Second); completedAt = time.Now(); return nil },
			func(err error) error { return err })
		if err != nil || time.Since(now) != time.Second || terminalChecks != 0 ||
			store.Data.TerminalDecisions[accepted.JobID].Outcome != "succeeded" || !completedAt.Before(deadline) {
			t.Fatalf("sibling terminal reply delayed selected fix: err=%v elapsed=%s terminal checks=%d completed=%s decision=%+v",
				err, time.Since(now), terminalChecks, completedAt, store.Data.TerminalDecisions[accepted.JobID])
		}
		m.serviceTerminalControls(t.Context(), store)
		if terminalChecks == 0 || store.Data.Processed[terminalID].IsZero() {
			t.Fatalf("deferred sibling reply was not serviced: checks=%d processed=%v", terminalChecks, store.Data.Processed[terminalID])
		}
	})
}

func TestSelectedFixServicesDueSiblingWhileWorkerRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		m.Now = time.Now
		deadline := now.Add(7 * time.Minute)
		store.Data.Accepted.Deadline = deadline
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = deadline
		store.Data.JobFailures[accepted.JobID] = failure
		item, terminalID := queueTerminalControl(store, now.Add(-time.Second), 8,
			state.JobFailure{Attempts: maxJobAttempts}, time.Time{})
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		currentItem := gh.currentItem
		gh.currentItem = func(item github.Item) (github.Item, error) {
			if item.Key == accepted.SourceKey {
				return currentItem(item)
			}
			return item, nil
		}
		gh.threadItems = terminalThreads(map[int]github.Item{8: item})
		release := make(chan struct{})
		finished := make(chan error, 1)
		go func() {
			finished <- m.runSelectedWithControls(t.Context(), store, jobFromAccepted(*store.Data.Accepted), "", nil,
				func(ctx context.Context, _ Job) error {
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}, func(err error) error { return err })
		}()
		synctest.Wait()
		if !terminalReplyRecorded(gh, store, terminalID) || store.Data.Accepted == nil ||
			!store.Data.Processed[accepted.JobID].IsZero() {
			t.Fatalf("held fix did not service sibling: comments=%v processed=%v accepted=%+v",
				gh.comments, store.Data.Processed, store.Data.Accepted)
		}
		select {
		case err := <-finished:
			t.Fatalf("selected fix completed before worker release: %v", err)
		default:
		}
		close(release)
		synctest.Wait()
		if err := <-finished; err != nil || store.Data.TerminalDecisions[accepted.JobID].Outcome != "succeeded" {
			t.Fatalf("selected fix did not complete after sibling reply: err=%v decision=%+v",
				err, store.Data.TerminalDecisions[accepted.JobID])
		}
	})
}

func TestPendingFixPushGetsSubsecondWakeBeforeDeadline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining time.Duration
	}{
		{"half second", 500 * time.Millisecond},
		{"just over one second", 1100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
				m.Now = time.Now
				deadline := now.Add(tc.remaining)
				store.Data.Accepted.Deadline = deadline
				store.Data.Accepted.RetryAt = deadline
				failure := store.Data.JobFailures[accepted.JobID]
				failure.Deadline = deadline
				store.Data.JobFailures[accepted.JobID] = failure
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				checks := 0
				gh.branch = func(context.Context, string, string) (github.Branch, error) {
					checks++
					return github.Branch{}, &github.StatusError{StatusCode: 404}
				}
				next, err := m.RunOnce(t.Context(), store)
				if err != nil || checks != 1 || !next.Equal(now.Add(tc.remaining/2)) ||
					!next.Equal(publicationWriteWake(store, now)) || store.Data.Accepted == nil {
					t.Fatalf("pending push wake: next=%s want=%s deadline=%s err=%v checks=%d accepted=%+v",
						next, now.Add(tc.remaining/2), deadline, err, checks, store.Data.Accepted)
				}
			})
		})
	}
}

func TestPendingFixPushWakeIgnoresOverdueOtherWriteAfterRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		m.Now = time.Now
		deadline := now.Add(500 * time.Millisecond)
		store.Data.Accepted.Deadline = deadline
		store.Data.Accepted.RetryAt = time.Time{}
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = deadline
		store.Data.JobFailures[accepted.JobID] = failure
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 9,
			Marker: "<!-- linkdataminion:old-job -->", Since: now,
			StartedAt: now.Add(-time.Minute), Deadline: now.Add(-time.Second), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		branchChecks := 0
		gh.branch = func(context.Context, string, string) (github.Branch, error) {
			branchChecks++
			return github.Branch{}, &github.StatusError{StatusCode: 404}
		}
		handler := &recoveredResultHandler{publish: func(Job) error { return ErrPostModelPublication }}
		m.Handler = handler
		next, err := m.RunOnce(t.Context(), store)
		want := now.Add(250 * time.Millisecond)
		if !errors.Is(err, ErrPostModelPublication) || !next.Equal(want) ||
			branchChecks != 2 || handler.publications != 1 || len(gh.verifiedCalls) != 0 ||
			store.Data.Accepted == nil || store.Data.PublicationWrites[accepted.JobID+"-push"].Status != state.PublicationWritePending {
			t.Fatalf("overdue other write displaced pending push wake: next=%s want=%s err=%v branch checks=%d publications=%d old checks=%d accepted=%+v",
				next, want, err, branchChecks, handler.publications, len(gh.verifiedCalls), store.Data.Accepted)
		}
	})
}

func TestRecoveredFixPushResultPreemptsOlderWriteNearDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
		m.Now = time.Now
		deadline := now.Add(500 * time.Millisecond)
		store.Data.Accepted.Deadline = deadline
		store.Data.Accepted.RetryAt = time.Time{}
		failure := store.Data.JobFailures[accepted.JobID]
		failure.Deadline = deadline
		store.Data.JobFailures[accepted.JobID] = failure
		store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
			AttemptID: "old-attempt", Kind: "comment", BotLogin: "bot", BotID: 8, Repository: "owner/repo", Number: 9,
			Marker: "<!-- linkdataminion:old-job -->", Since: now,
			StartedAt: now, Deadline: now.Add(time.Minute), Status: state.PublicationWritePending,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		push := store.Data.PublicationWrites[accepted.JobID+"-push"]
		branchChecks := 0
		gh.branch = func(context.Context, string, string) (github.Branch, error) {
			branchChecks++
			if branchChecks == 1 {
				return github.Branch{}, &github.StatusError{StatusCode: 404}
			}
			return github.Branch{Name: push.Branch, Commit: github.BranchCommit{SHA: push.SHA}}, nil
		}
		gh.beforeVerify = func(ctx context.Context, _ bool) error { <-ctx.Done(); return ctx.Err() }
		var replyAt time.Time
		calls := 0
		handler := &recoveredResultHandler{publish: func(Job) error {
			calls++
			if calls == 1 {
				return ErrPostModelPublication
			}
			replyAt = time.Now()
			return nil
		}}
		m.Handler = handler
		_, err := m.RunOnce(t.Context(), store)
		if err != nil || branchChecks != 2 || len(gh.verifiedCalls) != 0 ||
			store.Data.PublicationWrites[accepted.JobID+"-push"].Status != state.PublicationWriteObserved ||
			handler.publications != 2 || !replyAt.Before(deadline) ||
			store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() ||
			store.Data.TerminalDecisions[accepted.JobID].Outcome != "succeeded" || gh.commentCalls != 0 {
			t.Fatalf("observed push did not publish one-shot reply before deadline: err=%v checks=%d old checks=%d publications=%d reply=%s deadline=%s accepted=%+v processed=%v decision=%+v comments=%d",
				err, branchChecks, len(gh.verifiedCalls), handler.publications, replyAt, deadline, store.Data.Accepted,
				store.Data.Processed[accepted.JobID], store.Data.TerminalDecisions[accepted.JobID], gh.commentCalls)
		}
	})
}

func TestCompletedFixDoesNotDelayPushRecoveryForInvitation(t *testing.T) {
	for _, phase := range []string{"immediate result", "check in flight"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store, _, accepted, gh, m := storedIssuedFixPushFixtureAt(t, now)
				m.Now = time.Now
				deadline := now.Add(25 * time.Second)
				if phase == "check in flight" {
					deadline = now.Add(35 * time.Second)
				}
				store.Data.Accepted.Deadline = deadline
				store.Data.Accepted.RetryAt = time.Time{}
				failure := store.Data.JobFailures[accepted.JobID]
				failure.Deadline = deadline
				store.Data.JobFailures[accepted.JobID] = failure
				store.Data.NextInviteCheckAt = now
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				push := store.Data.PublicationWrites[accepted.JobID+"-push"]
				branchChecks := 0
				gh.branch = func(context.Context, string, string) (github.Branch, error) {
					branchChecks++
					if branchChecks == 1 {
						return github.Branch{}, &github.StatusError{StatusCode: 404}
					}
					time.Sleep(9 * time.Second)
					return github.Branch{Name: push.Branch, Commit: github.BranchCommit{SHA: push.SHA}}, nil
				}
				wrapped := &deadlineInvitationGitHub{fakeGitHub: gh}
				if phase == "check in flight" {
					wrapped.entered = make(chan struct{}, 1)
				}
				m.GitHub = wrapped
				publishCalls := 0
				m.Handler = &recoveredResultHandler{publish: func(Job) error {
					publishCalls++
					if publishCalls == 1 {
						if wrapped.entered != nil {
							<-wrapped.entered
						}
						return ErrPostModelPublication
					}
					time.Sleep(12 * time.Second)
					return nil
				}}
				_, err := m.RunOnce(t.Context(), store)
				if err != nil || time.Since(now) != 21*time.Second || branchChecks != 2 || publishCalls != 2 ||
					store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() ||
					store.Data.TerminalDecisions[accepted.JobID].Outcome != "succeeded" ||
					(phase == "check in flight" && wrapped.checks != 1) {
					t.Fatalf("invitation delayed completed fix recovery: phase=%s err=%v elapsed=%s checks=%d publications=%d invitations=%d accepted=%+v processed=%v decision=%+v",
						phase, err, time.Since(now), branchChecks, publishCalls, wrapped.checks, store.Data.Accepted,
						store.Data.Processed[accepted.JobID], store.Data.TerminalDecisions[accepted.JobID])
				}
			})
		})
	}
}

func TestMonitorPreservesControlFailureFromStoredResultPublisher(t *testing.T) {
	store, _, _, _, monitor := storedRecoveryFixture(t)
	controlErr := errors.New("control state unavailable")
	handler := &recoveredResultHandler{publish: func(Job) error {
		return errors.Join(ErrAcceptedDiscarded, ErrAcceptanceState, controlErr)
	}}
	monitor.Handler = handler
	_, err := monitor.RunOnce(t.Context(), store)
	if !errors.Is(err, ErrAcceptanceState) || !errors.Is(err, controlErr) || handler.publications != 1 || store.Data.Accepted == nil {
		t.Fatalf("stored discard hid control failure: err=%v publications=%d accepted=%+v", err, handler.publications, store.Data.Accepted)
	}
}

func TestSelectedExecutionKeepsSavedResultAfterPublicationError(t *testing.T) {
	at := time.Now().UTC()
	store := openState(t, at.Add(-time.Minute))
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(at)},
		items:         []github.Item{selectedItem(at)},
		permission:    github.Permission{Permission: "write"},
		currentItem:   func(github.Item) (github.Item, error) { return selectedItem(at), nil },
	}
	publishErr := errors.New("publication unavailable")
	handler := &recoveredResultHandler{execute: func(job Job) error {
		if err := store.WithLock(func() error {
			accepted := store.Data.Accepted
			if accepted == nil || accepted.JobID != job.ID || accepted.AttemptID != job.AttemptID {
				return errors.New("execution lost accepted reservation")
			}
			accepted.Assigned = true
			if err := store.Save(); err != nil {
				return err
			}
			if err := store.WriteAcceptedResult([]byte(`{"result":"saved-before-error"}`)); err != nil {
				return err
			}
			store.Data.Accepted.ResultReady = true
			return store.Save()
		}); err != nil {
			return err
		}
		return errors.Join(ErrPostModelPublication, publishErr)
	}}
	monitor := selectedMonitor(t, at, gh, handler)
	next, err := monitor.RunOnce(t.Context(), store)
	if !errors.Is(err, publishErr) || !errors.Is(err, ErrPostModelPublication) ||
		store.Data.Accepted == nil || !store.Data.Accepted.Assigned ||
		!next.Equal(store.Data.Accepted.RetryAt) || !next.After(time.Now()) ||
		store.Data.JobFailures[store.Data.Accepted.JobID].Attempts != 1 ||
		len(store.Data.Processed) != 0 || len(gh.comments) != 0 ||
		handler.executions != 1 || handler.publications != 0 {
		t.Fatalf("post-model publication error lost saved result: next=%s err=%v state=%+v handler=%+v", next, err, store.Data, handler)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("post-model publication error lost sidecar: present=%t err=%v", present, err)
	}
}

func TestSelectedExecutionEndsPermanentSavedResult(t *testing.T) {
	at := time.Now().UTC()
	store := openState(t, at.Add(-time.Minute))
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(at)},
		items:         []github.Item{selectedItem(at)},
		permission:    github.Permission{Permission: "write"},
		currentItem:   func(github.Item) (github.Item, error) { return selectedItem(at), nil },
	}
	handler := &recoveredResultHandler{execute: func(job Job) error {
		if err := store.WithLock(func() error {
			store.Data.Accepted.Assigned = true
			if err := store.Save(); err != nil {
				return err
			}
			if err := store.WriteAcceptedResult([]byte(`{"result":"saved-before-error"}`)); err != nil {
				return err
			}
			store.Data.Accepted.ResultReady = true
			return store.Save()
		}); err != nil {
			return err
		}
		return errors.Join(ErrTerminalJob, ErrPostModelPublication, errors.New("reviewed fix has no changes"))
	}}
	monitor := selectedMonitor(t, at, gh, handler)
	if _, err := monitor.RunOnce(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if handler.executions != 1 || handler.publications != 0 || store.Data.Accepted != nil ||
		len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 || len(gh.comments) != 1 {
		t.Fatalf("permanent result kept retrying: handler=%+v state=%+v comments=%v", handler, store.Data, gh.comments)
	}
	if present, err := store.HasAcceptedResult(); err != nil || present {
		t.Fatalf("permanent result kept receipt: present=%t err=%v", present, err)
	}
}

func TestStoredResultRecoveryPublishesAfterRestartWithoutWorkerSkill(t *testing.T) {
	store, path, accepted, gh, monitor := storedRecoveryFixture(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	if err := os.Remove(filepath.Join(os.Getenv("HOME"), ".agents", "skills", "review", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	monitor.SelectedCommands = map[string]bool{"fix": true}
	handler := &recoveredResultHandler{publish: func(job Job) error {
		if job.ID != accepted.JobID || job.AttemptID != accepted.AttemptID ||
			job.Source.Key != accepted.SourceKey || job.Source.Author.ID != accepted.RequesterID ||
			!job.Source.UpdatedAt.Equal(accepted.SourceUpdatedAt) ||
			!job.Deadline.Equal(accepted.Deadline) {
			t.Errorf("publication lost accepted identity: %+v", job)
			return errors.New("publication lost accepted identity")
		}
		present, err := reopened.HasAcceptedResult()
		if err != nil || !present {
			t.Errorf("publication started without saved result: present=%t err=%v", present, err)
			return errors.New("publication started without saved result")
		}
		return nil
	}}
	monitor.Handler = handler
	if _, err := monitor.RunOnce(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	if handler.executions != 0 || handler.publications != 1 || gh.currentChecks != 1 || gh.checks != 1 ||
		len(gh.comments) != 0 || len(gh.reactions) != 0 {
		t.Fatalf("saved result was not published exactly once: handler=%+v github=%+v", handler, gh)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	finished := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	present, err := finished.HasAcceptedResult()
	if err != nil || present || finished.Data.Accepted != nil ||
		finished.Data.Processed[accepted.JobID].IsZero() || len(finished.Data.JobFailures) != 0 {
		t.Fatalf("publication was not durably completed: present=%t err=%v state=%+v", present, err, finished.Data)
	}
}

func TestUncommittedStoredResultDoesNotPublishAfterRestart(t *testing.T) {
	store, path, accepted, _, monitor := storedRecoveryFixture(t)
	store.Data.Accepted.ResultReady = false // The sidecar write was not accepted before cancellation.
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	handler := &recoveredResultHandler{}
	monitor.Handler = handler
	if _, err := monitor.RunOnce(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	if handler.executions != 0 || handler.publications != 0 ||
		reopened.Data.TerminalDecisions[accepted.JobID].Outcome != "lost" {
		t.Fatalf("uncommitted sidecar authorized publication: handler=%+v state=%+v", handler, reopened.Data)
	}
}

func TestUncommittedStoredResultDoesNotDeferLiveFailure(t *testing.T) {
	store, _, accepted, gh, monitor := storedRecoveryFixture(t)
	store.Data.Accepted.ResultReady = false
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	job := jobFromAccepted(accepted)
	failure := errors.Join(ErrPostModelPublication, errors.New("publication failed"))
	outcome, err := selectedCompletedOutcome(store, job, failure, nil, time.Now().UTC(), time.Time{})
	if err != nil || outcome != "failed" {
		t.Fatalf("uncommitted result deferred worker failure: outcome=%q error=%v", outcome, err)
	}
	if err := monitor.completeSelected(t.Context(), store, job, time.Now().UTC(), failure); err != nil {
		t.Fatal(err)
	}
	if store.Data.TerminalDecisions[job.ID].Outcome != "failed" || store.Data.Accepted != nil ||
		store.Data.Processed[job.ID].IsZero() || len(gh.comments) != 1 {
		t.Fatalf("uncommitted result stayed retryable: state=%+v comments=%v", store.Data, gh.comments)
	}
	if present, err := store.HasAcceptedResult(); err != nil || present {
		t.Fatalf("uncommitted sidecar survived terminal cleanup: present=%t error=%v", present, err)
	}
}

func TestRunOnceRemovesCompletedResultSidecarAfterRestart(t *testing.T) {
	store, path, accepted, _, monitor := storedRecoveryFixture(t)
	store.Data.Processed[accepted.JobID] = time.Now().UTC()
	delete(store.Data.JobFailures, accepted.JobID)
	store.Data.Accepted = nil
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	handler := &recoveredResultHandler{}
	monitor.Handler = handler
	if _, err := monitor.RunOnce(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	if present, err := reopened.HasAcceptedResult(); err != nil || present ||
		handler.executions != 0 || handler.publications != 0 ||
		reopened.Data.Processed[accepted.JobID].IsZero() {
		t.Fatalf("completed attempt retained a result or dispatched work: present=%t err=%v handler=%+v state=%+v",
			present, err, handler, reopened.Data)
	}
}

func TestSelectedCompletedResultCleanupFailureKeepsCompletion(t *testing.T) {
	store, path, accepted, _, monitor := storedRecoveryFixture(t)
	if err := os.Remove(path + ".result"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".result", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".result", "held"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	err := monitor.completeSelected(t.Context(), store, jobFromAccepted(accepted), time.Now().UTC(), nil)
	if !errors.Is(err, ErrAcceptanceState) || store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
		t.Fatalf("cleanup failure reopened completed action: err=%v state=%+v", err, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, time.Time{})
	if reopened.Data.Accepted != nil || reopened.Data.Processed[accepted.JobID].IsZero() {
		t.Fatalf("cleanup failure was not durably completed: %+v", reopened.Data)
	}
}

func TestSelectedCompletionSaveFailureRetainsReceiptForRestart(t *testing.T) {
	store, path, accepted, gh, monitor := storedRecoveryFixture(t)
	job := jobFromAccepted(accepted)
	store.Data.PendingFixes[job.ID] = state.PendingFix{SHA: "reviewed-commit"}
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
	if err := monitor.completeSelected(t.Context(), store, job, time.Now().UTC(), nil); !errors.Is(err, ErrAcceptanceState) {
		t.Fatalf("failed completion save did not stop publication: %v", err)
	}
	if store.Data.Accepted == nil || !store.Data.Accepted.Terminal ||
		store.Data.TerminalDecisions[job.ID].Outcome != "succeeded" ||
		store.Data.JobFailures[job.ID].Attempts != 1 ||
		store.Data.PendingFixes[job.ID].SHA != "reviewed-commit" ||
		!store.Data.Processed[job.ID].IsZero() {
		t.Fatalf("failed completion save forgot the first decision or changed completion state: %+v", store.Data)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("failed completion save lost receipt: present=%t err=%v", present, err)
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
	reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	handler := &recoveredResultHandler{}
	monitor.Handler = handler
	if _, err := monitor.RunOnce(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	if handler.executions != 0 || handler.publications != 1 ||
		reopened.Data.Accepted != nil || reopened.Data.Processed[job.ID].IsZero() ||
		len(gh.comments) != 0 {
		t.Fatalf("restart did not finish saved result: handler=%+v state=%+v comments=%v", handler, reopened.Data, gh.comments)
	}
	if present, err := reopened.HasAcceptedResult(); err != nil || present {
		t.Fatalf("restart retained completed result: present=%t err=%v", present, err)
	}
}

func TestStoredResultRecoveryExpiresBeforePublication(t *testing.T) {
	at := time.Now().UTC().Add(-8 * time.Minute)
	store, path, accepted, gh, monitor := storedRecoveryFixtureAt(t, at)
	if !accepted.Deadline.Before(time.Now()) {
		t.Fatalf("accepted deadline did not expire: %s", accepted.Deadline)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	monitor.Now = time.Now
	handler := &recoveredResultHandler{}
	monitor.Handler = handler
	if _, err := monitor.RunOnce(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	decision := reopened.Data.TerminalDecisions[accepted.JobID]
	if handler.executions != 0 || handler.publications != 0 || decision.Outcome != "expired" ||
		decision.AttemptID != accepted.AttemptID || gh.currentChecks != 1 || gh.checks != 1 ||
		len(gh.comments) != 1 || reopened.Data.Accepted != nil ||
		reopened.Data.Processed[accepted.JobID].IsZero() || len(reopened.Data.JobFailures) != 0 {
		t.Fatalf("expired accepted result reached publication: handler=%+v github=%+v state=%+v", handler, gh, reopened.Data)
	}
	if present, err := reopened.HasAcceptedResult(); err != nil || present {
		t.Fatalf("expired result kept receipt: present=%t err=%v", present, err)
	}
}

func TestStoredResultRecoveryDefersInvitationUntilPublicationFinishes(t *testing.T) {
	for _, phase := range []string{"validation", "publication"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store, _, accepted, gh, monitor := storedRecoveryFixtureAt(t, now)
				monitor.Now = time.Now
				store.Data.NextInviteCheckAt = now.Add(time.Minute)
				store.Data.PublicationWrites["old-job"] = state.PublicationWrite{
					AttemptID: "old-attempt", Kind: "comment", BotLogin: "linkdataminion", BotID: 8,
					Repository: accepted.Repository, Number: accepted.SubjectNumber,
					Marker: "<!-- linkdataminion:old-job -->", Since: now,
					StartedAt: now, Deadline: now.Add(2 * time.Minute), Status: state.PublicationWritePending,
				}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				checked := make(chan struct{}, 1)
				gh.beforeList = func() { checked <- struct{}{} }
				entered, release := make(chan struct{}, 1), make(chan struct{})
				released := false
				defer func() {
					if !released {
						close(release)
					}
				}()
				handler := &recoveredResultHandler{}
				if phase == "validation" {
					gh.beforeRepository = func() {
						entered <- struct{}{}
						<-release
					}
				} else {
					handler.publish = func(Job) error {
						entered <- struct{}{}
						<-release
						return nil
					}
				}
				monitor.Handler = handler
				finished := make(chan error, 1)
				go func() { _, err := monitor.RunOnce(t.Context(), store); finished <- err }()
				select {
				case <-entered:
				case err := <-finished:
					t.Fatalf("%s did not start: %v", phase, err)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if len(gh.verifiedCalls) != 0 {
					t.Fatalf("older publication delayed held %s: checks=%d", phase, len(gh.verifiedCalls))
				}
				select {
				case <-checked:
					t.Fatalf("due invitation delayed held %s", phase)
				default:
				}
				select {
				case err := <-finished:
					t.Fatalf("held %s returned before release: %v", phase, err)
				default:
				}
				close(release)
				released = true
				if err := <-finished; err != nil || handler.executions != 0 || handler.publications != 1 ||
					store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
					t.Fatalf("held %s did not finish: err=%v handler=%+v state=%+v", phase, err, handler, store.Data)
				}
				if _, err := monitor.RunOnce(t.Context(), store); err != nil {
					t.Fatal(err)
				}
				select {
				case <-checked:
				default:
					t.Fatal("deferred invitation was not checked after publication")
				}
			})
		})
	}
}

func TestStoredResultRecoveryDefersTerminalReplyUntilPublicationFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store, _, accepted, gh, monitor := storedRecoveryFixtureAt(t, now)
		monitor.Now = time.Now
		sibling := selectedItem(now.Add(-time.Second))
		sibling.Key, sibling.NodeID = "comment:2", "node:2"
		sibling.HTMLURL = "https://github.com/example-owner/example-repo/pull/7#issuecomment-2"
		notice := testNotification(sibling.UpdatedAt)
		key := notificationKey(notice)
		store.Data.PendingNotifications[key] = state.PendingNotification{
			Repository: notice.Repository.FullName, RepositoryID: accepted.RepositoryID, OwnerID: accepted.OwnerID,
			SubjectType: notice.Subject.Type, SubjectURL: notice.Subject.URL, Since: now.Add(-time.Minute), UpdatedAt: notice.UpdatedAt,
		}
		store.Data.JobFailures[jobID(sibling.Key, "review")] = state.JobFailure{
			Attempts: 1, AcceptedAt: now.Add(-time.Minute), Deadline: now.Add(time.Minute), NotificationKey: key,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh.items = []github.Item{sibling}
		gh.currentItem = func(item github.Item) (github.Item, error) {
			if item.Key == sibling.Key {
				return sibling, nil
			}
			return selectedItem(now), nil
		}
		monitor.GitHub = &synchronizedRecoveryGitHub{fakeGitHub: gh}
		commented := make(chan struct{}, 1)
		gh.beforeComment = func() { commented <- struct{}{} }
		entered, release := make(chan struct{}, 1), make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		handler := &recoveredResultHandler{publish: func(Job) error {
			entered <- struct{}{}
			<-release
			return nil
		}}
		monitor.Handler = handler
		finished := make(chan error, 1)
		go func() { _, err := monitor.RunOnce(t.Context(), store); finished <- err }()
		select {
		case <-entered:
		case err := <-finished:
			t.Fatalf("publication did not start: %v", err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case <-commented:
			t.Fatal("due terminal reply delayed held publication")
		default:
		}
		select {
		case err := <-finished:
			t.Fatalf("held publication returned before release: %v", err)
		default:
		}
		close(release)
		released = true
		if err := <-finished; err != nil || handler.executions != 0 || handler.publications != 1 ||
			store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
			t.Fatalf("terminal reply or publication failed: err=%v handler=%+v state=%+v comments=%v", err, handler, store.Data, gh.comments)
		}
		if len(gh.comments) == 0 {
			if _, err := monitor.RunOnce(t.Context(), store); err != nil {
				t.Fatal(err)
			}
		}
		if len(gh.comments) != 1 {
			t.Fatalf("deferred terminal reply was not posted after publication: comments=%v", gh.comments)
		}
	})
}

func TestStoredResultRecoveryRetriesPublicationWithoutExecution(t *testing.T) {
	store, path, accepted, gh, monitor := storedRecoveryFixture(t)
	publishErr := errors.New("publication unavailable")
	publishAttempts := 0
	handler := &recoveredResultHandler{publish: func(Job) error {
		publishAttempts++
		if publishAttempts == 1 {
			return publishErr
		}
		return nil
	}}
	monitor.Handler = handler
	next, err := monitor.RunOnce(t.Context(), store)
	if !errors.Is(err, publishErr) || !errors.Is(err, ErrPostModelPublication) ||
		store.Data.Accepted == nil || store.Data.Accepted.AttemptID != accepted.AttemptID ||
		!next.Equal(store.Data.Accepted.RetryAt) || !next.After(time.Now()) ||
		store.Data.JobFailures[accepted.JobID].Attempts != 1 || handler.executions != 0 || handler.publications != 1 ||
		len(gh.comments) != 0 {
		t.Fatalf("publication failure did not retain a scheduled result: next=%s err=%v state=%+v handler=%+v", next, err, store.Data, handler)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("failed publication lost sidecar: present=%t err=%v", present, err)
	}
	if _, err := monitor.RunOnce(t.Context(), store); err != nil || handler.publications != 1 {
		t.Fatalf("publication retried before due time: err=%v publications=%d", err, handler.publications)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	if reopened.Data.Accepted == nil || !reopened.Data.Accepted.RetryAt.Equal(next) {
		t.Fatalf("publication retry was not durable: %+v", reopened.Data)
	}
	if present, err := reopened.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("restart lost saved result: present=%t err=%v", present, err)
	}
	reopened.Data.Accepted.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := reopened.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := monitor.RunOnce(t.Context(), reopened); err != nil || handler.executions != 0 || handler.publications != 2 ||
		reopened.Data.Accepted != nil || reopened.Data.Processed[accepted.JobID].IsZero() || len(gh.comments) != 0 {
		t.Fatalf("due publication did not finish saved result: err=%v handler=%+v state=%+v", err, handler, reopened.Data)
	}
	if present, err := reopened.HasAcceptedResult(); err != nil || present {
		t.Fatalf("successful retry kept sidecar: present=%t err=%v", present, err)
	}
}

func TestStoredResultRecoveryEndsPermanentPublicationFailure(t *testing.T) {
	store, path, accepted, gh, monitor := storedRecoveryFixture(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
	handler := &recoveredResultHandler{publish: func(Job) error {
		return errors.Join(ErrTerminalJob, ErrPostModelPublication, errors.New("reviewed fix has no changes"))
	}}
	monitor.Handler = handler
	if _, err := monitor.RunOnce(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	if handler.executions != 0 || handler.publications != 1 || reopened.Data.Accepted != nil ||
		reopened.Data.Processed[accepted.JobID].IsZero() || len(reopened.Data.JobFailures) != 0 || len(gh.comments) != 1 {
		t.Fatalf("permanent saved result kept retrying: handler=%+v state=%+v comments=%v", handler, reopened.Data, gh.comments)
	}
	if present, err := reopened.HasAcceptedResult(); err != nil || present {
		t.Fatalf("permanent saved result kept receipt: present=%t err=%v", present, err)
	}
}

func TestStoredResultRecoveryDoesNotRetryAfterAcceptedDeadline(t *testing.T) {
	at := time.Now().UTC().Add(-8 * time.Minute)
	store, _, accepted, _, monitor := storedRecoveryFixtureAt(t, at)
	monitor.Now = time.Now
	handler := &recoveredResultHandler{publish: func(Job) error {
		t.Fatal("expired result reached publisher")
		return nil
	}}
	monitor.Handler = handler
	if _, err := monitor.RunOnce(t.Context(), store); err != nil ||
		store.Data.Accepted != nil || store.Data.TerminalDecisions[accepted.JobID].Outcome != "expired" ||
		handler.executions != 0 || handler.publications != 0 {
		t.Fatalf("expired publication was retried: err=%v state=%+v handler=%+v", err, store.Data, handler)
	}
	if present, err := store.HasAcceptedResult(); err != nil || present {
		t.Fatalf("expired result retained receipt: present=%t err=%v", present, err)
	}
}

func TestCompleteSelectedCancelsAssignedSavedResult(t *testing.T) {
	for _, name := range []string{"publisher canceled", "parent canceled"} {
		t.Run(name, func(t *testing.T) {
			store, _, accepted, _, monitor := storedRecoveryFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.Join(ErrTerminalJob, errors.New("publication interrupted"))
			if name == "publisher canceled" {
				failure = errors.Join(failure, context.Canceled)
			} else {
				cancel()
			}
			if err := monitor.completeSelected(ctx, store, jobFromAccepted(accepted), time.Now().UTC(), failure); err != nil {
				t.Fatal(err)
			}
			if store.Data.TerminalDecisions[accepted.JobID].Outcome != "canceled" ||
				store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() {
				t.Fatalf("cancellation did not end assigned result: %+v", store.Data)
			}
			if present, err := store.HasAcceptedResult(); err != nil || present {
				t.Fatalf("canceled result retained receipt: present=%t err=%v", present, err)
			}
		})
	}
}

func TestStoredResultRecoveryDiscardsInvalidSourceOrRequester(t *testing.T) {
	for _, test := range []struct {
		name             string
		change           func(*fakeGitHub, time.Time)
		permissionChecks int
	}{
		{"edited source", func(gh *fakeGitHub, at time.Time) {
			gh.currentItem = func(github.Item) (github.Item, error) {
				item := selectedItem(at)
				item.Edited = true
				return item, nil
			}
		}, 0},
		{"revoked requester", func(gh *fakeGitHub, _ time.Time) {
			gh.permission.Permission = "read"
		}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, path, accepted, gh, monitor := storedRecoveryFixture(t)
			test.change(gh, accepted.AcceptedAt)
			usageCalls := 0
			handler := &terminalUsageRecoveryHandler{onTerminal: func(job Job, outcome string, at time.Time) {
				usageCalls++
				if job.ID != accepted.JobID || outcome != "lost" || at.IsZero() || store.Data.Accepted == nil {
					t.Errorf("discard skipped terminal usage before cleanup: job=%+v outcome=%q state=%+v", job, outcome, store.Data)
				}
			}}
			monitor.Handler = handler
			if _, err := monitor.RunOnce(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			if handler.executions != 0 || handler.publications != 0 || usageCalls == 0 || gh.currentChecks == 0 ||
				gh.checks < test.permissionChecks || len(gh.comments) != 0 ||
				store.Data.TerminalDecisions[accepted.JobID].Outcome != "lost" {
				t.Fatalf("invalid command reached publisher: usage_calls=%d handler=%+v github=%+v", usageCalls, handler, gh)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openStateAt(t, path, accepted.AcceptedAt.Add(-time.Minute))
			present, err := reopened.HasAcceptedResult()
			if err != nil || present || reopened.Data.Accepted != nil ||
				reopened.Data.Processed[accepted.JobID].IsZero() || len(reopened.Data.JobFailures) != 0 {
				t.Fatalf("invalid command was not durably discarded: present=%t err=%v state=%+v", present, err, reopened.Data)
			}
		})
	}
}

func TestStoredResultRecoveryStopsOnUnusableSidecarOrPublisher(t *testing.T) {
	for _, test := range []struct {
		name          string
		change        func(*testing.T, string, *Monitor)
		currentChecks int
	}{
		{"nonregular sidecar", func(t *testing.T, path string, monitor *Monitor) {
			if err := os.Remove(path + ".result"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path+".result", 0700); err != nil {
				t.Fatal(err)
			}
			monitor.Handler = &recoveredResultHandler{}
		}, 0},
		{"missing publisher", func(_ *testing.T, _ string, monitor *Monitor) {
			monitor.Handler = selectedHandler(func(Job) error { return errors.New("stored result was executed again") })
		}, 0},
		{"publisher rejects corrupt receipt", func(_ *testing.T, _ string, monitor *Monitor) {
			monitor.Handler = &recoveredResultHandler{publish: func(Job) error {
				return errors.Join(ErrAcceptanceState, errors.New("corrupt saved result"))
			}}
		}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, path, accepted, gh, monitor := storedRecoveryFixture(t)
			test.change(t, path, monitor)
			if _, err := monitor.RunOnce(t.Context(), store); !errors.Is(err, ErrAcceptanceState) {
				t.Fatalf("unsafe result state did not stop recovery: %v", err)
			}
			if store.Data.Accepted == nil || store.Data.Accepted.AttemptID != accepted.AttemptID ||
				!store.Data.Accepted.RetryAt.IsZero() || len(store.Data.Processed) != 0 ||
				gh.currentChecks != test.currentChecks || len(gh.comments) != 0 {
				t.Fatalf("unsafe result state changed reservation or published: %+v", store.Data)
			}
			if _, err := os.Lstat(path + ".result"); err != nil {
				t.Fatalf("unsafe result sidecar was removed: %v", err)
			}
		})
	}
}
