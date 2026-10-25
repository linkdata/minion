package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type capacityHandler struct {
	jobs   []Job
	events []string
	waits  int
	failAt int
	err    error
}

func (h *capacityHandler) WaitCapacity(context.Context) error {
	h.waits++
	h.events = append(h.events, "wait")
	if h.waits == h.failAt {
		return h.err
	}
	return nil
}

func (h *capacityHandler) Execute(_ context.Context, job Job) error {
	h.jobs = append(h.jobs, job)
	h.events = append(h.events, "execute:"+job.Source.Key)
	return nil
}

func TestRunOnceWaitsBeforeAnyDiscoveryOrStateMutation(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, now.Add(-time.Minute))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, beforeList: func() { t.Fatal("invitation discovery ran without capacity") }}
	unavailable := errors.New("capacity unavailable")
	handler := &capacityHandler{failAt: 1, err: unavailable}
	m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return now }}
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, unavailable) {
		t.Fatalf("capacity error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || len(gh.notificationCalls) != 0 || len(handler.jobs) != 0 || !slices.Equal(handler.events, []string{"wait"}) {
		t.Fatalf("unavailable capacity changed state or discovered work: state changed=%t notifications=%d jobs=%d events=%v", !bytes.Equal(after, before), len(gh.notificationCalls), len(handler.jobs), handler.events)
	}
}

func TestRunOnceWaitsAfterEachPersistedAction(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		failAt int
		want   []string
	}{
		{"both actions", 0, []string{"wait", "execute:comment:1", "wait", "execute:comment:2", "wait"}},
		{"capacity lost after first action", 2, []string{"wait", "execute:comment:1", "wait"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			initial := now.Add(-time.Minute)
			path := filepath.Join(t.TempDir(), "state.json")
			store := openStateAt(t, path, initial)
			gh := &fakeGitHub{
				notifications: []github.Notification{testNotification(now)},
				items: []github.Item{
					{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}},
					{Key: "comment:2", NodeID: "node:2", Body: "@linkdataminion review", Author: github.User{Login: "writer"}},
				},
				permission: github.Permission{Permission: "write"},
			}
			unavailable := errors.New("capacity unavailable")
			handler := &capacityHandler{failAt: test.failAt, err: unavailable}
			m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return now }}
			_, err := m.RunOnce(t.Context(), store)
			if test.failAt == 0 && err != nil || test.failAt != 0 && !errors.Is(err, unavailable) {
				t.Fatalf("RunOnce error = %v", err)
			}
			if !slices.Equal(handler.events, test.want) {
				t.Fatalf("events = %v, want %v", handler.events, test.want)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved state.Data
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if len(saved.Processed) != len(handler.jobs) {
				t.Fatalf("persisted jobs = %d, executed = %d", len(saved.Processed), len(handler.jobs))
			}
			if test.failAt != 0 && (gh.currentChecks != 1 || len(gh.reactions) != 1 || !saved.Cursor.Equal(initial) || !saved.RepositoryCursors["example-owner/example-repo"].Equal(initial)) {
				t.Fatalf("discovery or cursor advanced after capacity loss: current checks=%d reactions=%d cursor=%s repo cursor=%s", gh.currentChecks, len(gh.reactions), saved.Cursor, saved.RepositoryCursors["example-owner/example-repo"])
			}
		})
	}
}
