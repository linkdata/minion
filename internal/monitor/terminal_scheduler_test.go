package monitor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func queueTerminalControl(store *state.Store, updatedAt time.Time, number int, failure state.JobFailure, retryAt time.Time) (github.Item, string) {
	item := selectedItem(updatedAt)
	item.Key = fmt.Sprintf("comment:%d", number)
	item.NodeID = fmt.Sprintf("node:%d", number)
	item.HTMLURL = fmt.Sprintf("https://github.com/example-owner/example-repo/pull/%d#issuecomment-%d", number, number)
	notice := testNotification(updatedAt)
	notice.Subject.URL = fmt.Sprintf("https://api.github.com/repos/example-owner/example-repo/pulls/%d", number)
	key := notificationKey(notice)
	store.Data.PendingNotifications[key] = state.PendingNotification{
		Repository: notice.Repository.FullName, RepositoryID: 1, OwnerID: 2,
		SubjectType: notice.Subject.Type, SubjectURL: notice.Subject.URL,
		Since: updatedAt.Add(-time.Minute), UpdatedAt: updatedAt, RetryAt: retryAt,
	}
	failure.NotificationKey = key
	id := jobID(item.Key, "review")
	store.Data.JobFailures[id] = failure
	return item, id
}

func terminalThreads(items map[int]github.Item) func(string, int, time.Time) ([]github.Item, error) {
	return func(_ string, number int, _ time.Time) ([]github.Item, error) {
		item, ok := items[number]
		if !ok {
			return nil, fmt.Errorf("unexpected thread #%d", number)
		}
		return []github.Item{item}, nil
	}
}

func terminalReplyRecorded(gh *fakeGitHub, store *state.Store, id string) bool {
	return len(gh.comments) == 1 && strings.HasSuffix(gh.comments[0].Body, "\n\n<!-- linkdataminion:"+id+" -->") &&
		!store.Data.Processed[id].IsZero()
}

func TestRunOnceServicesTerminalControlBeforeAcceptedRetryOrFuturePoll(t *testing.T) {
	for _, test := range []struct {
		name     string
		accepted bool
	}{
		{name: "accepted retry", accepted: true},
		{name: "future poll"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store := openState(t, now.Add(-time.Minute))
				due, dueID := queueTerminalControl(store, now.Add(-2*time.Second), 8,
					state.JobFailure{Attempts: maxJobAttempts}, time.Time{})
				deadline := now.Add(2 * time.Minute)
				future, futureID := queueTerminalControl(store, now.Add(-time.Second), 9,
					state.JobFailure{Attempts: 1, AcceptedAt: now.Add(-time.Minute), Deadline: deadline}, time.Time{})
				store.Data.NextPollAt = now.Add(20 * time.Minute)
				store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
				if test.accepted {
					accepted := selectedAcceptance(t, now)
					accepted.RetryAt = now.Add(5 * time.Minute)
					store.Data.Accepted = &accepted
					store.Data.JobFailures[accepted.JobID] = state.JobFailure{
						Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
					}
					store.Data.NextInviteCheckAt = now.Add(-time.Second)
				}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				gh := &fakeGitHub{
					permission:  github.Permission{Permission: "write"},
					threadItems: terminalThreads(map[int]github.Item{8: due, 9: future}),
					invitations: []github.Invitation{{ID: 41, Repository: github.Repository{
						FullName: "example-owner/example-repo", ID: 1,
						Owner: github.User{Login: "example-owner", ID: 2},
					}, Permissions: "write"}},
				}
				handler := &capacityHandler{}
				m := selectedMonitor(t, now, gh, handler)
				next, err := m.RunOnce(t.Context(), store)
				if err != nil {
					t.Fatalf("RunOnce: %v", err)
				}
				if !terminalReplyRecorded(gh, store, dueID) || !store.Data.Processed[futureID].IsZero() ||
					!next.Equal(deadline) || len(gh.notificationCalls) != 0 || len(gh.reactions) != 0 || len(handler.jobs) != 0 {
					t.Fatalf("terminal control or wake missed: next=%s want=%s comments=%v processed=%v calls=%v jobs=%v",
						next, deadline, gh.comments, store.Data.Processed, gh.notificationCalls, handler.jobs)
				}
				if test.accepted {
					if !slices.Equal(gh.accepted, []int64{41}) || store.Data.Accepted == nil ||
						!store.Data.Accepted.RetryAt.Equal(now.Add(5*time.Minute)) || handler.waits != 0 {
						t.Fatalf("accepted retry or invitation changed: invitations=%v accepted=%+v waits=%d",
							gh.accepted, store.Data.Accepted, handler.waits)
					}
				} else if handler.waits != 1 || len(gh.accepted) != 0 {
					t.Fatalf("future poll readiness or invitations changed: waits=%d invitations=%v", handler.waits, gh.accepted)
				}
			})
		})
	}
}

func TestRunOnceServicesTerminalDeadlineDuringHeldCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		deadline := now.Add(time.Minute)
		store := openState(t, now.Add(-time.Minute))
		terminal, id := queueTerminalControl(store, now.Add(-time.Second), 8,
			state.JobFailure{Attempts: 1, AcceptedAt: now.Add(-time.Minute), Deadline: deadline}, time.Time{})
		store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &fakeGitHub{permission: github.Permission{Permission: "write"}, items: []github.Item{terminal}}
		handler := &heldCapacityHandler{entered: make(chan struct{}), release: make(chan struct{})}
		m := selectedMonitor(t, now, gh, handler)
		m.Now = nil
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()
		<-handler.entered
		time.Sleep(time.Minute)
		synctest.Wait()
		if !terminalReplyRecorded(gh, store, id) || handler.waits.Load() != 1 || handler.cancels.Load() != 0 ||
			len(gh.notificationCalls) != 0 || len(gh.reactions) != 0 || len(handler.jobs) != 0 {
			t.Errorf("terminal deadline waited for capacity: comments=%v processed=%v waits=%d cancels=%d calls=%v jobs=%v",
				gh.comments, store.Data.Processed[id], handler.waits.Load(), handler.cancels.Load(), gh.notificationCalls, handler.jobs)
		}
		select {
		case err := <-result:
			t.Errorf("RunOnce returned before capacity release: %v", err)
		default:
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", err)
		}
	})
}

func TestSelectedExecuteRetriesTerminalIdentityMismatch(t *testing.T) {
	for _, identity := range []string{"current author", "permission user"} {
		t.Run(identity, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				selected := selectedItem(now.Add(-2 * time.Second))
				store := openState(t, now.Add(-time.Minute))
				terminal, id := queueTerminalControl(store, now.Add(-time.Second), 8,
					state.JobFailure{Attempts: 1, AcceptedAt: now.Add(-time.Minute), Deadline: now.Add(30 * time.Second)}, time.Time{})
				store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				base := &fakeGitHub{
					notifications: []github.Notification{testNotification(selected.UpdatedAt)},
					permission:    github.Permission{Permission: "write"},
					threadItems:   terminalThreads(map[int]github.Item{7: selected, 8: terminal}),
				}
				gh := &synchronizedCommentsGitHub{fakeGitHub: base}
				var terminalChecks atomic.Int32
				base.currentItem = func(item github.Item) (github.Item, error) {
					if item.Key == terminal.Key {
						check := terminalChecks.Add(1)
						if identity == "current author" && check == 1 {
							item.Author.ID++
						}
						if identity == "permission user" {
							base.permission.User.ID = 7
							if check == 1 {
								base.permission.User.ID++
							}
						}
					}
					return item, nil
				}
				held := &heldSelectedAction{entered: make(chan Job, 1), release: make(chan struct{})}
				m := selectedMonitor(t, now, gh, held)
				m.Now = nil
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				go func() { _, err := m.RunOnce(ctx, store); result <- err }()
				select {
				case <-held.entered:
				case err := <-result:
					t.Fatalf("selected Execute did not start: %v", err)
				}
				synctest.Wait()
				time.Sleep(31 * time.Second)
				synctest.Wait()
				_, comments := gh.commentsSnapshot()
				if terminalChecks.Load() != 1 || len(comments) != 0 || held.executions.Load() != 1 {
					t.Errorf("first terminal identity check did not happen during Execute: checks=%d comments=%v executions=%d",
						terminalChecks.Load(), comments, held.executions.Load())
				}
				time.Sleep(time.Minute + time.Second)
				synctest.Wait()
				_, comments = gh.commentsSnapshot()
				var processed time.Time
				var accepted bool
				if err := store.WithLock(func() error {
					processed = store.Data.Processed[id]
					accepted = store.Data.Accepted != nil
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if terminalChecks.Load() < 2 || len(comments) != 1 ||
					!strings.HasSuffix(comments[0].Body, "\n\n<!-- linkdataminion:"+id+" -->") || processed.IsZero() ||
					held.executions.Load() != 1 || !accepted || len(gh.reactions) != 1 {
					t.Errorf("terminal identity was not retried during Execute: checks=%d comments=%v processed=%v executions=%d accepted=%+v",
						terminalChecks.Load(), comments, processed, held.executions.Load(), accepted)
				}
				select {
				case err := <-result:
					t.Errorf("RunOnce returned while Execute was held: %v", err)
				default:
				}
				cancel()
				synctest.Wait()
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("RunOnce after cancellation: %v", err)
				}
			})
		})
	}
}

func TestRunOnceCompletedScanWakesForTerminalDeadlineOrReplyRetry(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure state.JobFailure
		retry   time.Duration
	}{
		{name: "deadline", failure: state.JobFailure{Attempts: 1}, retry: 5 * time.Minute},
		{name: "reply retry", failure: state.JobFailure{Attempts: maxJobAttempts, ReplyAttempts: 1}, retry: 2 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				store := openState(t, now.Add(-time.Minute))
				failure := test.failure
				if test.name == "deadline" {
					failure.AcceptedAt = now.Add(-time.Minute)
					failure.Deadline = now.Add(2 * time.Minute)
				}
				_, id := queueTerminalControl(store, now.Add(-time.Second), 8, failure, now.Add(test.retry))
				store.Data.NextInviteCheckAt = now.Add(15 * time.Minute)
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				cfg := testConfig(t)
				cfg.PollIntervalSeconds = 30 * 60
				gh := &fakeGitHub{}
				handler := &capacityHandler{}
				m := &Monitor{Config: cfg, GitHub: gh, Handler: handler, Now: func() time.Time { return now },
					SelectedExecutorID: "local"}
				next, err := m.RunOnce(t.Context(), store)
				want := now.Add(2 * time.Minute)
				if err != nil || !next.Equal(want) || !store.Data.NextPollAt.Equal(now.Add(30*time.Minute)) ||
					!store.Data.Processed[id].IsZero() || len(gh.comments) != 0 || len(gh.notificationCalls) != 1 || handler.waits != 1 {
					t.Fatalf("completed scan missed terminal wake: next=%s want=%s err=%v state=%+v calls=%v waits=%d",
						next, want, err, store.Data, gh.notificationCalls, handler.waits)
				}
			})
		})
	}
}
