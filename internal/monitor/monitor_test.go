package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type fakeGitHub struct {
	notifications []github.Notification
	items         []github.Item
	invitations   []github.Invitation
	permission    github.Permission
	permissionErr error
	reactionErr   error
	acceptErr     error
	beforeAccept  func()
	accepted      []int64
	reactions     []string
	checks        int
	currentItem   func(github.Item) (github.Item, error)
}

func (f *fakeGitHub) ListNotifications(context.Context, time.Time) ([]github.Notification, time.Duration, error) {
	return f.notifications, time.Minute, nil
}

func (f *fakeGitHub) ListRepositoryInvitations(context.Context) ([]github.Invitation, error) {
	return f.invitations, nil
}

func (f *fakeGitHub) AcceptRepositoryInvitation(_ context.Context, id int64) error {
	if f.beforeAccept != nil {
		f.beforeAccept()
	}
	f.accepted = append(f.accepted, id)
	return f.acceptErr
}

func (f *fakeGitHub) ThreadItems(context.Context, string, string, int, time.Time) ([]github.Item, error) {
	return f.items, nil
}

func (f *fakeGitHub) CurrentItem(_ context.Context, item github.Item) (github.Item, error) {
	if f.currentItem != nil {
		return f.currentItem(item)
	}
	return item, nil
}

func (f *fakeGitHub) Permission(context.Context, string, string) (github.Permission, error) {
	f.checks++
	return f.permission, f.permissionErr
}

func (f *fakeGitHub) ReactEyes(_ context.Context, nodeID string) error {
	f.reactions = append(f.reactions, nodeID)
	return f.reactionErr
}

type fakeExecutor struct {
	jobs      []Job
	onExecute func()
}

func (f *fakeExecutor) Execute(_ context.Context, job Job) error {
	if f.onExecute != nil {
		f.onExecute()
	}
	f.jobs = append(f.jobs, job)
	return nil
}

func testConfig() config.Config {
	return config.Config{
		BotLogin:            "linkdataminion",
		PollIntervalSeconds: 60,
		Repositories: []config.Repository{{
			Name: "example-owner/example-repo", Actions: []string{"review", "fix", "release"},
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
	if len(executor.jobs) != 1 || gh.checks != 1 || len(gh.reactions) != 1 {
		t.Fatalf("duplicate command handled: %d jobs, %d permission checks, %d reactions", len(executor.jobs), gh.checks, len(gh.reactions))
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
	if !store.Data.Cursor.Equal(initial) || len(store.Data.Processed) != 0 || len(executor.jobs) != 0 {
		t.Fatalf("command was lost or executed without acknowledgement: %+v", store.Data)
	}
	gh.reactionErr = nil
	clock = clock.Add(time.Minute)
	if _, err := m.RunOnce(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(gh.reactions) != 2 || len(executor.jobs) != 1 {
		t.Fatalf("reaction retry: %d reactions, %d jobs", len(gh.reactions), len(executor.jobs))
	}
}

func TestRunOnceKeepsCursorOnPermissionFailure(t *testing.T) {
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
	if !store.Data.Cursor.Equal(initial) || len(store.Data.Processed) != 0 || len(executor.jobs) != 0 {
		t.Fatalf("command was lost or executed after permission failure: %+v", store.Data)
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
		{ID: 1, Repository: github.Repository{FullName: "example-owner/example-repo", Owner: github.User{Login: "example-owner"}}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "write"},
		{ID: 2, Repository: github.Repository{FullName: "other/project", Owner: github.User{Login: "other"}}, Invitee: github.User{Login: "linkdataminion"}, Inviter: github.User{Login: "example-owner"}, Permissions: "admin"},
		{ID: 3, Repository: github.Repository{FullName: "example-owner/example-repo", Owner: github.User{Login: "example-owner"}}, Invitee: github.User{Login: "someone-else"}, Permissions: "write"},
		{ID: 4, Repository: github.Repository{FullName: "example-owner/example-repo", Owner: github.User{Login: "example-owner"}}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "write", Expired: true},
		{ID: 5, Repository: github.Repository{FullName: "example-owner/private-repo", Owner: github.User{Login: "example-owner"}}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "read"},
		{ID: 6, Repository: github.Repository{FullName: "another-owner/service", Owner: github.User{Login: "another-owner"}}, Invitee: github.User{Login: "linkdataminion"}, Inviter: github.User{Login: "someone-else"}, Permissions: "triage"},
		{ID: 7, Repository: github.Repository{FullName: "example-owner/mismatch", Owner: github.User{Login: "other"}}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "write"},
		{ID: 8, Repository: github.Repository{FullName: "other/mismatch", Owner: github.User{Login: "example-owner"}}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "write"},
		{ID: 9, Repository: github.Repository{FullName: "example-owner/ownerless"}, Invitee: github.User{Login: "linkdataminion"}, Permissions: "write"},
	}}
	config := testConfig()
	config.InvitationOwners = []string{"example-owner", "another-owner"}
	m := &Monitor{Config: config, GitHub: gh}
	if err := m.acceptWhitelistedInvitations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gh.accepted) != 3 || gh.accepted[0] != 1 || gh.accepted[1] != 5 || gh.accepted[2] != 6 {
		t.Fatalf("accepted invitations %v, want 1, 5, 6", gh.accepted)
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
