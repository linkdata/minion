package monitor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func saveModelCallStart(t *testing.T, store *state.Store, id string) {
	t.Helper()
	failure := store.Data.JobFailures[id]
	failure.ModelCallStarted = true
	store.Data.JobFailures[id] = failure
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestRunOnceRestartFinishesAdmittedModelWithoutExecution(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, items: []github.Item{item}, permission: github.Permission{Permission: "write"}}
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, now.Add(-time.Minute))
	id := jobID(item.Key, "review")
	store.Data.JobFailures[id] = state.JobFailure{Attempts: 1, ModelCallStarted: true}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, now.Add(-time.Minute))
	handler := &fakeHandler{onExecute: func() { t.Fatal("restart dispatched an admitted model call") }}
	m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return now }}
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || len(handler.jobs) != 0 || len(gh.comments) != 1 || len(reopened.Data.Processed) != 1 || len(reopened.Data.JobFailures) != 0 {
		t.Fatalf("admitted call restarted or lacked a failure reply: err=%v jobs=%d replies=%d state=%+v", err, len(handler.jobs), len(gh.comments), reopened.Data)
	}
}

func TestRunOnceCanceledAdmittedModelDoesNotRetry(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, items: []github.Item{item}, permission: github.Permission{Permission: "write"}}
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, now.Add(-time.Minute))
	id := jobID(item.Key, "review")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := &fakeHandler{executeErr: context.Canceled}
	handler.onExecute = func() {
		saveModelCallStart(t, store, id)
		cancel()
	}
	m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return now }}
	if _, err := m.RunOnce(ctx, store); !errors.Is(err, context.Canceled) || len(handler.jobs) != 1 || len(gh.comments) != 1 || len(store.Data.Processed) != 1 {
		t.Fatalf("canceled admitted call was refunded or retried: err=%v jobs=%d replies=%d state=%+v", err, len(handler.jobs), len(gh.comments), store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, now.Add(-time.Minute))
	handler.onExecute = func() { t.Fatal("canceled model call restarted") }
	m.Now = func() time.Time { return now.Add(time.Minute) }
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || len(handler.jobs) != 1 || len(gh.comments) != 1 {
		t.Fatalf("canceled admitted call did not remain finished: err=%v jobs=%d replies=%d", err, len(handler.jobs), len(gh.comments))
	}
}
