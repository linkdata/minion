package monitor

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func TestConfigUpdateReplaysUnacceptedSiblingWithNewPolicy(t *testing.T) {
	now := time.Now().UTC()
	initial := now.Add(-time.Minute)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, initial)
	first := selectedItem(now)
	second := selectedItem(now.Add(time.Second))
	second.Key, second.NodeID = "comment:2", "node:2"
	second.HTMLURL = "https://github.com/example-owner/example-repo/pull/7#issuecomment-2"
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(now)},
		items:         []github.Item{first, second},
		permission:    github.Permission{Permission: "write"},
	}
	updates := make(chan struct{}, 1)
	var executed []string
	m := selectedMonitor(t, now, gh, selectedHandler(func(job Job) error {
		executed = append(executed, job.Source.Key)
		if len(executed) == 1 {
			updates <- struct{}{}
		}
		return nil
	}))
	m.ConfigUpdates = updates
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, ErrConfigUpdatePending) {
		t.Fatalf("config update after first action: %v", err)
	}
	key := notificationKey(testNotification(now))
	if len(executed) != 1 || executed[0] != first.Key || store.Data.Accepted != nil ||
		!store.Data.Processed[jobID(second.Key, "review")].IsZero() ||
		store.Data.PendingNotifications[key].Repository == "" ||
		!store.Data.Cursor.Equal(initial) || !store.Data.RepositoryCursors["example-owner/example-repo"].Equal(initial) {
		t.Fatalf("config update consumed sibling: executed=%v state=%+v", executed, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStateAt(t, path, initial)
	if store.Data.PendingNotifications[key].Repository == "" {
		t.Fatalf("unaccepted sibling was not saved for replay: %+v", store.Data.PendingNotifications)
	}
	gh.notifications = nil
	m.ConfigUpdates = nil
	m.Config.Repositories[0].DenyAllActors = true
	m.Now = func() time.Time { return store.Data.NextPollAt.Add(time.Second) }
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatalf("replay with requester denied: %v", err)
	}
	if len(executed) != 1 || store.Data.AcceptedTotal != 1 ||
		store.Data.Processed[jobID(second.Key, "review")].IsZero() || len(store.Data.PendingNotifications) != 0 {
		t.Fatalf("replayed sibling under old requester policy: executed=%v state=%+v", executed, store.Data)
	}
}

func TestConfigUpdateRefreshesNewerPendingNotification(t *testing.T) {
	now := time.Now().UTC()
	initial := now.Add(-2 * time.Minute)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, initial)
	notification := testNotification(now)
	key := notificationKey(notification)
	store.Data.PendingNotifications[key] = state.PendingNotification{
		Repository: "example-owner/example-repo", RepositoryID: 1, OwnerID: 2,
		SubjectType: notification.Subject.Type, SubjectURL: notification.Subject.URL,
		Since: initial, UpdatedAt: now.Add(-time.Minute), RetryAt: now.Add(time.Hour), Failures: 7,
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	item := selectedItem(now)
	updates := make(chan struct{}, 1)
	gh := &fakeGitHub{
		notifications: []github.Notification{notification},
		items:         []github.Item{item},
		permission:    github.Permission{Permission: "write"},
		currentItem: func(item github.Item) (github.Item, error) {
			updates <- struct{}{}
			return item, nil
		},
	}
	var executions int
	m := selectedMonitor(t, now, gh, selectedHandler(func(Job) error {
		executions++
		return nil
	}))
	m.ConfigUpdates = updates
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, ErrConfigUpdatePending) {
		t.Fatalf("config update before acceptance: %v", err)
	}
	pending := store.Data.PendingNotifications[key]
	if executions != 0 || !pending.UpdatedAt.Equal(now) || !pending.RetryAt.IsZero() || pending.Failures != 7 {
		t.Fatalf("new notification kept stale backoff: executions=%d pending=%+v", executions, pending)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStateAt(t, path, initial)
	gh.notifications = nil
	gh.currentItem = nil
	m.ConfigUpdates = nil
	m.Now = func() time.Time { return store.Data.NextPollAt.Add(time.Second) }
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatalf("replay refreshed notification: %v", err)
	}
	if executions != 1 || store.Data.Processed[jobID(item.Key, "review")].IsZero() || len(store.Data.PendingNotifications) != 0 {
		t.Fatalf("refreshed notification did not replay: executions=%d state=%+v", executions, store.Data)
	}
}

func TestConfigUpdatePreservesCommandDeniedByOldRequesterPolicy(t *testing.T) {
	now := time.Now().UTC()
	initial := now.Add(-time.Minute)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, initial)
	item := selectedItem(now)
	updatePending := false
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(now)},
		items:         []github.Item{item},
		permission:    github.Permission{Permission: "write"},
		currentItem: func(item github.Item) (github.Item, error) {
			updatePending = true // Simulate an admin save while the scan uses its old policy.
			return item, nil
		},
	}
	var executed, decisions int
	m := selectedMonitor(t, now, gh, selectedHandler(func(Job) error {
		executed++
		return nil
	}))
	m.Config.Repositories[0].DenyAllActors = true
	m.BeforeAccept = func(decide func() error) error {
		decisions++
		if updatePending {
			return ErrConfigUpdatePending
		}
		return decide()
	}
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, ErrConfigUpdatePending) {
		t.Fatalf("saved grant did not stop old-policy denial: %v", err)
	}
	key := notificationKey(testNotification(now))
	if decisions != 1 || executed != 0 || !store.Data.Processed[jobID(item.Key, "review")].IsZero() ||
		store.Data.PendingNotifications[key].Repository == "" || !store.Data.RepositoryCursors["example-owner/example-repo"].Equal(initial) {
		t.Fatalf("old policy consumed newly granted command: decisions=%d executed=%d state=%+v", decisions, executed, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStateAt(t, path, initial)
	m.Config.Repositories[0].DenyAllActors = false
	m.Config.Repositories[0].AllowedActors = []string{"writer"}
	m.Config.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
	updatePending = false
	gh.notifications, gh.currentItem = nil, nil
	m.Now = func() time.Time { return store.Data.NextPollAt.Add(time.Second) }
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatalf("replay with granted requester: %v", err)
	}
	if decisions != 2 || executed != 1 || store.Data.Processed[jobID(item.Key, "review")].IsZero() || len(store.Data.PendingNotifications) != 0 {
		t.Fatalf("granted command did not replay: decisions=%d executed=%d state=%+v", decisions, executed, store.Data)
	}
}
