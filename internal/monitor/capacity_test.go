package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

type heldCapacityHandler struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	waits   atomic.Int32
	cancels atomic.Int32
	jobs    []Job
}

func (h *heldCapacityHandler) WaitCapacity(ctx context.Context) error {
	h.waits.Add(1)
	h.once.Do(func() { close(h.entered) })
	select {
	case <-ctx.Done():
		h.cancels.Add(1)
		return ctx.Err()
	case <-h.release:
		return nil
	}
}

func (h *heldCapacityHandler) Execute(_ context.Context, job Job) error {
	h.jobs = append(h.jobs, job)
	return nil
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

func TestRunOnceWaitsBeforeActionDiscoveryOrStateMutation(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, now.Add(-time.Minute))
	store.Data.NextInviteCheckAt = now.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, beforeList: func() { t.Fatal("invitation check ran before due time") }}
	workerExited := errors.New("selected worker exited")
	handler := &capacityHandler{failAt: 1, err: workerExited}
	m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return now }}
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, workerExited) {
		t.Fatalf("worker exit = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || len(gh.notificationCalls) != 0 || len(handler.jobs) != 0 || !slices.Equal(handler.events, []string{"wait"}) {
		t.Fatalf("unavailable capacity changed state or discovered work: state changed=%t notifications=%d jobs=%d events=%v", !bytes.Equal(after, before), len(gh.notificationCalls), len(handler.jobs), handler.events)
	}
}

func TestRunOnceCancellationWhileIdlePreservesDiscoveryState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		initial := now.Add(-time.Minute)
		store := openState(t, initial)
		store.Data.NextInviteCheckAt = now.Add(time.Hour)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &fakeGitHub{}
		handler := &heldCapacityHandler{entered: make(chan struct{}), release: make(chan struct{})}
		ctx, cancel := context.WithCancel(t.Context())
		m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, SelectedExecutorID: "local"}
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(ctx, store); result <- err }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("idle cancellation = %v", err)
		}
		if len(gh.notificationCalls) != 0 {
			t.Error("discovery ran after idle cancellation")
		}
		if !store.Data.Cursor.Equal(initial) || !store.Data.NextPollAt.IsZero() ||
			!store.Data.NextInviteCheckAt.Equal(now.Add(time.Hour)) || len(handler.jobs) != 0 {
			t.Fatalf("idle cancellation changed discovery state or dispatched work: jobs=%v state=%+v", handler.jobs, store.Data)
		}
	})
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
			for _, job := range handler.jobs {
				if job.AcceptedAt.IsZero() {
					t.Fatalf("accepted job lacks a runtime start: %+v", job)
				}
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
				for _, pending := range saved.PendingNotifications {
					if pending.Failures != 0 || !pending.RetryAt.IsZero() {
						t.Fatalf("capacity loss counted as a thread failure: %+v", pending)
					}
				}
				gh.notifications = nil
				handler.failAt = 0
				clock = clock.Add(time.Minute)
				if _, err := m.RunOnce(t.Context(), store); err != nil {
					t.Fatalf("replay after capacity returned: %v", err)
				}
				if len(handler.jobs) != 2 || handler.jobs[1].Source.Key != "comment:2" || len(store.Data.Processed) != 2 || len(store.Data.PendingNotifications) != 0 {
					t.Fatalf("unaccepted action was lost or accepted action repeated: jobs=%v processed=%v pending=%v", handler.jobs, store.Data.Processed, store.Data.PendingNotifications)
				}
			}
		})
	}
}

func TestRunOnceCapacityLossPreservesPendingRetryBytes(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	initial := now.Add(-time.Minute)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, initial)
	pendingNotification := testNotification(now.Add(-time.Minute))
	pendingNotification.ID = "pending-thread"
	pendingNotification.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/8"
	pending := state.PendingNotification{
		Repository: pendingNotification.Repository.FullName, RepositoryID: 1, OwnerID: 2,
		SubjectType: pendingNotification.Subject.Type, SubjectURL: pendingNotification.Subject.URL,
		Since: initial.Add(-time.Hour), UpdatedAt: pendingNotification.UpdatedAt,
		RetryAt: now.Add(20 * time.Minute), Failures: 4,
	}
	pendingKey := notificationKey(pendingNotification)
	store.Data.PendingNotifications[pendingKey] = pending
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{
		notifications: []github.Notification{pendingNotification, testNotification(now)},
		items: []github.Item{
			{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}},
			{Key: "comment:2", NodeID: "node:2", Body: "@linkdataminion review", Author: github.User{Login: "writer"}},
		},
		permission: github.Permission{Permission: "write"},
		currentItem: func(item github.Item) (github.Item, error) {
			if item.Key == "comment:1" {
				return github.Item{}, errors.New("provenance unavailable")
			}
			return item, nil
		},
	}
	unavailable := errors.New("capacity unavailable")
	handler := &capacityHandler{failAt: 2, err: unavailable}
	m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return now }}
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, unavailable) {
		t.Fatalf("capacity loss = %v", err)
	}
	if len(handler.jobs) != 1 || gh.threadCalls != 1 || len(store.Data.Processed) != 1 || !store.Data.Cursor.Equal(initial) ||
		!store.Data.RepositoryCursors["example-owner/example-repo"].Equal(initial) {
		t.Fatalf("capacity loss changed action or cursor state: jobs=%v state=%+v", handler.jobs, store.Data)
	}
	failed := store.Data.PendingNotifications[notificationKey(testNotification(now))]
	if failed.Failures != 1 || !failed.Since.Equal(initial) {
		t.Fatalf("independent failure retry changed by capacity loss: %+v", failed)
	}
	after, err := json.Marshal(store.Data.PendingNotifications[pendingKey])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("capacity loss changed pending retry bytes: before=%s after=%s", before, after)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStateAt(t, path, initial)
	failed = store.Data.PendingNotifications[notificationKey(testNotification(now))]
	if failed.Failures != 1 || !failed.Since.Equal(initial) {
		t.Fatalf("independent failure retry was not durable: %+v", failed)
	}
	after, err = json.Marshal(store.Data.PendingNotifications[pendingKey])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("restart changed pending retry bytes: before=%s after=%s", before, after)
	}
}

func TestRunOnceInterleavedRepositoriesReplayOnlyUnacceptedActions(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	initial := now.Add(-time.Minute)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, initial)
	makeNotification := func(repo string, number, seconds int) github.Notification {
		return github.Notification{
			ID: repo + ":" + strconv.Itoa(number), Repository: github.Repository{FullName: repo},
			Subject:   github.Subject{Type: "Issue", URL: "https://api.github.com/repos/" + repo + "/issues/" + strconv.Itoa(number)},
			UpdatedAt: now.Add(time.Duration(seconds) * time.Second),
		}
	}
	firstRepo, secondRepo := "a/one", "b/two"
	gh := &fakeGitHub{
		notifications: []github.Notification{
			makeNotification(firstRepo, 1, 0), makeNotification(secondRepo, 2, 1),
			makeNotification(firstRepo, 3, 2), makeNotification(secondRepo, 4, 3),
		},
		permission: github.Permission{Permission: "write"},
		threadItems: func(repo string, number int, _ time.Time) ([]github.Item, error) {
			key := repo + ":" + strconv.Itoa(number)
			return []github.Item{{Key: key, NodeID: "node:" + key, Body: "@linkdataminion review",
				Author: github.User{Login: "writer"}}}, nil
		},
	}
	cfg := testConfig(t)
	cfg.Repositories = []config.Repository{
		{Name: firstRepo, ID: 1, OwnerID: 2, Actions: []string{"review"}},
		{Name: secondRepo, ID: 1, OwnerID: 2, Actions: []string{"review"}},
	}
	clock := now
	unavailable := errors.New("capacity unavailable")
	handler := &capacityHandler{failAt: 3, err: unavailable}
	m := &Monitor{Config: cfg, GitHub: gh, Handler: handler, Now: func() time.Time { return clock }}
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, unavailable) {
		t.Fatalf("capacity loss = %v", err)
	}
	if len(handler.jobs) != 2 || handler.jobs[0].Source.Key != "a/one:1" || handler.jobs[1].Source.Key != "b/two:2" ||
		len(store.Data.Processed) != 2 || len(store.Data.PendingNotifications) != 1 ||
		!store.Data.Cursor.Equal(initial) || !store.Data.RepositoryCursors[firstRepo].Equal(initial) ||
		!store.Data.RepositoryCursors[secondRepo].Equal(initial) {
		t.Fatalf("partial interleaved batch changed unaccepted state: jobs=%v state=%+v", handler.jobs, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStateAt(t, path, initial)
	handler.failAt = 0
	clock = now.Add(time.Minute)
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	want := []string{"a/one:1", "b/two:2", "a/one:3", "b/two:4"}
	got := make([]string, 0, len(handler.jobs))
	for _, job := range handler.jobs {
		got = append(got, job.Source.Key)
	}
	if !slices.Equal(got, want) || len(store.Data.Processed) != 4 || len(store.Data.PendingNotifications) != 0 ||
		!store.Data.RepositoryCursors[firstRepo].After(initial) || !store.Data.RepositoryCursors[secondRepo].After(initial) {
		t.Fatalf("replay lost or duplicated interleaved actions: jobs=%v state=%+v", got, store.Data)
	}
}

func TestRunOnceIdleResumesAndRejectsInvalidPendingIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		store := openState(t, now.Add(-time.Minute))
		store.Data.NextInviteCheckAt = now.Add(time.Hour)
		invalid := testNotification(now)
		invalid.ID = "invalid-thread"
		invalid.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/pulls/8"
		key := notificationKey(invalid)
		store.Data.PendingNotifications[key] = state.PendingNotification{
			Repository: invalid.Repository.FullName, RepositoryID: 9, OwnerID: 2,
			SubjectType: invalid.Subject.Type, SubjectURL: invalid.Subject.URL,
			Since: now.Add(-time.Minute), UpdatedAt: now, RetryAt: now.Add(-time.Second), Failures: 2,
		}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &fakeGitHub{
			notifications: []github.Notification{testNotification(now)},
			items: []github.Item{{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review",
				Author: github.User{Login: "writer"}}},
			permission: github.Permission{Permission: "write"},
		}
		handler := &heldCapacityHandler{entered: make(chan struct{}), release: make(chan struct{})}
		m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler, Now: func() time.Time { return time.Now() }}
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(t.Context(), store); result <- err }()
		synctest.Wait()
		if _, exists := store.Data.PendingNotifications[key]; !exists {
			t.Error("invalid pending identity was touched before capacity")
		}
		select {
		case <-handler.entered:
		default:
			t.Error("monitor did not wait for a held pull")
		}
		if len(gh.notificationCalls) != 0 {
			t.Error("GitHub action discovery ran before capacity")
		}
		close(handler.release)
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if len(handler.jobs) != 1 || handler.jobs[0].Source.Key != "comment:1" ||
			len(store.Data.PendingNotifications) != 0 || gh.threadCalls != 1 {
			t.Fatalf("ready pull failed to resume valid work or reject invalid retry: jobs=%v pending=%v threads=%d",
				handler.jobs, store.Data.PendingNotifications, gh.threadCalls)
		}
	})
}

func TestRunOnceChecksInvitationThatBecomesDueWhileIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		initial := now.Add(-time.Minute)
		statePath := filepath.Join(t.TempDir(), "state.json")
		store := openStateAt(t, statePath, initial)
		store.Data.NextInviteCheckAt = now.Add(time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(t)
		cfg.InvitationOwners = []string{"example-owner"}
		cfg.InvitationOwnerIDs = map[string]int64{"example-owner": 2}
		configPath := filepath.Join(t.TempDir(), "config.json")
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		const newRepo = "example-owner/new-repo"
		gh := &fakeGitHub{
			invitations: []github.Invitation{{ID: 41, Repository: github.Repository{
				FullName: newRepo, ID: 3, Owner: github.User{Login: "example-owner", ID: 2},
			}, Permissions: "write"}},
		}
		var invitationChecks atomic.Int32
		gh.beforeList = func() { invitationChecks.Add(1) }
		handler := &heldCapacityHandler{entered: make(chan struct{}), release: make(chan struct{})}
		m := &Monitor{Config: cfg, ConfigPath: configPath, GitHub: gh, Handler: handler,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(t.Context(), store); result <- err }()
		<-handler.entered
		if invitationChecks.Load() != 0 {
			t.Error("invitation was checked before due time")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if len(gh.notificationCalls) != 0 {
			t.Error("action discovery ran during idle invitation check")
		}
		if handler.waits.Load() != 1 || handler.cancels.Load() != 0 {
			t.Errorf("invitation check restarted or canceled readiness: waits=%d cancels=%d", handler.waits.Load(), handler.cancels.Load())
		}
		if !slices.Equal(gh.accepted, []int64{41}) || !store.Data.NextInviteCheckAt.After(time.Now()) ||
			!store.Data.RepositoryCursors[newRepo].Equal(initial) || len(m.Config.Repositories) != 1 {
			t.Errorf("due invitation lost its deferred cursor: accepted=%v next=%s cursors=%v repositories=%v",
				gh.accepted, store.Data.NextInviteCheckAt, store.Data.RepositoryCursors, m.Config.Repositories)
		}
		close(handler.release)
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened := openStateAt(t, statePath, time.Time{})
		if !reopened.Data.RepositoryCursors[newRepo].Equal(initial) {
			t.Fatalf("invited repository cursor was not durable: %v", reopened.Data.RepositoryCursors)
		}
	})
}

func TestRunOnceChecksInvitationDueWhenCapacityArrives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		at := time.Now().UTC()
		store := openState(t, at.Add(-time.Minute))
		store.Data.NextInviteCheckAt = at.Add(time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &fakeGitHub{invitations: []github.Invitation{{ID: 41, Repository: github.Repository{
			FullName: "example-owner/example-repo", ID: 1, Owner: github.User{Login: "example-owner", ID: 2},
		}, Permissions: "write"}}}
		handler := &heldCapacityHandler{entered: make(chan struct{}), release: make(chan struct{})}
		clock := at
		m := &Monitor{Config: testConfig(t), GitHub: gh, Handler: handler,
			SelectedExecutorID: "local", Now: func() time.Time { return clock }}
		result := make(chan error, 1)
		go func() { _, err := m.RunOnce(t.Context(), store); result <- err }()
		synctest.Wait()
		clock = at.Add(2 * time.Minute)
		close(handler.release)
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(gh.accepted, []int64{41}) {
			t.Fatalf("due invitation lost when readiness arrived: %v", gh.accepted)
		}
		if handler.waits.Load() != 1 || handler.cancels.Load() != 0 {
			t.Fatalf("readiness was restarted or canceled: waits=%d cancels=%d", handler.waits.Load(), handler.cancels.Load())
		}
	})
}

func TestRunOnceTerminalAcceptedReplyDoesNotWaitForCapacity(t *testing.T) {
	at := time.Now().UTC().Add(-10 * time.Minute)
	accepted := selectedAcceptance(t, at)
	accepted.Assigned = true
	store := openState(t, at.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{permission: github.Permission{Permission: "write"},
		currentItem: func(github.Item) (github.Item, error) { return selectedItem(at), nil }}
	handler := &capacityHandler{failAt: 1, err: errors.New("worker unavailable")}
	m := selectedMonitor(t, at, gh, handler)
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatalf("terminal reply during idle: %v", err)
	}
	if handler.waits != 0 || len(handler.jobs) != 0 || len(gh.comments) != 1 ||
		store.Data.Accepted != nil || len(store.Data.Processed) != 1 || len(gh.notificationCalls) != 0 {
		t.Fatalf("terminal reply depended on worker capacity or scanned new work: waits=%d jobs=%v comments=%v state=%+v",
			handler.waits, handler.jobs, gh.comments, store.Data)
	}
}

func TestRunOnceWakesForInvitationsBeforePollOrAcceptedRetry(t *testing.T) {
	now := time.Now().UTC()
	t.Run("long poll", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.PollIntervalSeconds = 30 * 60
		store := openState(t, now.Add(-time.Minute))
		m := &Monitor{Config: cfg, GitHub: &fakeGitHub{}, Handler: &capacityHandler{}, Now: func() time.Time { return now }}
		next, err := m.RunOnce(t.Context(), store)
		if err != nil || !next.Equal(now.Add(15*time.Minute)) || !store.Data.NextPollAt.Equal(now.Add(30*time.Minute)) {
			t.Fatalf("long poll missed invitation wake: next=%s poll=%s err=%v", next, store.Data.NextPollAt, err)
		}
	})
	t.Run("accepted backoff", func(t *testing.T) {
		accepted := selectedAcceptance(t, now)
		accepted.RetryAt = now.Add(30 * time.Minute)
		store := openState(t, now.Add(-time.Minute))
		store.Data.Accepted = &accepted
		store.Data.NextInviteCheckAt = now.Add(-time.Minute)
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		gh := &fakeGitHub{invitations: []github.Invitation{{ID: 41, Repository: github.Repository{
			FullName: "example-owner/example-repo", ID: 1, Owner: github.User{Login: "example-owner", ID: 2},
		}, Permissions: "write"}}}
		m := selectedMonitor(t, now, gh, selectedHandler(func(Job) error {
			t.Fatal("accepted backoff dispatched work")
			return nil
		}))
		next, err := m.RunOnce(t.Context(), store)
		if err != nil || !next.Equal(now.Add(15*time.Minute)) || !slices.Equal(gh.accepted, []int64{41}) ||
			len(gh.notificationCalls) != 0 || store.Data.Accepted == nil || store.Data.Accepted.JobID != accepted.JobID {
			t.Fatalf("accepted backoff missed invitation or scanned new work: next=%s err=%v accepted=%v calls=%v state=%+v",
				next, err, gh.accepted, gh.notificationCalls, store.Data)
		}
	})
}

func TestRunOnceSelectedModeRequiresReadinessCheck(t *testing.T) {
	now := time.Now().UTC()
	store := openState(t, now.Add(-time.Minute))
	store.Data.NextInviteCheckAt = now.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}}
	m := selectedMonitor(t, now, gh, &fakeHandler{onExecute: func() {
		t.Fatal("selected action ran without a readiness check")
	}})
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, ErrAcceptanceState) ||
		len(gh.notificationCalls) != 0 || len(store.Data.Processed) != 0 || !store.Data.NextPollAt.IsZero() {
		t.Fatalf("selected mode discovered work without readiness: calls=%v state=%+v err=%v",
			gh.notificationCalls, store.Data, err)
	}
}

func TestRunOnceSelectedCapacityLossReplaysSiblingAfterRestart(t *testing.T) {
	now := time.Now().UTC()
	initial := now.Add(-time.Minute)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, initial)
	first := selectedItem(now)
	second := selectedItem(now.Add(time.Second))
	second.Key, second.NodeID = "comment:2", "node:2"
	second.HTMLURL = "https://github.com/example-owner/example-repo/pull/7#issuecomment-2"
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, items: []github.Item{first, second},
		permission: github.Permission{Permission: "write"}}
	unavailable := errors.New("worker capacity lost")
	handler := &capacityHandler{failAt: 2, err: unavailable}
	clock := now
	m := selectedMonitor(t, now, gh, handler)
	m.Now = func() time.Time { return clock }
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, unavailable) {
		t.Fatalf("selected capacity loss = %v", err)
	}
	if len(handler.jobs) != 1 || handler.jobs[0].Source.Key != first.Key || len(store.Data.Processed) != 1 ||
		store.Data.Accepted != nil || len(store.Data.PendingNotifications) != 1 ||
		!store.Data.Cursor.Equal(initial) || !store.Data.RepositoryCursors["example-owner/example-repo"].Equal(initial) {
		t.Fatalf("selected capacity loss consumed sibling or advanced cursor: jobs=%v state=%+v", handler.jobs, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStateAt(t, path, initial)
	gh.notifications = nil
	handler.failAt = 0
	clock = now.Add(time.Minute)
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatalf("selected sibling replay after restart: %v", err)
	}
	if len(handler.jobs) != 2 || handler.jobs[1].Source.Key != second.Key || len(store.Data.Processed) != 2 ||
		len(store.Data.PendingNotifications) != 0 {
		t.Fatalf("selected sibling was lost or first was duplicated: jobs=%v state=%+v", handler.jobs, store.Data)
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
	deferred, err := m.acceptWhitelistedInvitations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Config.Repositories) != 1 || len(gh.accepted) != 1 || !slices.Equal(deferred, []string{"example-owner/new-repo"}) {
		t.Fatalf("new repository became active before restart: repos=%v accepted=%v deferred=%v", m.Config.Repositories, gh.accepted, deferred)
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

func TestSelectedWorkerInvitationKeepsCursorUntilRestart(t *testing.T) {
	cfg := testConfig(t)
	cfg.InvitationOwners = []string{"example-owner"}
	cfg.InvitationOwnerIDs = map[string]int64{"example-owner": 2}
	configPath := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	const newRepo = "example-owner/new-repo"
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	initial := now.Add(-time.Minute)
	requestAt := now.Add(-10 * time.Second)
	notification := testNotification(requestAt)
	notification.Repository.FullName = newRepo
	notification.Subject.URL = "https://api.github.com/repos/" + newRepo + "/pulls/7"
	gh := &fakeGitHub{
		invitations: []github.Invitation{{ID: 1, Repository: github.Repository{FullName: newRepo,
			Owner: github.User{Login: "example-owner"}}, Permissions: "write"}},
		notifications: []github.Notification{notification},
		items: []github.Item{{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review",
			Author: github.User{Login: "writer"}, UpdatedAt: requestAt}},
		permission: github.Permission{Permission: "write"},
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, statePath, initial)
	handler := &capacityHandler{}
	m := &Monitor{Config: cfg, ConfigPath: configPath, GitHub: gh, Handler: handler,
		SelectedCommands: map[string]bool{"review": true}, Now: func() time.Time { return now }}
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if len(m.Config.Repositories) != 1 || len(handler.jobs) != 0 || len(gh.accepted) != 1 ||
		!store.Data.RepositoryCursors[newRepo].Equal(initial) || !store.Data.Cursor.After(requestAt) {
		t.Fatalf("deferred invitation cursor lost: repositories=%v jobs=%v accepted=%v cursors=%v account=%v",
			m.Config.Repositories, handler.jobs, gh.accepted, store.Data.RepositoryCursors, store.Data.Cursor)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStateAt(t, statePath, initial)
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	gh.invitations = nil
	gh.notificationCalls = nil
	m = &Monitor{Config: reloaded, ConfigPath: configPath, GitHub: gh, Handler: handler,
		SelectedCommands: map[string]bool{"review": true}, Now: func() time.Time { return now.Add(time.Minute) }}
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	var sawNewRepo bool
	for _, call := range gh.notificationCalls {
		if call.repo == newRepo {
			sawNewRepo = true
			if call.since.After(requestAt) {
				t.Fatalf("restart skipped new repository notification: since=%s request=%s", call.since, requestAt)
			}
		}
	}
	if !sawNewRepo || len(handler.jobs) != 1 || handler.jobs[0].Repository.Name != newRepo {
		t.Fatalf("restart did not discover deferred command: calls=%v jobs=%v", gh.notificationCalls, handler.jobs)
	}
}
