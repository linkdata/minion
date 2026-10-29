package monitor

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type cancelDiscoveryGitHub struct {
	*fakeGitHub
	stage           string
	cancel          context.CancelFunc
	err             error
	calls           []string
	repositoryCalls int
}

type intervalErrorGitHub struct {
	*fakeGitHub
	beforeReturn func()
	err          error
}

func (f *intervalErrorGitHub) ListRepositoryNotifications(context.Context, string, time.Time) ([]github.Notification, time.Duration, error) {
	f.beforeReturn()
	return nil, 5 * time.Minute, f.err
}

func TestRunOnceLabelsListErrorWhenIntervalSaveFails(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, statePath, now.Add(-time.Minute))
	store.Data.NextInviteCheckAt = now.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	listErr := errors.New("list failed")
	gh := &intervalErrorGitHub{fakeGitHub: &fakeGitHub{}, err: listErr, beforeReturn: func() {
		if err := os.Remove(statePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(statePath, 0700); err != nil {
			t.Fatal(err)
		}
	}}
	m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: &fakeHandler{}, Now: func() time.Time { return now }, SelectedExecutorID: "local"}
	_, err := m.RunOnce(t.Context(), store)
	if !errors.Is(err, listErr) || !strings.Contains(err.Error(), "list example-owner/example-repo notifications: list failed") || !strings.Contains(err.Error(), statePath) {
		t.Fatalf("list and save errors lost context: %v", err)
	}
}

func (f *cancelDiscoveryGitHub) call(stage string) error {
	f.calls = append(f.calls, stage)
	if stage == "repository" {
		f.repositoryCalls++
	}
	if stage == f.stage || f.stage == "reply" && stage == "repository" && f.repositoryCalls == 2 {
		f.cancel()
		return f.err
	}
	return nil
}

func (f *cancelDiscoveryGitHub) ListRepositoryNotifications(ctx context.Context, repo string, since time.Time) ([]github.Notification, time.Duration, error) {
	if err := f.call("notifications"); err != nil {
		return nil, 5 * time.Minute, err
	}
	notifications, interval, err := f.fakeGitHub.ListRepositoryNotifications(ctx, repo, since)
	if f.stage == "notifications" {
		interval = 5 * time.Minute
	}
	return notifications, interval, err
}

func (f *cancelDiscoveryGitHub) Repository(ctx context.Context, repo string) (github.Repository, error) {
	if err := f.call("repository"); err != nil {
		return github.Repository{}, err
	}
	if f.stage == "repository" && f.err == nil {
		// Simulate a completed API call returning success after local cancellation.
		ctx = context.WithoutCancel(ctx)
	}
	return f.fakeGitHub.Repository(ctx, repo)
}

func (f *cancelDiscoveryGitHub) ThreadItems(ctx context.Context, repo, subject string, number int, since time.Time) ([]github.Item, error) {
	if err := f.call("thread"); err != nil {
		return nil, err
	}
	return f.fakeGitHub.ThreadItems(ctx, repo, subject, number, since)
}

func (f *cancelDiscoveryGitHub) CurrentItem(ctx context.Context, item github.Item) (github.Item, error) {
	if err := f.call("current"); err != nil {
		return github.Item{}, err
	}
	return f.fakeGitHub.CurrentItem(ctx, item)
}

func (f *cancelDiscoveryGitHub) Permission(ctx context.Context, repo, login string) (github.Permission, error) {
	if err := f.call("permission"); err != nil {
		return github.Permission{}, err
	}
	return f.fakeGitHub.Permission(ctx, repo, login)
}

func (f *cancelDiscoveryGitHub) ReactEyes(ctx context.Context, nodeID string) error {
	if err := f.call("reaction"); err != nil {
		return err
	}
	return f.fakeGitHub.ReactEyes(ctx, nodeID)
}

func seedShutdownPending(store *state.Store, first, tail github.Notification, since time.Time) (string, state.PendingNotification) {
	pending := state.PendingNotification{Repository: tail.Repository.FullName, RepositoryID: 1, OwnerID: 2, SubjectType: tail.Subject.Type, SubjectURL: tail.Subject.URL,
		Since: since, UpdatedAt: tail.UpdatedAt, RetryAt: first.UpdatedAt.Add(-time.Second), Failures: 5}
	key := notificationKey(tail)
	store.Data.PendingNotifications[key] = pending
	store.Data.RepositoryCursors[first.Repository.FullName] = since
	return key, pending
}

func TestRunOnceStopsCanceledFailureReplyWithoutDeferring(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		replies int
	}{
		{name: "reply succeeds", replies: 1},
		{name: "repository fails", err: errors.New("repository unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
			initial := now.Add(-time.Minute)
			item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}
			first, tail := testNotification(now), testNotification(now.Add(time.Second))
			tail.ID = "thread-2"
			tail.Subject.URL = strings.Replace(tail.Subject.URL, "/7", "/8", 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			gh := &cancelDiscoveryGitHub{stage: "reply", fakeGitHub: &fakeGitHub{notifications: []github.Notification{first, tail}, items: []github.Item{item}, permission: github.Permission{Permission: "write"}}, cancel: cancel, err: test.err}
			store := openState(t, initial)
			handler := &fakeHandler{}
			m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return now }, SelectedExecutorID: "local"}
			cmd, err := command.ParseForLoginCommands(item.Body, m.Config.BotLogin, m.availableCommands(m.Config.Repositories[0]))
			if err != nil {
				t.Fatal(err)
			}
			id := jobID(item.Key, cmd.Name)
			store.Data.JobFailures[id] = state.JobFailure{Attempts: maxJobAttempts}
			key, pending := seedShutdownPending(store, first, tail, initial)
			if _, err := m.RunOnce(ctx, store); !errors.Is(err, context.Canceled) || test.err != nil && !errors.Is(err, test.err) {
				t.Fatalf("canceled failure reply lost cancellation: %v", err)
			}
			if gh.repositoryCalls != 2+test.replies || len(handler.jobs) != 0 || len(store.Data.PendingNotifications) != 1 || store.Data.PendingNotifications[key] != pending ||
				!store.Data.Cursor.Equal(initial) || !store.Data.RepositoryCursors[first.Repository.FullName].Equal(initial) {
				t.Fatalf("canceled failure reply continued discovery or changed backoff/cursors: calls=%d state=%+v", gh.repositoryCalls, store.Data)
			}
			_, processed := store.Data.Processed[id]
			if processed != (test.replies == 1) || len(gh.comments) != test.replies || store.Data.JobFailures[id].ReplyAttempts != 0 {
				t.Fatalf("failure reply completion or budget changed: processed=%t comments=%d failure=%+v", processed, len(gh.comments), store.Data.JobFailures[id])
			}
		})
	}
}

func TestRunOnceStopsCanceledDiscoveryWithoutDeferring(t *testing.T) {
	stages := []string{"notifications", "repository", "thread", "current", "permission", "reaction"}
	for _, stage := range append(slices.Clone(stages), "before start", "invitations") {
		for _, returnError := range []bool{false, true} {
			if returnError && (stage == "before start" || stage == "invitations") {
				continue
			}
			name := stage + "/nil"
			if returnError {
				name = stage + "/error"
			}
			t.Run(name, func(t *testing.T) {
				now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
				initial := now.Add(-time.Minute)
				item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}
				first := testNotification(now)
				tail := testNotification(now.Add(time.Second))
				tail.ID = "thread-2"
				tail.Subject.URL = strings.Replace(tail.Subject.URL, "/7", "/8", 1)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				gh := &cancelDiscoveryGitHub{
					fakeGitHub: &fakeGitHub{notifications: []github.Notification{first, tail}, items: []github.Item{item, item}, permission: github.Permission{Permission: "write"}},
					stage:      stage,
					cancel:     cancel,
				}
				if returnError {
					gh.err = errors.New("discovery unavailable")
				}
				path := filepath.Join(t.TempDir(), "state.json")
				store := openStateAt(t, path, initial)
				key, pending := seedShutdownPending(store, first, tail, initial)
				store.Data.NextInviteCheckAt = now.Add(time.Minute)
				switch stage {
				case "before start":
					cancel()
				case "invitations":
					store.Data.NextInviteCheckAt = time.Time{}
					gh.beforeList = cancel
				}
				inviteCheckAt := store.Data.NextInviteCheckAt
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				handler := &fakeHandler{}
				m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return now }, SelectedExecutorID: "local"}
				_, runErr := m.RunOnce(ctx, store)
				if !errors.Is(runErr, context.Canceled) || gh.err != nil && !errors.Is(runErr, gh.err) {
					t.Fatalf("canceled discovery: %v", runErr)
				}
				locations := map[string]string{
					"notifications": "list example-owner/example-repo notifications",
					"repository":    "verify example-owner/example-repo repository identity",
					"thread":        "example-owner/example-repo PullRequest #7",
					"current":       "verify command in comment:1",
					"permission":    "check @writer permission in example-owner/example-repo",
					"reaction":      "react to comment:1",
				}
				if location := locations[stage]; location != "" && !strings.Contains(runErr.Error(), location) {
					t.Fatalf("canceled discovery lost %s context: %v", stage, runErr)
				}
				var want []string
				if index := slices.Index(stages, gh.stage); index >= 0 {
					want = stages[:index+1]
				}
				if !slices.Equal(gh.calls, want) {
					t.Fatalf("discovery continued after cancellation: got %v, want %v", gh.calls, want)
				}
				if len(handler.jobs) != 0 || len(gh.comments) != 0 {
					t.Fatalf("canceled discovery executed or replied: jobs=%d comments=%d", len(handler.jobs), len(gh.comments))
				}
				checkState := func(data state.Data) {
					t.Helper()
					if stage == "notifications" && !data.NextPollAt.Equal(now.Add(5*time.Minute)) {
						t.Fatalf("canceled poll lost returned interval: %s", data.NextPollAt)
					}
					if !data.NextInviteCheckAt.Equal(inviteCheckAt) {
						t.Fatalf("shutdown advanced invitation check: %s", data.NextInviteCheckAt)
					}
					if stage == "reaction" {
						firstPending := data.PendingNotifications[notificationKey(first)]
						if len(data.PendingNotifications) != 2 || data.PendingNotifications[key] != pending ||
							firstPending.Failures != 0 || !firstPending.RetryAt.IsZero() || data.Accepted == nil ||
							data.JobFailures[data.Accepted.JobID].Attempts != 1 {
							t.Fatalf("shutdown lost selected reservation or replay: %+v", data)
						}
					} else if len(data.PendingNotifications) != 1 || data.PendingNotifications[key] != pending {
						t.Fatalf("shutdown changed retry state: %+v", data.PendingNotifications)
					}
					if !data.Cursor.Equal(initial) || !data.RepositoryCursors[first.Repository.FullName].Equal(initial) {
						t.Fatalf("shutdown advanced cursors: %+v", data)
					}
					if stage != "reaction" && len(data.JobFailures) != 0 || len(data.Processed) != 0 || len(data.PendingFixes) != 0 {
						t.Fatalf("shutdown consumed work: %+v", data)
					}
				}
				checkState(store.Data)
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				reopened := openStateAt(t, path, initial)
				checkState(reopened.Data)
			})
		}
	}
}

func TestRunOnceDefersDiscoveryCancellationWithoutParentShutdown(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, threadItems: func(string, int, time.Time) ([]github.Item, error) {
		return nil, context.Canceled
	}}
	store := openState(t, now.Add(-time.Minute))
	m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: &fakeHandler{}, Now: func() time.Time { return now }, SelectedExecutorID: "local"}
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, context.Canceled) {
		t.Fatalf("internal discovery cancellation: %v", err)
	}
	if pending := store.Data.PendingNotifications[notificationKey(gh.notifications[0])]; len(store.Data.PendingNotifications) != 1 || pending.Failures != 1 || !pending.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("internal cancellation did not retain ordinary retry: %+v", store.Data.PendingNotifications)
	}
	if !store.Data.Cursor.After(now.Add(-time.Minute)) {
		t.Fatalf("internal cancellation pinned the cursor: %+v", store.Data)
	}
}

type cancelOnRetryLog struct {
	cancel context.CancelFunc
	called bool
}

func (w *cancelOnRetryLog) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "retry ") {
		w.called = true
		w.cancel()
	}
	return len(p), nil
}

func TestRunOnceCancellationAfterRetrySavePreservesCursors(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	initial := now.Add(-time.Minute)
	threadErr := errors.New("thread unavailable")
	notification := testNotification(now)
	untrusted := testNotification(now.Add(time.Second))
	untrusted.ID = "thread-2"
	untrusted.Repository.ID = 99
	untrusted.Subject.URL = strings.Replace(untrusted.Subject.URL, "/7", "/8", 1)
	later := testNotification(now.Add(2 * time.Second))
	later.ID = "thread-3"
	later.Subject.URL = strings.Replace(later.Subject.URL, "/7", "/9", 1)
	gh := &fakeGitHub{notifications: []github.Notification{notification, untrusted, later}, threadItems: func(string, int, time.Time) ([]github.Item, error) {
		return nil, threadErr
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, initial)
	store.Data.NextInviteCheckAt = now.Add(time.Minute)
	untrustedKey, untrustedPending := seedShutdownPending(store, notification, untrusted, initial)
	untrustedPending.RepositoryID = untrusted.Repository.ID
	store.Data.PendingNotifications[untrustedKey] = untrustedPending
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	logs := &cancelOnRetryLog{cancel: cancel}
	handler := &fakeHandler{}
	m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Logger: log.New(logs, "", 0), Now: func() time.Time { return now }, SelectedExecutorID: "local"}
	if _, err := m.RunOnce(ctx, store); !errors.Is(err, context.Canceled) || !errors.Is(err, threadErr) {
		t.Fatalf("cancellation after retry save lost a cause: %v", err)
	}
	if !logs.called || gh.repoChecks != 1 || gh.threadCalls != 1 || len(handler.jobs) != 0 || len(gh.comments) != 0 {
		t.Fatalf("retry-save cancellation continued another scan: logged=%t repository checks=%d thread calls=%d jobs=%d replies=%d", logs.called, gh.repoChecks, gh.threadCalls, len(handler.jobs), len(gh.comments))
	}
	key := notificationKey(notification)
	pending := store.Data.PendingNotifications[key]
	if pending.Failures != 1 || !pending.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("retry was not saved before cancellation: %+v", pending)
	}
	checkState := func(data state.Data) {
		t.Helper()
		if !data.Cursor.Equal(initial) || !data.RepositoryCursors[notification.Repository.FullName].Equal(initial) {
			t.Fatalf("cancellation after retry save advanced cursors: %+v", data)
		}
		if len(data.PendingNotifications) != 2 || data.PendingNotifications[key] != pending || data.PendingNotifications[untrustedKey] != untrustedPending {
			t.Fatalf("cancellation changed the durable retries: %+v", data.PendingNotifications)
		}
	}
	checkState(store.Data)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	checkState(openStateAt(t, path, initial).Data)
}
