package monitor

import (
	"errors"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func TestLegacyRetryKeepsFirstRuntimeDeadline(t *testing.T) {
	scanAt := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := selectedItem(scanAt)
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(scanAt)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}}
	store := openState(t, scanAt.Add(-time.Minute))
	handler := &fakeHandler{executeErr: errors.New("temporary failure")}
	cfg := testConfig(t)
	cfg.MaxRuntimeMinutes = 7
	clock := scanAt
	m := &Monitor{Config: cfg, GitHub: gh, Handler: handler, Now: func() time.Time { return clock }}
	if _, err := m.RunOnce(t.Context(), store); err == nil || len(handler.jobs) != 1 {
		t.Fatalf("first attempt did not fail: err=%v jobs=%v", err, handler.jobs)
	}
	first := handler.jobs[0]
	if first.AcceptedAt.IsZero() || !first.Deadline.Equal(first.AcceptedAt.Add(7*time.Minute)) ||
		!store.Data.JobFailures[first.ID].AcceptedAt.Equal(first.AcceptedAt) || !store.Data.JobFailures[first.ID].Deadline.Equal(first.Deadline) {
		t.Fatalf("first runtime window was not saved: job=%+v state=%+v", first, store.Data.JobFailures[first.ID])
	}
	handler.executeErr = nil
	clock = advanceToRetry(clock, store)
	if _, err := m.RunOnce(t.Context(), store); err != nil || len(handler.jobs) != 2 {
		t.Fatalf("legacy retry failed: err=%v jobs=%v", err, handler.jobs)
	}
	second := handler.jobs[1]
	if !second.AcceptedAt.Equal(first.AcceptedAt) || !second.Deadline.Equal(first.Deadline) || len(store.Data.Processed) != 1 {
		t.Fatalf("legacy retry renewed deadline: first=%+v second=%+v state=%+v", first, second, store.Data)
	}
}

func TestLegacyExpiredRetryStopsBeforeExecution(t *testing.T) {
	scanAt := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := selectedItem(scanAt)
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(scanAt)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}}
	store := openState(t, scanAt.Add(-time.Minute))
	firstAt := time.Now().UTC().Add(-9 * time.Minute)
	id := jobID(item.Key, "review")
	store.Data.JobFailures[id] = state.JobFailure{Attempts: 1, AcceptedAt: firstAt, Deadline: firstAt.Add(7 * time.Minute)}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	handler := &fakeHandler{onExecute: func() { t.Fatal("expired legacy job dispatched") }}
	cfg := testConfig(t)
	cfg.MaxRuntimeMinutes = 7
	m := &Monitor{Config: cfg, GitHub: gh, Handler: handler, Now: func() time.Time { return scanAt }}
	if _, err := m.RunOnce(t.Context(), store); err != nil || len(gh.comments) != 1 ||
		len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 || len(handler.jobs) != 0 {
		t.Fatalf("expired legacy retry renewed runtime: err=%v comments=%+v state=%+v", err, gh.comments, store.Data)
	}
}
