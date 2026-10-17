package monitor

import (
	"context"
	"errors"
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
	stage  string
	cancel context.CancelFunc
	err    error
	calls  []string
}

type cancelFailureReplyGitHub struct {
	*fakeGitHub
	cancel          context.CancelFunc
	err             error
	repositoryCalls int
}

func (f *cancelFailureReplyGitHub) Repository(ctx context.Context, repo string) (github.Repository, error) {
	f.repositoryCalls++
	if f.repositoryCalls == 2 {
		f.cancel()
		if f.err != nil {
			return github.Repository{}, f.err
		}
	}
	return f.fakeGitHub.Repository(ctx, repo)
}

func TestRunOnceStopsCanceledFailureReplyWithoutDeferring(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		replies int
	}{
		{name: "reply succeeds", replies: 1},
		{name: "repository fails", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
			initial := now.Add(-time.Minute)
			item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}
			first, tail := testNotification(now), testNotification(now.Add(time.Second))
			tail.ID = "thread-2"
			tail.Subject.URL = strings.Replace(tail.Subject.URL, "/7", "/8", 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gh := &cancelFailureReplyGitHub{fakeGitHub: &fakeGitHub{notifications: []github.Notification{first, tail}, items: []github.Item{item}, permission: github.Permission{Permission: "write"}}, cancel: cancel, err: test.err}
			store := openState(t, initial)
			executor := &fakeExecutor{}
			m := &Monitor{Config: testConfig(t), GitHub: gh, Executor: executor, Now: func() time.Time { return now }}
			cmd, err := command.ParseForLoginCommands(item.Body, m.Config.BotLogin, m.availableCommands(m.Config.Repositories[0]))
			if err != nil {
				t.Fatal(err)
			}
			id := jobID(item.Key, cmd.Key)
			store.Data.JobFailures[id] = state.JobFailure{Attempts: maxJobAttempts}
			pending := state.PendingNotification{Repository: tail.Repository.FullName, RepositoryID: 1, OwnerID: 2, SubjectType: tail.Subject.Type, SubjectURL: tail.Subject.URL,
				Since: initial, UpdatedAt: tail.UpdatedAt, RetryAt: now.Add(-time.Second), Failures: 5}
			key := notificationKey(tail)
			store.Data.PendingNotifications[key] = pending
			store.Data.RepositoryCursors[first.Repository.FullName] = initial
			if _, err := m.RunOnce(ctx, store); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled failure reply lost cancellation: %v", err)
			}
			if gh.repositoryCalls != 2 || len(executor.jobs) != 0 || len(store.Data.PendingNotifications) != 1 || store.Data.PendingNotifications[key] != pending ||
				!store.Data.Cursor.Equal(initial) || !store.Data.RepositoryCursors[first.Repository.FullName].Equal(initial) {
				t.Fatalf("canceled failure reply continued discovery or changed backoff/cursors: calls=%d state=%+v", gh.repositoryCalls, store.Data)
			}
			_, processed := store.Data.Processed[id]
			if processed != (test.replies == 1) || len(gh.comments) != test.replies {
				t.Fatalf("failure reply completion changed: processed=%t comments=%d", processed, len(gh.comments))
			}
		})
	}
}

func TestRunOnceCanceledExecutionPreservesEarlierFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		executeErr error
		terminal   bool
		processed  bool
		attempts   int
		saveFails  bool
	}{
		{name: "interrupted", executeErr: context.Canceled},
		{name: "completed", processed: true},
		{name: "publication", executeErr: ErrPostModelPublication, attempts: 1},
		{name: "terminal publication", executeErr: ErrPostModelPublication, terminal: true, processed: true},
		{name: "interrupted state save fails", executeErr: context.Canceled, saveFails: true},
		{name: "completed state save fails", processed: true, saveFails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
			initial := now.Add(-time.Minute)
			bad := github.Item{Key: "comment:bad", NodeID: "node:bad", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}
			item := bad
			item.Key, item.NodeID = "comment:good", "node:good"
			first, tail := testNotification(now), testNotification(now.Add(time.Second))
			tail.ID = "thread-2"
			tail.Subject.URL = strings.Replace(tail.Subject.URL, "/7", "/8", 1)
			earlier := errors.New("earlier CurrentItem failure")
			gh := &fakeGitHub{notifications: []github.Notification{first, tail}, items: []github.Item{bad, item}, permission: github.Permission{Permission: "write"}, currentItem: func(current github.Item) (github.Item, error) {
				if current.Key == bad.Key {
					return github.Item{}, earlier
				}
				return current, nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			statePath := filepath.Join(t.TempDir(), "state.json")
			store, err := state.Open(statePath, initial)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			executor := &fakeExecutor{onExecute: func() {
				cancel()
				if test.saveFails {
					if err := os.RemoveAll(filepath.Dir(statePath)); err != nil {
						t.Fatal(err)
					}
				}
			}, executeErr: test.executeErr}
			m := &Monitor{Config: testConfig(t), GitHub: gh, Executor: executor, Now: func() time.Time { return now }}
			cmd, err := command.ParseForLoginCommands(item.Body, m.Config.BotLogin, m.availableCommands(m.Config.Repositories[0]))
			if err != nil {
				t.Fatal(err)
			}
			id := jobID(item.Key, cmd.Key)
			if test.terminal {
				store.Data.JobFailures[id] = state.JobFailure{Attempts: maxJobAttempts - 1}
				gh.items = append(gh.items, github.Item{Body: "failure\n\n" + (Job{ID: id}).Marker(), Author: github.User{Login: m.Config.BotLogin}})
			}
			pending := state.PendingNotification{Repository: tail.Repository.FullName, RepositoryID: 1, OwnerID: 2, SubjectType: tail.Subject.Type, SubjectURL: tail.Subject.URL,
				Since: initial, UpdatedAt: tail.UpdatedAt, RetryAt: now.Add(-time.Second), Failures: 5}
			key := notificationKey(tail)
			store.Data.PendingNotifications[key] = pending
			store.Data.RepositoryCursors[first.Repository.FullName] = initial
			_, err = m.RunOnce(ctx, store)
			if !errors.Is(err, earlier) || !errors.Is(err, context.Canceled) || test.executeErr != nil && !errors.Is(err, test.executeErr) {
				t.Fatalf("cancellation lost an earlier/current failure or parent context: %v", err)
			}
			if test.saveFails && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cancellation lost state-save failure: %v", err)
			}
			if len(executor.jobs) != 1 || len(gh.comments) != 0 || len(store.Data.PendingNotifications) != 1 || store.Data.PendingNotifications[key] != pending ||
				!store.Data.Cursor.Equal(initial) || !store.Data.RepositoryCursors[first.Repository.FullName].Equal(initial) {
				t.Fatalf("canceled execution continued discovery or changed backoff/cursors: jobs=%d state=%+v", len(executor.jobs), store.Data)
			}
			_, processed := store.Data.Processed[id]
			failure, remains := store.Data.JobFailures[id]
			if processed != test.processed || remains == test.processed || failure.Attempts != test.attempts {
				t.Fatalf("cancellation changed completion/attempt policy: processed=%t failure=%+v remains=%t", processed, failure, remains)
			}
		})
	}
}

func (f *cancelDiscoveryGitHub) call(stage string) error {
	f.calls = append(f.calls, stage)
	if stage == f.stage {
		f.cancel()
		return f.err
	}
	return nil
}

func (f *cancelDiscoveryGitHub) ListRepositoryNotifications(ctx context.Context, repo string, since time.Time) ([]github.Notification, time.Duration, error) {
	if err := f.call("notifications"); err != nil {
		return nil, 0, err
	}
	return f.fakeGitHub.ListRepositoryNotifications(ctx, repo, since)
}

func (f *cancelDiscoveryGitHub) Repository(ctx context.Context, repo string) (github.Repository, error) {
	if err := f.call("repository"); err != nil {
		return github.Repository{}, err
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

func TestRunOnceStopsCanceledDiscoveryWithoutDeferring(t *testing.T) {
	stages := []string{"notifications", "repository", "thread", "current", "permission", "reaction"}
	for _, stage := range append(slices.Clone(stages), "empty thread", "before start", "invitations") {
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
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				gh := &cancelDiscoveryGitHub{
					fakeGitHub: &fakeGitHub{notifications: []github.Notification{first, tail}, items: []github.Item{item, item}, permission: github.Permission{Permission: "write"}},
					stage:      stage,
					cancel:     cancel,
				}
				if stage == "empty thread" {
					gh.stage = "thread"
					gh.items = nil
				}
				if returnError {
					gh.err = context.Canceled
				}
				path := filepath.Join(t.TempDir(), "state.json")
				store, err := state.Open(path, initial)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := store.Close(); err != nil {
						t.Error(err)
					}
				})
				pending := state.PendingNotification{Repository: tail.Repository.FullName, RepositoryID: 1, OwnerID: 2, SubjectType: tail.Subject.Type, SubjectURL: tail.Subject.URL,
					Since: initial, UpdatedAt: tail.UpdatedAt, RetryAt: now.Add(-time.Second), Failures: 5}
				key := notificationKey(tail)
				store.Data.PendingNotifications[key] = pending
				store.Data.RepositoryCursors[first.Repository.FullName] = initial
				store.Data.NextInviteCheckAt = now.Add(time.Minute)
				if stage == "before start" {
					cancel()
				} else if stage == "invitations" {
					store.Data.NextInviteCheckAt = time.Time{}
					gh.beforeList = cancel
				}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				executor := &fakeExecutor{}
				m := &Monitor{Config: testConfig(t), GitHub: gh, Executor: executor, Now: func() time.Time { return now }}
				if _, err := m.RunOnce(ctx, store); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled discovery: %v", err)
				}
				if want := stages[:slices.Index(stages, gh.stage)+1]; !slices.Equal(gh.calls, want) {
					t.Fatalf("discovery continued after cancellation: got %v, want %v", gh.calls, want)
				}
				if len(executor.jobs) != 0 || len(gh.comments) != 0 {
					t.Fatalf("canceled discovery executed or replied: jobs=%d comments=%d", len(executor.jobs), len(gh.comments))
				}
				checkState := func(data state.Data) {
					t.Helper()
					if len(data.PendingNotifications) != 1 || data.PendingNotifications[key] != pending {
						t.Fatalf("shutdown changed retry state: %+v", data.PendingNotifications)
					}
					if !data.Cursor.Equal(initial) || !data.RepositoryCursors[first.Repository.FullName].Equal(initial) {
						t.Fatalf("shutdown advanced cursors: %+v", data)
					}
					if len(data.JobFailures) != 0 || len(data.Processed) != 0 || len(data.PendingFixes) != 0 {
						t.Fatalf("shutdown consumed work: %+v", data)
					}
				}
				checkState(store.Data)
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := state.Open(path, initial)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := reopened.Close(); err != nil {
						t.Error(err)
					}
				})
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
	m := &Monitor{Config: testConfig(t), GitHub: gh, Executor: &fakeExecutor{}, Now: func() time.Time { return now }}
	if _, err := m.RunOnce(context.Background(), store); !errors.Is(err, context.Canceled) {
		t.Fatalf("internal discovery cancellation: %v", err)
	}
	if pending := store.Data.PendingNotifications[notificationKey(gh.notifications[0])]; len(store.Data.PendingNotifications) != 1 || pending.Failures != 1 || !pending.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("internal cancellation did not retain ordinary retry: %+v", store.Data.PendingNotifications)
	}
	if !store.Data.Cursor.After(now.Add(-time.Minute)) {
		t.Fatalf("internal cancellation pinned the cursor: %+v", store.Data)
	}
}
