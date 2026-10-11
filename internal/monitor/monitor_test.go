package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type fakeGitHub struct {
	notifications     []github.Notification
	items             []github.Item
	invitations       []github.Invitation
	inviteErr         error
	beforeList        func()
	permission        github.Permission
	permissionErr     error
	reactionErr       error
	acceptErr         error
	beforeAccept      func()
	accepted          []int64
	declined          []int64
	notificationCalls int
	reactions         []string
	checks            int
	currentChecks     int
	currentItem       func(github.Item) (github.Item, error)
	commentErr        error
	commentCalls      int
	comments          []github.Comment
	threadItems       func(string, int, time.Time) ([]github.Item, error)
	threadCalls       int
	rawIDs            bool
	liveRepo          *github.Repository
}

func fixtureRepository(repo github.Repository) github.Repository {
	if repo.ID == 0 {
		repo.ID = 1
	}
	if repo.Owner.ID == 0 {
		owner := repo.Owner.Login
		if owner == "" {
			owner, _, _ = strings.Cut(repo.FullName, "/")
		}
		switch owner {
		case "another-owner":
			repo.Owner.ID = 3
		case "other":
			repo.Owner.ID = 4
		default:
			repo.Owner.ID = 2
		}
	}
	return repo
}

func (f *fakeGitHub) ListNotifications(context.Context, time.Time) ([]github.Notification, time.Duration, error) {
	f.notificationCalls++
	notifications := append([]github.Notification(nil), f.notifications...)
	if !f.rawIDs {
		for i := range notifications {
			notifications[i].Repository = fixtureRepository(notifications[i].Repository)
		}
	}
	return notifications, time.Minute, nil
}

func (f *fakeGitHub) Repository(_ context.Context, name string) (github.Repository, error) {
	if f.liveRepo != nil {
		return *f.liveRepo, nil
	}
	return fixtureRepository(github.Repository{FullName: name}), nil
}

func (f *fakeGitHub) ListRepositoryInvitations(context.Context) ([]github.Invitation, error) {
	if f.beforeList != nil {
		f.beforeList()
	}
	invitations := append([]github.Invitation(nil), f.invitations...)
	if !f.rawIDs {
		for i := range invitations {
			invitations[i].Repository = fixtureRepository(invitations[i].Repository)
		}
	}
	return invitations, f.inviteErr
}

func (f *fakeGitHub) AcceptRepositoryInvitation(_ context.Context, id int64) error {
	if f.beforeAccept != nil {
		f.beforeAccept()
	}
	f.accepted = append(f.accepted, id)
	return f.acceptErr
}

func (f *fakeGitHub) DeclineRepositoryInvitation(_ context.Context, id int64) error {
	f.declined = append(f.declined, id)
	return nil
}

func (f *fakeGitHub) ThreadItems(_ context.Context, repo, _ string, number int, since time.Time) ([]github.Item, error) {
	f.threadCalls++
	var items []github.Item
	if f.threadItems != nil {
		var err error
		items, err = f.threadItems(repo, number, since)
		if err != nil {
			return nil, err
		}
	} else {
		items = f.items
	}
	items = append([]github.Item(nil), items...)
	if !f.rawIDs {
		for i := range items {
			if items[i].Author.ID == 0 {
				items[i].Author.ID = 7
			}
		}
	}
	return items, nil
}

func (f *fakeGitHub) CurrentItem(_ context.Context, item github.Item) (github.Item, error) {
	f.currentChecks++
	if f.currentItem != nil {
		return f.currentItem(item)
	}
	return item, nil
}

func (f *fakeGitHub) Permission(_ context.Context, _, login string) (github.Permission, error) {
	f.checks++
	permission := f.permission
	if !f.rawIDs {
		if permission.User.ID == 0 {
			permission.User.ID = 7
		}
		if permission.User.Login == "" {
			permission.User.Login = login
		}
	}
	return permission, f.permissionErr
}

func (f *fakeGitHub) ReactEyes(_ context.Context, nodeID string) error {
	f.reactions = append(f.reactions, nodeID)
	return f.reactionErr
}

func (f *fakeGitHub) CreateIssueComment(ctx context.Context, _ string, _ int, body string) (github.Comment, error) {
	f.commentCalls++
	if err := ctx.Err(); err != nil {
		return github.Comment{}, err
	}
	if f.commentErr != nil {
		return github.Comment{}, f.commentErr
	}
	comment := github.Comment{Body: body, User: github.User{Login: "linkdataminion"}}
	f.comments = append(f.comments, comment)
	f.items = append(f.items, github.Item{Body: body, Author: comment.User})
	return comment, nil
}

type fakeExecutor struct {
	jobs       []Job
	onExecute  func()
	executeErr error
}

func (f *fakeExecutor) Execute(_ context.Context, job Job) error {
	if f.onExecute != nil {
		f.onExecute()
	}
	f.jobs = append(f.jobs, job)
	return f.executeErr
}

func testConfig() config.Config {
	return config.Config{
		BotLogin:            "linkdataminion",
		PollIntervalSeconds: 60,
		Repositories: []config.Repository{{
			Name: "example-owner/example-repo", ID: 1, OwnerID: 2, Actions: []string{"review", "fix", "release"},
		}},
	}
}

func testNotification(at time.Time) github.Notification {
	return github.Notification{
		ID: "thread-1", Repository: github.Repository{FullName: "example-owner/example-repo"},
		Subject:   github.Subject{Type: "PullRequest", URL: "https://api.github.com/repos/example-owner/example-repo/pulls/7"},
		UpdatedAt: at,
	}
}

func openState(t *testing.T, initial time.Time) *state.Store {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "state.json"), initial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRunOnceChecksPermissionAndDeduplicates(t *testing.T) {
	first := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(first)},
		items: []github.Item{{
			Key: "example-owner/example-repo#issue-comment:123", NodeID: "comment-node-123", Body: "@linkdataminion review", Author: github.User{Login: "writer"}, UpdatedAt: first,
		}},
		permission: github.Permission{Permission: "write"},
	}
	executor := &fakeExecutor{onExecute: func() {
		if len(gh.reactions) != 1 || gh.reactions[0] != "comment-node-123" {
			t.Fatalf("command executed before eyes reaction: %v", gh.reactions)
		}
	}}
	store := openState(t, first.Add(-time.Minute))
	clock := first
	m := &Monitor{Config: testConfig(), GitHub: gh, Executor: executor, Logger: log.New(io.Discard, "", 0), Now: func() time.Time { return clock }}
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(executor.jobs) != 1 || gh.checks != 1 || len(gh.reactions) != 1 {
		t.Fatalf("got %d jobs, %d permission checks, and %d reactions, want one each", len(executor.jobs), gh.checks, len(gh.reactions))
	}
	if len(store.Data.Processed) != 1 || !store.Data.Cursor.After(first.Add(-time.Minute)) {
		t.Fatalf("state not advanced after successful job: %+v", store.Data)
	}
	clock = clock.Add(time.Minute)
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(executor.jobs) != 1 || gh.checks != 1 || len(gh.reactions) != 1 || gh.currentChecks != 1 {
		t.Fatalf("duplicate command handled: %d jobs, %d permission checks, %d reactions, %d provenance checks", len(executor.jobs), gh.checks, len(gh.reactions), gh.currentChecks)
	}
}

func TestRunOnceBoundsFailedExecutions(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}, HTMLURL: "https://github.com/example-owner/example-repo/pull/7#issuecomment-1"}
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, items: []github.Item{item}, permission: github.Permission{Permission: "write"}}
	executor := &fakeExecutor{executeErr: errors.New("always fails")}
	store := openState(t, now.Add(-time.Minute))
	executor.onExecute = func() {
		if len(store.Data.JobFailures) != 1 {
			t.Fatalf("execution began before a durable attempt was recorded: %+v", store.Data.JobFailures)
		}
		for _, failure := range store.Data.JobFailures {
			if failure.Attempts != len(executor.jobs)+1 {
				t.Fatalf("execution attempt was not recorded first: %+v", failure)
			}
		}
	}
	clock := now
	m := &Monitor{Config: testConfig(), GitHub: gh, Executor: executor, Now: func() time.Time { return clock }}
	for attempt := 1; attempt <= maxJobAttempts; attempt++ {
		_, err := m.RunOnce(context.Background(), store)
		if (err != nil) != (attempt < maxJobAttempts) {
			t.Fatalf("attempt %d error = %v", attempt, err)
		}
		if len(executor.jobs) != attempt {
			t.Fatalf("attempt %d executed %d jobs", attempt, len(executor.jobs))
		}
		clock = clock.Add(time.Minute)
		for _, pending := range store.Data.PendingNotifications {
			clock = pending.RetryAt
		}
	}
	if len(gh.comments) != 1 || !strings.HasSuffix(gh.comments[0].Body, "\n\n"+executor.jobs[0].Marker()) {
		t.Fatalf("failure reply missing job marker: %+v", gh.comments)
	}
	if len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 || !store.Data.Cursor.After(now.Add(-time.Minute)) {
		t.Fatalf("job did not finish after retry limit: %+v", store.Data)
	}
	if _, err := m.RunOnce(context.Background(), store); err != nil || len(executor.jobs) != maxJobAttempts || len(gh.comments) != 1 {
		t.Fatalf("processed job reran: err=%v jobs=%d replies=%d", err, len(executor.jobs), len(gh.comments))
	}
}

func TestRunOnceDoesNotExhaustJobsDuringGitHubOutage(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}, HTMLURL: "https://github.com/example-owner/example-repo/pull/7#issuecomment-1"}
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, items: []github.Item{item}, permission: github.Permission{Permission: "write"}}
	executor := &fakeExecutor{executeErr: &github.StatusError{StatusCode: http.StatusTooManyRequests}}
	store := openState(t, now.Add(-time.Minute))
	clock := now
	m := &Monitor{Config: testConfig(), GitHub: gh, Executor: executor, Now: func() time.Time { return clock }}
	for attempt := 1; attempt <= maxJobAttempts+1; attempt++ {
		if _, err := m.RunOnce(context.Background(), store); err == nil {
			t.Fatal("GitHub outage was not surfaced")
		}
		clock = clock.Add(time.Minute)
		for _, pending := range store.Data.PendingNotifications {
			clock = pending.RetryAt
		}
	}
	if len(executor.jobs) != maxJobAttempts+1 || len(gh.comments) != 0 || len(store.Data.Processed) != 0 || store.Data.JobFailures[executor.jobs[0].ID].Attempts != 0 {
		t.Fatalf("temporary outage consumed retry limit: jobs=%d comments=%d state=%+v", len(executor.jobs), len(gh.comments), store.Data)
	}
}

func TestRunOnceTerminalFailureRepliesWithFreshContext(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}, HTMLURL: "https://github.com/example-owner/example-repo/pull/7#issuecomment-1"}
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, items: []github.Item{item}, permission: github.Permission{Permission: "write"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executor := &fakeExecutor{onExecute: cancel, executeErr: errors.Join(ErrTerminalJob, context.DeadlineExceeded)}
	store := openState(t, now.Add(-time.Minute))
	m := &Monitor{Config: testConfig(), GitHub: gh, Executor: executor, Now: func() time.Time { return now }}
	if _, err := m.RunOnce(ctx, store); err != nil {
		t.Fatal(err)
	}
	if len(executor.jobs) != 1 || len(gh.comments) != 1 || len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 {
		t.Fatalf("terminal job was not handled: jobs=%d comments=%d state=%+v", len(executor.jobs), len(gh.comments), store.Data)
	}
	// A reply may succeed just before saving completion fails. Its marker must
	// prevent a duplicate reply after restart.
	id := executor.jobs[0].ID
	delete(store.Data.Processed, id)
	store.Data.JobFailures[id] = state.JobFailure{Attempts: maxJobAttempts}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	m.Now = func() time.Time { return now.Add(time.Minute) }
	if _, err := m.RunOnce(context.Background(), store); err != nil || gh.commentCalls != 1 || len(executor.jobs) != 1 || len(store.Data.Processed) != 1 {
		t.Fatalf("failure reply duplicated after interrupted save: err=%v replies=%d jobs=%d state=%+v", err, gh.commentCalls, len(executor.jobs), store.Data)
	}
}

func TestRunOnceStopsAfterFailedReplyAttempts(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}, HTMLURL: "https://github.com/example-owner/example-repo/pull/7#issuecomment-1"}
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}, items: []github.Item{item}, permission: github.Permission{Permission: "write"}, commentErr: errors.New("comment service unavailable")}
	executor := &fakeExecutor{executeErr: ErrTerminalJob}
	store := openState(t, now.Add(-time.Minute))
	clock := now
	m := &Monitor{Config: testConfig(), GitHub: gh, Executor: executor, Now: func() time.Time { return clock }}
	for attempt := 1; attempt <= maxFailureReplyAttempts; attempt++ {
		_, err := m.RunOnce(context.Background(), store)
		if (err != nil) != (attempt < maxFailureReplyAttempts) {
			t.Fatalf("reply attempt %d error = %v", attempt, err)
		}
		if attempt < maxFailureReplyAttempts {
			failure := store.Data.JobFailures[executor.jobs[0].ID]
			if failure.Attempts != maxJobAttempts || failure.ReplyAttempts != attempt {
				t.Fatalf("failure disposition was not saved: %+v", failure)
			}
		}
		clock = clock.Add(time.Minute)
		for _, pending := range store.Data.PendingNotifications {
			clock = pending.RetryAt
		}
	}
	if len(executor.jobs) != 1 || gh.commentCalls != maxFailureReplyAttempts || len(store.Data.Processed) != 1 || !store.Data.Cursor.After(now.Add(-time.Minute)) {
		t.Fatalf("failed reply pinned job: jobs=%d replies=%d state=%+v", len(executor.jobs), gh.commentCalls, store.Data)
	}
}

func TestRunOnceIgnoresQuotedOutsiderCommand(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(now)},
		items: []github.Item{{
			Key: "comment:1", NodeID: "comment-node-1",
			Body:   "> An outsider wrote:\n@linkdataminion fix planted",
			Author: github.User{Login: "writer"}, UpdatedAt: now,
		}},
		permission: github.Permission{Permission: "write"},
	}
	executor := &fakeExecutor{}
	store := openState(t, now.Add(-time.Minute))
	m := &Monitor{Config: testConfig(), GitHub: gh, Executor: executor, Logger: log.New(io.Discard, "", 0), Now: func() time.Time { return now }}
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(executor.jobs) != 0 || gh.checks != 0 || len(gh.reactions) != 0 {
		t.Fatalf("quoted outsider command was handled: jobs=%d checks=%d reactions=%d", len(executor.jobs), gh.checks, len(gh.reactions))
	}
}

func TestRunOnceRetriesFailedReaction(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(now)},
		items: []github.Item{{
			Key: "example-owner/example-repo#issue-comment:123", NodeID: "comment-node-123", Body: "@linkdataminion review", Author: github.User{Login: "writer"}, UpdatedAt: now,
		}},
		permission:  github.Permission{Permission: "write"},
		reactionErr: errors.New("HTTP 503"),
	}
	executor := &fakeExecutor{}
	store := openState(t, now.Add(-time.Minute))
	initial := store.Data.Cursor
	clock := now
	m := &Monitor{Config: testConfig(), GitHub: gh, Executor: executor, Logger: log.New(io.Discard, "", 0), Now: func() time.Time { return clock }}
	if _, err := m.RunOnce(context.Background(), store); err == nil {
		t.Fatal("expected reaction failure")
	}
	if !store.Data.Cursor.After(initial) || len(store.Data.PendingNotifications) != 1 || len(store.Data.Processed) != 0 || len(executor.jobs) != 0 {
		t.Fatalf("command was lost or executed without acknowledgement: %+v", store.Data)
	}
	gh.reactionErr = nil
	clock = clock.Add(time.Minute)
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(gh.reactions) != 2 || len(executor.jobs) != 1 || len(store.Data.PendingNotifications) != 0 {
		t.Fatalf("reaction retry: %d reactions, %d jobs", len(gh.reactions), len(executor.jobs))
	}
}

func TestRunOncePersistsPermissionFailureWithoutPinningCursor(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(now)},
		items:         []github.Item{{Key: "comment:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}, UpdatedAt: now}},
		permissionErr: errors.New("HTTP 403"),
	}
	executor := &fakeExecutor{}
	store := openState(t, now.Add(-time.Minute))
	initial := store.Data.Cursor
	m := &Monitor{Config: testConfig(), GitHub: gh, Executor: executor, Logger: log.New(io.Discard, "", 0), Now: func() time.Time { return now }}
	if _, err := m.RunOnce(context.Background(), store); err == nil {
		t.Fatal("expected permission failure")
	}
	if !store.Data.Cursor.After(initial) || len(store.Data.PendingNotifications) != 1 || len(store.Data.Processed) != 0 || len(executor.jobs) != 0 {
		t.Fatalf("command was lost or executed after permission failure: %+v", store.Data)
	}
}

func TestRunOnceRetriesFailedThreadAfterCursorAdvances(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	initial := now.Add(-time.Minute)
	bad := testNotification(now)
	good := testNotification(now)
	good.ID = "thread-2"
	good.Repository.FullName = "other/repo"
	good.Subject.URL = "https://api.github.com/repos/other/repo/pulls/8"
	badItem := github.Item{Key: "bad-comment", NodeID: "bad-node", Body: "@linkdataminion review", Author: github.User{Login: "writer"}, HTMLURL: "https://github.com/example-owner/example-repo/pull/7#issuecomment-1"}
	goodItem := github.Item{Key: "good-comment", NodeID: "good-node", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}
	var badSince []time.Time
	gh := &fakeGitHub{
		notifications: []github.Notification{bad, good},
		permission:    github.Permission{Permission: "write"},
		threadItems: func(_ string, number int, since time.Time) ([]github.Item, error) {
			if number == 7 {
				badSince = append(badSince, since)
				return []github.Item{badItem}, nil
			}
			return []github.Item{goodItem}, nil
		},
		currentItem: func(item github.Item) (github.Item, error) {
			if item.Key == badItem.Key {
				return github.Item{}, errors.New("provenance unavailable")
			}
			return item, nil
		},
	}
	cfg := testConfig()
	cfg.Repositories = append(cfg.Repositories, config.Repository{Name: "other/repo", ID: 1, OwnerID: 4, Actions: []string{"review"}})
	var logs bytes.Buffer
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(path, initial)
	if err != nil {
		t.Fatal(err)
	}
	clock := now
	m := &Monitor{Config: cfg, GitHub: gh, Executor: &fakeExecutor{}, Logger: log.New(&logs, "", 0), Now: func() time.Time { return clock }}
	if _, err := m.RunOnce(context.Background(), store); err == nil || !strings.Contains(err.Error(), badItem.HTMLURL) {
		t.Fatalf("failure did not identify item URL: %v", err)
	}
	if !store.Data.Cursor.After(initial) || len(store.Data.PendingNotifications) != 1 || len(store.Data.Processed) != 1 {
		t.Fatalf("unrelated command or cursor blocked: %+v", store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(path, initial)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gh.notifications = nil
	clock = clock.Add(time.Minute)
	if _, err := m.RunOnce(context.Background(), store); err == nil {
		t.Fatal("pending failure was not retried after restart")
	}
	if len(badSince) != 2 || !badSince[0].Equal(initial) || !badSince[1].Equal(initial) || len(store.Data.PendingNotifications) != 1 {
		t.Fatalf("lost original thread scan window: %v, %+v", badSince, store.Data.PendingNotifications)
	}
	gh.currentItem = nil
	clock = clock.Add(time.Minute)
	if _, err := m.RunOnce(context.Background(), store); err != nil || len(badSince) != 2 {
		t.Fatalf("pending thread retried before backoff elapsed: %v, %v", err, badSince)
	}
	clock = clock.Add(time.Minute)
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(store.Data.PendingNotifications) != 0 || len(store.Data.Processed) != 2 || len(badSince) != 3 {
		t.Fatalf("valid pending command was lost: state=%+v, scans=%v", store.Data, badSince)
	}
}

func TestRunOnceRetriesThreadFetchWithoutNewNotification(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	initial := now.Add(-time.Minute)
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)}}
	var seen []time.Time
	gh.threadItems = func(_ string, _ int, since time.Time) ([]github.Item, error) {
		seen = append(seen, since)
		if len(seen) == 1 {
			return nil, errors.New("thread unavailable")
		}
		return nil, nil
	}
	store := openState(t, initial)
	clock := now
	m := &Monitor{Config: testConfig(), GitHub: gh, Executor: &fakeExecutor{}, Now: func() time.Time { return clock }}
	if _, err := m.RunOnce(context.Background(), store); err == nil || !store.Data.Cursor.After(initial) {
		t.Fatalf("failure did not advance global cursor: %v, %+v", err, store.Data)
	}
	gh.notifications = nil
	clock = clock.Add(time.Minute)
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || !seen[1].Equal(initial) || len(store.Data.PendingNotifications) != 0 {
		t.Fatalf("thread was not durably retried from original cursor: %v, %+v", seen, store.Data.PendingNotifications)
	}
}

func TestAllowedActorsCannotBypassWritePermission(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(now)},
		items:         []github.Item{{Key: "comment:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}, UpdatedAt: now}},
		permission:    github.Permission{Permission: "read"},
	}
	config := testConfig()
	config.Repositories[0].AllowedActors = []string{"writer"}
	config.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
	executor := &fakeExecutor{}
	store := openState(t, now.Add(-time.Minute))
	m := &Monitor{Config: config, GitHub: gh, Executor: executor, Logger: log.New(io.Discard, "", 0), Now: func() time.Time { return now }}
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if gh.checks != 1 || len(executor.jobs) != 0 || len(gh.reactions) != 0 {
		t.Fatalf("allowlisted reader ran command: checks=%d jobs=%d reactions=%d", gh.checks, len(executor.jobs), len(gh.reactions))
	}
}

func TestEditedCommentCannotUseOriginalAuthorAuthorization(t *testing.T) {
	first := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{
		notifications: []github.Notification{testNotification(first)},
		items: []github.Item{{
			Key: "comment:1", NodeID: "comment-node-1", Body: "No command yet", Author: github.User{Login: "allowed"}, UpdatedAt: first,
		}},
		permission: github.Permission{Permission: "write"},
	}
	executor := &fakeExecutor{}
	config := testConfig()
	config.Repositories[0].AllowedActors = []string{"allowed"}
	config.Repositories[0].AllowedActorIDs = map[string]int64{"allowed": 7}
	store := openState(t, first.Add(-time.Minute))
	clock := first
	m := &Monitor{Config: config, GitHub: gh, Executor: executor, Logger: log.New(io.Discard, "", 0), Now: func() time.Time { return clock }}
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(executor.jobs) != 0 {
		t.Fatal("plain comment started a job")
	}

	clock = clock.Add(time.Minute)
	gh.notifications = []github.Notification{testNotification(clock)}
	gh.items = []github.Item{
		{Key: "comment:1", NodeID: "comment-node-1", Body: "@linkdataminion review", Author: github.User{Login: "allowed"}, UpdatedAt: clock},
		{Key: "comment:2", NodeID: "comment-node-2", Body: "@linkdataminion review", Author: github.User{Login: "allowed"}, UpdatedAt: clock},
	}
	gh.currentItem = func(item github.Item) (github.Item, error) {
		if item.NodeID == "comment-node-1" {
			item.Edited = true
			item.Editor = github.User{Login: "excluded"}
		}
		return item, nil
	}
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(executor.jobs) != 1 || executor.jobs[0].Source.NodeID != "comment-node-2" || gh.checks != 1 || len(gh.reactions) != 1 {
		t.Fatalf("edited command ran or original command failed: jobs=%+v checks=%d reactions=%v", executor.jobs, gh.checks, gh.reactions)
	}
}

func TestAcceptOnlyWhitelistedInvitations(t *testing.T) {
	gh := &fakeGitHub{invitations: []github.Invitation{
		{ID: 2, Repository: github.Repository{FullName: "other/project", Owner: github.User{Login: "other"}}, Invitee: github.User{Login: "linkdataminion"}, Inviter: github.User{Login: "example-owner"}, Permissions: "admin"},
		{ID: 1, Repository: github.Repository{FullName: "example-owner/example-repo", Owner: github.User{Login: "example-owner"}}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "write"},
		{ID: 3, Repository: github.Repository{FullName: "example-owner/example-repo", Owner: github.User{Login: "example-owner"}}, Invitee: github.User{Login: "someone-else"}, Permissions: "write"},
		{ID: 4, Repository: github.Repository{FullName: "example-owner/example-repo", Owner: github.User{Login: "example-owner"}}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "write", Expired: true},
		{ID: 5, Repository: github.Repository{FullName: "example-owner/private-repo", Owner: github.User{Login: "example-owner"}}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "read"},
		{ID: 6, Repository: github.Repository{FullName: "another-owner/service", Owner: github.User{Login: "another-owner"}}, Invitee: github.User{Login: "linkdataminion"}, Inviter: github.User{Login: "someone-else"}, Permissions: "triage"},
		{ID: 7, Repository: github.Repository{FullName: "example-owner/mismatch", Owner: github.User{Login: "other"}}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "write"},
		{ID: 8, Repository: github.Repository{FullName: "other/mismatch", Owner: github.User{Login: "example-owner"}}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "write"},
		{ID: 9, Repository: github.Repository{FullName: "example-owner/ownerless"}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "write"},
		{ID: 10, Repository: github.Repository{FullName: "example-owner/reclaimed", Owner: github.User{Login: "example-owner", ID: 99}}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "write"},
	}}
	gh.beforeAccept = func() {
		if len(gh.declined) != 0 {
			t.Fatal("outsider invitation declined before allowed invitations were accepted")
		}
	}
	config := testConfig()
	config.InvitationOwners = []string{"example-owner", "another-owner"}
	config.InvitationOwnerIDs = map[string]int64{"example-owner": 2, "another-owner": 3}
	m := &Monitor{Config: config, GitHub: gh}
	if err := m.acceptWhitelistedInvitations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gh.accepted) != 3 || gh.accepted[0] != 1 || gh.accepted[1] != 5 || gh.accepted[2] != 6 {
		t.Fatalf("accepted invitations %v, want 1, 5, 6", gh.accepted)
	}
	if len(gh.declined) != 1 || gh.declined[0] != 2 {
		t.Fatalf("declined invitations %v, want outsider 2", gh.declined)
	}
}

func TestInvitationListingErrorKeepsPartialWorkAndPollsAfterLongCheck(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{
		invitations: []github.Invitation{
			{ID: 1, Repository: github.Repository{FullName: "example-owner/example-repo", Owner: github.User{Login: "example-owner"}}, Permissions: "write"},
			{ID: 2, Repository: github.Repository{FullName: "outsider/repo", Owner: github.User{Login: "outsider"}}, Permissions: "write"},
		},
		inviteErr:  errors.New("later page failed"),
		beforeList: func() { now = now.Add(time.Hour) },
	}
	store := openState(t, now.Add(-time.Minute))
	m := &Monitor{Config: testConfig(), GitHub: gh, Now: func() time.Time { return now }}
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(gh.accepted) != 1 || gh.accepted[0] != 1 || len(gh.declined) != 1 || gh.declined[0] != 2 {
		t.Fatalf("partial invitations not handled: accepted=%v declined=%v", gh.accepted, gh.declined)
	}
	if gh.notificationCalls != 1 || !store.Data.NextInviteCheckAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("notification calls=%d, next invitation check=%v, now=%v", gh.notificationCalls, store.Data.NextInviteCheckAt, now)
	}
}

func TestMonitoredRepositoryStillRequiresWriteInvitation(t *testing.T) {
	gh := &fakeGitHub{invitations: []github.Invitation{
		{ID: 1, Repository: github.Repository{FullName: "example-owner/example-repo", Owner: github.User{Login: "example-owner"}}, Permissions: "read"},
		{ID: 2, Repository: github.Repository{FullName: "example-owner/other", Owner: github.User{Login: "example-owner"}}, Permissions: "read"},
		{ID: 3, Repository: github.Repository{FullName: "another-owner/unknown-role", Owner: github.User{Login: "another-owner"}}, Permissions: "superadmin"},
	}}
	config := testConfig()
	config.InvitationOwners = []string{"example-owner", "another-owner"}
	config.InvitationOwnerIDs = map[string]int64{"example-owner": 2, "another-owner": 3}
	m := &Monitor{Config: config, GitHub: gh}
	if err := m.acceptWhitelistedInvitations(context.Background()); err == nil {
		t.Fatal("read-only invitation to monitored repository should fail")
	}
	if len(gh.accepted) != 1 || gh.accepted[0] != 2 {
		t.Fatalf("accepted invitations %v, want only owner-only repository 2", gh.accepted)
	}
}

func TestInvitationOwnerDoesNotAuthorizeCommands(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{
		notifications: []github.Notification{{
			Repository: github.Repository{FullName: "example-owner/other"},
			Subject:    github.Subject{Type: "PullRequest", URL: "https://api.github.com/repos/example-owner/other/pulls/7"},
			UpdatedAt:  now,
		}},
		items: []github.Item{{Key: "comment:1", Body: "@linkdataminion review", Author: github.User{Login: "writer"}}},
	}
	config := testConfig()
	config.InvitationOwners = []string{"example-owner", "another-owner"}
	config.InvitationOwnerIDs = map[string]int64{"example-owner": 2, "another-owner": 3}
	executor := &fakeExecutor{}
	store := openState(t, now.Add(-time.Minute))
	m := &Monitor{Config: config, GitHub: gh, Executor: executor, Now: func() time.Time { return now }}
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(executor.jobs) != 0 || gh.checks != 0 {
		t.Fatalf("owner-only repository triggered command: jobs=%d permission checks=%d", len(executor.jobs), gh.checks)
	}
}

func TestInvitationRegistersRepositoryBeforeAcceptanceAndMonitorsImmediately(t *testing.T) {
	for _, role := range []string{"write", "maintain", "admin"} {
		t.Run(role, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
			cfg := testConfig()
			cfg.InvitationOwners = []string{"example-owner"}
			cfg.InvitationOwnerIDs = map[string]int64{"example-owner": 2}
			skillDir := filepath.Join(t.TempDir(), "security-audit")
			if err := os.Mkdir(skillDir, 0700); err != nil {
				t.Fatal(err)
			}
			skill := filepath.Join(skillDir, "SKILL.md")
			if err := os.WriteFile(skill, []byte("Audit source."), 0600); err != nil {
				t.Fatal(err)
			}
			cfg.Codex.Skills = []string{skill}
			path := filepath.Join(t.TempDir(), "config.json")
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			gh := &fakeGitHub{
				invitations: []github.Invitation{{ID: 1, Repository: github.Repository{FullName: "example-owner/second-repo", Owner: github.User{Login: "example-owner"}}, Permissions: role}},
				notifications: []github.Notification{{
					Repository: github.Repository{FullName: "example-owner/second-repo"},
					Subject:    github.Subject{Type: "PullRequest", URL: "https://api.github.com/repos/example-owner/second-repo/pulls/7"},
					UpdatedAt:  now,
				}},
				items:      []github.Item{{Key: "comment:1", NodeID: "node-1", Body: "@linkdataminion audit", Author: github.User{Login: "writer"}}},
				permission: github.Permission{Permission: "write"},
				beforeAccept: func() {
					saved, err := config.Load(path)
					if err != nil {
						t.Fatal(err)
					}
					repo, ok := saved.Repository("example-owner/second-repo")
					if !ok || !repo.AllowsAction("review") || !repo.AllowsAction("audit") || !repo.AllowsAction("fix") || !repo.AllowsAction("release") {
						t.Fatalf("repository not persisted before accepting invitation: %+v", saved.Repositories)
					}
				},
			}
			executor := &fakeExecutor{}
			m := &Monitor{Config: cfg, ConfigPath: path, GitHub: gh, Executor: executor, Now: func() time.Time { return now }}
			if _, err := m.RunOnce(context.Background(), openState(t, now.Add(-time.Minute))); err != nil {
				t.Fatal(err)
			}
			if len(gh.accepted) != 1 || len(executor.jobs) != 1 || len(m.Config.Repositories) != 2 {
				t.Fatalf("invitation not registered and handled immediately: accepted=%v jobs=%d config=%+v", gh.accepted, len(executor.jobs), m.Config.Repositories)
			}
		})
	}
}

func TestInvitationRegistrationRetriesWithoutLosingOrDuplicatingRepository(t *testing.T) {
	cfg := testConfig()
	cfg.InvitationOwners = []string{"example-owner"}
	cfg.InvitationOwnerIDs = map[string]int64{"example-owner": 2}
	path := filepath.Join(t.TempDir(), "config.json")
	gh := &fakeGitHub{invitations: []github.Invitation{{ID: 1, Repository: github.Repository{FullName: "example-owner/second-repo", Owner: github.User{Login: "example-owner"}}, Permissions: "write"}}}
	m := &Monitor{Config: cfg, ConfigPath: path, GitHub: gh}
	if err := m.acceptWhitelistedInvitations(context.Background()); err == nil || len(gh.accepted) != 0 || len(m.Config.Repositories) != 1 {
		t.Fatalf("accepted invitation despite config write failure: %v, %v", err, gh.accepted)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	gh.acceptErr = errors.New("HTTP 503")
	if err := m.acceptWhitelistedInvitations(context.Background()); err == nil {
		t.Fatal("expected acceptance failure")
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Repositories) != 2 {
		t.Fatalf("lost registration on acceptance failure: %+v", saved.Repositories)
	}
	m.Config = saved
	gh.acceptErr = nil
	if err := m.acceptWhitelistedInvitations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gh.accepted) != 2 || len(m.Config.Repositories) != 2 {
		t.Fatalf("retry failed or duplicated repository: accepted=%v config=%+v", gh.accepted, m.Config.Repositories)
	}
}

func TestSubjectNumberVerifiesRepository(t *testing.T) {
	number, err := subjectNumber("https://api.github.com/repos/example-owner/example-repo/pulls/17", "example-owner/example-repo", "PullRequest")
	if err != nil || number != 17 {
		t.Fatalf("number=%d, err=%v", number, err)
	}
	if _, err := subjectNumber("https://api.github.com/repos/other/project/pulls/17", "example-owner/example-repo", "PullRequest"); err == nil {
		t.Fatal("accepted URL for other repository")
	}
}

func TestReclaimedRepositoryCannotTriggerCommandOrRetry(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		prepare func(*fakeGitHub, *state.Store)
	}{
		{"notification repository ID", func(gh *fakeGitHub, _ *state.Store) { gh.notifications[0].Repository.ID = 9 }},
		{"notification owner ID", func(gh *fakeGitHub, _ *state.Store) { gh.notifications[0].Repository.Owner.ID = 9 }},
		{"live repository ID", func(gh *fakeGitHub, _ *state.Store) {
			gh.liveRepo = &github.Repository{ID: 9, Owner: github.User{ID: 2}}
		}},
		{"live owner ID", func(gh *fakeGitHub, _ *state.Store) {
			gh.liveRepo = &github.Repository{ID: 1, Owner: github.User{ID: 9}}
		}},
		{"legacy pending retry", func(gh *fakeGitHub, store *state.Store) {
			gh.notifications = nil
			notification := testNotification(now)
			store.Data.PendingNotifications[notificationKey(notification)] = state.PendingNotification{
				Repository: notification.Repository.FullName, SubjectType: notification.Subject.Type,
				SubjectURL: notification.Subject.URL, Since: now.Add(-time.Minute), UpdatedAt: now, RetryAt: now,
			}
		}},
		{"retry after live replacement", func(gh *fakeGitHub, store *state.Store) {
			gh.notifications = nil
			gh.liveRepo = &github.Repository{ID: 9, Owner: github.User{ID: 2}}
			notification := testNotification(now)
			store.Data.PendingNotifications[notificationKey(notification)] = state.PendingNotification{
				Repository: notification.Repository.FullName, RepositoryID: 1, OwnerID: 2,
				SubjectType: notification.Subject.Type, SubjectURL: notification.Subject.URL,
				Since: now.Add(-time.Minute), UpdatedAt: now, RetryAt: now,
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := &fakeGitHub{rawIDs: true, notifications: []github.Notification{testNotification(now)},
				items:      []github.Item{{Key: "comment:1", Body: "@linkdataminion review", Author: github.User{ID: 7, Login: "writer"}}},
				permission: github.Permission{Permission: "write", User: github.User{ID: 7, Login: "writer"}}}
			gh.notifications[0].Repository = fixtureRepository(gh.notifications[0].Repository)
			store := openState(t, now.Add(-time.Minute))
			tc.prepare(gh, store)
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			executor := &fakeExecutor{}
			m := &Monitor{Config: testConfig(), GitHub: gh, Executor: executor, Now: func() time.Time { return now }}
			if _, err := m.RunOnce(context.Background(), store); err != nil {
				t.Fatal(err)
			}
			if gh.threadCalls != 0 || gh.checks != 0 || len(gh.reactions) != 0 || len(executor.jobs) != 0 || len(store.Data.PendingNotifications) != 0 {
				t.Fatalf("untrusted repository reached command path: threads=%d checks=%d reactions=%v jobs=%v pending=%v", gh.threadCalls, gh.checks, gh.reactions, executor.jobs, store.Data.PendingNotifications)
			}
		})
	}
}

func TestReclaimedLoginCannotAuthorizeCommand(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		prepare func(*fakeGitHub, *config.Config)
	}{
		{"allowed actor ID", func(_ *fakeGitHub, cfg *config.Config) { cfg.Repositories[0].AllowedActorIDs["writer"] = 8 }},
		{"REST and GraphQL author IDs", func(gh *fakeGitHub, _ *config.Config) {
			gh.currentItem = func(item github.Item) (github.Item, error) { item.Author.ID = 8; return item, nil }
		}},
		{"permission user ID", func(gh *fakeGitHub, _ *config.Config) { gh.permission.User.ID = 8 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := &fakeGitHub{notifications: []github.Notification{testNotification(now)},
				items:      []github.Item{{Key: "comment:1", Body: "@linkdataminion review", Author: github.User{ID: 7, Login: "writer"}}},
				permission: github.Permission{Permission: "write", User: github.User{ID: 7, Login: "writer"}}}
			cfg := testConfig()
			cfg.Repositories[0].AllowedActors = []string{"writer"}
			cfg.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
			tc.prepare(gh, &cfg)
			executor := &fakeExecutor{}
			m := &Monitor{Config: cfg, GitHub: gh, Executor: executor, Now: func() time.Time { return now }}
			if _, err := m.RunOnce(context.Background(), openState(t, now.Add(-time.Minute))); err != nil {
				t.Fatal(err)
			}
			if len(gh.reactions) != 0 || len(executor.jobs) != 0 {
				t.Fatalf("reclaimed login executed: reactions=%v jobs=%v", gh.reactions, executor.jobs)
			}
		})
	}
}

func TestReclaimedInvitationOwnerCannotRegisterOrAccept(t *testing.T) {
	cfg := testConfig()
	cfg.InvitationOwners = []string{"example-owner"}
	cfg.InvitationOwnerIDs = map[string]int64{"example-owner": 2}
	gh := &fakeGitHub{rawIDs: true, invitations: []github.Invitation{
		{ID: 1, Repository: github.Repository{ID: 9, FullName: "example-owner/example-repo", Owner: github.User{ID: 2, Login: "example-owner"}}, Permissions: "write"},
		{ID: 2, Repository: github.Repository{ID: 10, FullName: "example-owner/new-repo", Owner: github.User{ID: 99, Login: "example-owner"}}, Permissions: "write"},
	}}
	m := &Monitor{Config: cfg, GitHub: gh}
	if err := m.acceptWhitelistedInvitations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gh.accepted) != 0 || len(m.Config.Repositories) != 1 {
		t.Fatalf("accepted reclaimed identity: accepted=%v repos=%v", gh.accepted, m.Config.Repositories)
	}
}
