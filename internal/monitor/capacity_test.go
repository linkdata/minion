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

	"github.com/linkdata/minion/internal/config"
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
			clock := now
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
			m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return clock }}
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
			if test.failAt != 0 && (gh.currentChecks != 1 || len(gh.reactions) != 1 || len(saved.PendingNotifications) != 1 || !saved.Cursor.Equal(initial) || !saved.RepositoryCursors["example-owner/example-repo"].Equal(initial)) {
				t.Fatalf("discovery or cursor advanced after capacity loss: current checks=%d reactions=%d cursor=%s repo cursor=%s", gh.currentChecks, len(gh.reactions), saved.Cursor, saved.RepositoryCursors["example-owner/example-repo"])
			}
			if test.failAt != 0 {
				gh.notifications = nil
				handler.failAt = 0
				clock = clock.Add(time.Minute)
				if _, err := m.RunOnce(t.Context(), store); err != nil {
					t.Fatalf("retry after notification disappeared: %v", err)
				}
				if len(handler.jobs) != 2 || handler.jobs[1].Source.Key != "comment:2" || len(store.Data.Processed) != 2 || len(store.Data.PendingNotifications) != 0 {
					t.Fatalf("second action was lost or first was repeated: jobs=%v processed=%v pending=%v", handler.jobs, store.Data.Processed, store.Data.PendingNotifications)
				}
			}
		})
	}
}

func TestRunOnceCapacityLossPersistsEarlierThreadFailure(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	initial := now.Add(-time.Minute)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, initial)
	bad := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}
	good := github.Item{Key: "comment:2", NodeID: "node:2", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(now)},
		items:         []github.Item{bad, good},
		permission:    github.Permission{Permission: "write"},
		currentItem: func(item github.Item) (github.Item, error) {
			if item.Key == bad.Key {
				return github.Item{}, errors.New("provenance unavailable")
			}
			return item, nil
		},
	}
	unavailable := errors.New("capacity unavailable")
	handler := &capacityHandler{failAt: 2, err: unavailable}
	clock := now
	m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return clock }}
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, unavailable) {
		t.Fatalf("capacity loss = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStateAt(t, path, initial)
	if len(store.Data.Processed) != 1 || len(store.Data.PendingNotifications) != 1 || !store.Data.Cursor.Equal(initial) {
		t.Fatalf("capacity loss did not retain accepted action and retry: %+v", store.Data)
	}
	gh.notifications = nil
	gh.currentItem = nil
	handler.failAt = 0
	clock = clock.Add(time.Minute)
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatalf("retry after notification disappeared: %v", err)
	}
	if len(store.Data.Processed) != 2 || len(store.Data.PendingNotifications) != 0 || len(handler.jobs) != 2 || handler.jobs[1].Source.Key != bad.Key {
		t.Fatalf("lost failed action after capacity returned: state=%+v jobs=%+v", store.Data, handler.jobs)
	}
}

func TestSelectedCommandsDoNotExpandWhenSkillAppears(t *testing.T) {
	cfg := testConfig(t)
	fixSkill := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "fix", "SKILL.md")
	if err := os.Remove(fixSkill); err != nil {
		t.Fatal(err)
	}
	m := &Monitor{Config: cfg, SelectedCommands: map[string]bool{"review": true, "audit": true}}
	if got := m.availableCommands(cfg.Repositories[0]); len(got) != 2 {
		t.Fatalf("selected commands before install = %v", got)
	}
	if err := os.WriteFile(fixSkill, []byte("Use this test skill.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := m.availableCommands(cfg.Repositories[0]); len(got) != 2 {
		t.Fatalf("later skill expanded frozen command selection: %v", got)
	}
}

func TestSelectedWorkerDefersInvitedRepositoryUntilRestart(t *testing.T) {
	cfg := testConfig(t)
	cfg.InvitationOwners = []string{"example-owner"}
	cfg.InvitationOwnerIDs = map[string]int64{"example-owner": 2}
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{invitations: []github.Invitation{{
		ID: 1, Repository: github.Repository{FullName: "example-owner/new-repo", Owner: github.User{Login: "example-owner"}}, Permissions: "write",
	}}}
	m := &Monitor{Config: cfg, ConfigPath: path, GitHub: gh, SelectedCommands: map[string]bool{"review": true}}
	if err := m.acceptWhitelistedInvitations(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(m.Config.Repositories) != 1 || len(gh.accepted) != 1 {
		t.Fatalf("new repository became active before restart: repos=%v accepted=%v", m.Config.Repositories, gh.accepted)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	newRepo, ok := saved.Repository("example-owner/new-repo")
	if !ok || len(newRepo.Actions) != len(cfg.Commands) {
		t.Fatalf("invitation was not persisted for a new worker selection: %+v", saved.Repositories)
	}
	if got := m.availableCommands(newRepo); len(got) != 1 {
		t.Fatalf("selected commands changed for persisted repository: %v", got)
	}
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	notification := testNotification(now)
	notification.Repository = github.Repository{FullName: newRepo.Name, ID: newRepo.ID, Owner: github.User{ID: newRepo.OwnerID}}
	gh.invitations = nil
	gh.notifications = []github.Notification{notification}
	m.Handler = &capacityHandler{}
	m.Now = func() time.Time { return now }
	if _, err := m.RunOnce(t.Context(), openState(t, now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if len(gh.notificationCalls) != 1 || gh.notificationCalls[0].repo != cfg.Repositories[0].Name || gh.threadCalls != 0 {
		t.Fatalf("invited repository was scanned before restart: notifications=%v threads=%d", gh.notificationCalls, gh.threadCalls)
	}
}
