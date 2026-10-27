package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type selectedHandler func(Job) error

func (f selectedHandler) Execute(_ context.Context, job Job) error { return f(job) }

type reactionProbeGitHub struct {
	*fakeGitHub
	beforeReaction func()
	afterReaction  func()
}

func (g *reactionProbeGitHub) ReactEyes(ctx context.Context, nodeID string) error {
	if g.beforeReaction != nil {
		g.beforeReaction()
	}
	err := g.fakeGitHub.ReactEyes(ctx, nodeID)
	if g.afterReaction != nil {
		g.afterReaction()
	}
	return err
}

type permissionBreakGitHub struct {
	*fakeGitHub
	breakState func()
}

func (g *permissionBreakGitHub) Permission(ctx context.Context, repo, login string) (github.Permission, error) {
	permission, err := g.fakeGitHub.Permission(ctx, repo, login)
	if g.breakState != nil {
		g.breakState()
	}
	return permission, err
}

func selectedItem(at time.Time) github.Item {
	return github.Item{
		Key: "comment:1", NodeID: "node:1", HTMLURL: "https://github.com/example-owner/example-repo/pull/7#issuecomment-1",
		Body: "@linkdataminion review", UpdatedAt: at,
		Author: github.User{Login: "writer", ID: 7},
	}
}

func selectedAcceptance(t *testing.T, at time.Time) state.AcceptedAction {
	t.Helper()
	return state.AcceptedAction{
		JobID: jobID("comment:1", "review"), AttemptID: "0123456789abcdef0123456789abcdef", ExecutorID: "local",
		Repository: "example-owner/example-repo", RepositoryID: 1, OwnerID: 2,
		SubjectType: "PullRequest", SubjectNumber: 7,
		SourceKey: "comment:1", SourceNodeID: "node:1", SourceURL: selectedItem(at).HTMLURL, SourceUpdatedAt: at,
		Requester: "writer", RequesterID: 7, CommandName: "review", Action: "review",
		AcceptedAt: at, Deadline: at.Add(7 * time.Minute),
	}
}

func selectedMonitor(t *testing.T, at time.Time, gh GitHub, handler ActionHandler) *Monitor {
	t.Helper()
	cfg := testConfig(t)
	cfg.MaxRuntimeMinutes = 7
	return &Monitor{Config: cfg, GitHub: gh, Handler: handler, Now: func() time.Time { return at }, SelectedExecutorID: "local"}
}

func TestSelectedAcceptanceSavedBeforeReactionAndCarriesFixedIdentity(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	item := selectedItem(at)
	gh := &reactionProbeGitHub{fakeGitHub: &fakeGitHub{
		notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"},
	}}
	var saved state.Data
	beforeAccept := time.Now().UTC()
	gh.beforeReaction = func() {
		bytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bytes, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Accepted == nil || saved.JobFailures[saved.Accepted.JobID].Attempts != 1 {
			t.Fatalf("reaction preceded accepted attempt save: %+v", saved)
		}
	}
	var executed int
	handler := selectedHandler(func(job Job) error {
		executed++
		accepted := saved.Accepted
		if accepted == nil || job.ID != accepted.JobID || job.AttemptID != accepted.AttemptID ||
			!job.AcceptedAt.Equal(accepted.AcceptedAt) || !job.Deadline.Equal(accepted.Deadline) ||
			!job.Deadline.Equal(job.AcceptedAt.Add(7*time.Minute)) ||
			accepted.ExecutorID != "local" || accepted.Repository != "example-owner/example-repo" ||
			accepted.RepositoryID != 1 || accepted.OwnerID != 2 || accepted.SubjectType != "PullRequest" ||
			accepted.SubjectNumber != 7 || accepted.SourceKey != item.Key || accepted.SourceNodeID != item.NodeID ||
			accepted.SourceURL != item.HTMLURL || !accepted.SourceUpdatedAt.Equal(item.UpdatedAt) ||
			accepted.Requester != "writer" || accepted.RequesterID != 7 || accepted.CommandName != "review" || accepted.Action != "review" {
			t.Fatalf("accepted identity or deadline changed: job=%+v accepted=%+v", job, accepted)
		}
		if err := accepted.Validate(); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	m := selectedMonitor(t, at, gh, handler)
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if saved.Accepted == nil || saved.Accepted.AcceptedAt.Before(beforeAccept) || saved.Accepted.AcceptedAt.After(time.Now().UTC()) {
		t.Fatalf("acceptance time did not use the live clock: %+v", saved.Accepted)
	}
	if executed != 1 || store.Data.Accepted != nil || len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 {
		t.Fatalf("selected completion not cleared: executions=%d state=%+v", executed, store.Data)
	}
}

func TestSelectedRestartAfterReactionResumesOriginalAttemptAndDeadline(t *testing.T) {
	scanAt := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := selectedItem(scanAt)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, scanAt.Add(-time.Minute))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	gh := &reactionProbeGitHub{fakeGitHub: &fakeGitHub{
		notifications: []github.Notification{testNotification(scanAt)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"},
	}, afterReaction: cancel}
	m := selectedMonitor(t, scanAt, gh, selectedHandler(func(Job) error { t.Fatal("canceled scan dispatched work"); return nil }))
	m.Config.Repositories[0].AllowedActors = []string{"writer"}
	m.Config.Repositories[0].AllowedActorIDs = map[string]int64{"writer": 7}
	if _, err := m.RunOnce(ctx, store); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-reaction cancellation = %v", err)
	}
	if store.Data.Accepted == nil || store.Data.Accepted.Assigned || store.Data.JobFailures[store.Data.Accepted.JobID].Attempts != 1 {
		t.Fatalf("crash point lost unassigned acceptance: %+v", store.Data)
	}
	original := *store.Data.Accepted
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, scanAt.Add(-time.Minute))
	gh.afterReaction = nil
	gh.currentItem = func(seed github.Item) (github.Item, error) {
		if seed.Key != original.SourceKey || seed.NodeID != original.SourceNodeID {
			t.Fatalf("restart read another source: %+v", seed)
		}
		return item, nil
	}
	var executions int
	m.Handler = selectedHandler(func(job Job) error {
		executions++
		if job.AttemptID != original.AttemptID || !job.AcceptedAt.Equal(original.AcceptedAt) || !job.Deadline.Equal(original.Deadline) ||
			reopened.Data.Accepted == nil || reopened.Data.JobFailures[job.ID].Attempts != 1 ||
			!job.Repository.AllowsAction(job.Command.Name) || !job.Repository.AllowsActor("writer", 7) ||
			len(job.Repository.Actions) == 0 || len(job.Repository.AllowedActors) == 0 {
			t.Fatalf("restart minted another acceptance: job=%+v original=%+v state=%+v", job, original, reopened.Data)
		}
		return nil
	})
	if _, err := m.RunOnce(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	if executions != 1 || len(gh.reactions) != 2 || len(gh.notificationCalls) != 1 ||
		len(reopened.Data.Processed) != 1 || reopened.Data.Accepted != nil {
		t.Fatalf("restart discovered or dispatched duplicate work: executions=%d reactions=%v scans=%v state=%+v", executions, gh.reactions, gh.notificationCalls, reopened.Data)
	}
}

func TestSelectedReactionFailureRetainsOriginalAcceptance(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := selectedItem(at)
	reactionErr := errors.New("reaction unavailable")
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}, reactionErr: reactionErr}
	store := openState(t, at.Add(-time.Minute))
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { t.Fatal("reaction failure dispatched work"); return nil }))
	next, err := m.RunOnce(t.Context(), store)
	if !errors.Is(err, reactionErr) {
		t.Fatalf("reaction error = %v", err)
	}
	accepted := store.Data.Accepted
	if accepted == nil || !next.Equal(accepted.RetryAt) || !time.Now().Before(accepted.RetryAt) ||
		store.Data.JobFailures[accepted.JobID].Attempts != 1 || len(store.Data.Processed) != 0 {
		t.Fatalf("reaction failure lost original acceptance: %+v", store.Data)
	}
	if next, err := m.RunOnce(t.Context(), store); err != nil || !next.Equal(accepted.RetryAt) || len(gh.reactions) != 1 {
		t.Fatalf("reaction retried before durable schedule: next=%s err=%v reactions=%v", next, err, gh.reactions)
	}
	gh.reactionErr = nil
	gh.currentItem = func(github.Item) (github.Item, error) { return item, nil }
	m.Handler = selectedHandler(func(job Job) error {
		if job.AttemptID != accepted.AttemptID || !job.Deadline.Equal(accepted.Deadline) {
			t.Fatalf("reaction retry renewed acceptance: job=%+v original=%+v", job, accepted)
		}
		return nil
	})
	accepted.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunOnce(t.Context(), store); err != nil || len(gh.reactions) != 2 || len(store.Data.Processed) != 1 {
		t.Fatalf("reaction retry failed to resume same action: err=%v reactions=%v state=%+v", err, gh.reactions, store.Data)
	}
}

func TestSelectedPreparationFailureReleasesUnusedAcceptance(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := selectedItem(at)
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}}
	store := openState(t, at.Add(-time.Minute))
	preparationErr := errors.New("preparation unavailable")
	var firstAttempt string
	m := selectedMonitor(t, at, gh, selectedHandler(func(job Job) error {
		firstAttempt = job.AttemptID
		if store.Data.Accepted == nil || store.Data.Accepted.JobID != job.ID || store.Data.Accepted.Assigned {
			t.Fatalf("preparation did not see its unassigned reservation: %+v", store.Data.Accepted)
		}
		return preparationErr
	}))
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, preparationErr) {
		t.Fatalf("preparation error = %v", err)
	}
	failure := store.Data.JobFailures[jobID(item.Key, "review")]
	if store.Data.Accepted != nil || failure.Attempts != 0 || failure.AcceptedAt.IsZero() || failure.Deadline.IsZero() || len(store.Data.Processed) != 0 {
		t.Fatalf("unused preparation retained acceptance: %+v", store.Data)
	}
	retryAt := advanceToRetry(at, store)
	m.Now = func() time.Time { return retryAt }
	m.Handler = selectedHandler(func(job Job) error {
		if job.AttemptID == firstAttempt || !job.AcceptedAt.Equal(failure.AcceptedAt) || !job.Deadline.Equal(failure.Deadline) {
			t.Fatalf("preparation retry renewed original deadline: job=%+v first=%+v", job, failure)
		}
		return nil
	})
	if _, err := m.RunOnce(t.Context(), store); err != nil || len(store.Data.Processed) != 1 {
		t.Fatalf("preparation retry failed: err=%v state=%+v", err, store.Data)
	}
}

func TestSelectedExpiredOriginalDeadlineNeverReaccepts(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "released preparation"
		if held {
			name = "unassigned restart"
		}
		t.Run(name, func(t *testing.T) {
			scanAt := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
			firstAt := time.Now().UTC().Add(-9 * time.Minute)
			item := selectedItem(scanAt)
			gh := &fakeGitHub{notifications: []github.Notification{testNotification(scanAt)}, items: []github.Item{item},
				permission: github.Permission{Permission: "write"}}
			gh.currentItem = func(github.Item) (github.Item, error) { return selectedItem(firstAt), nil }
			store := openState(t, scanAt.Add(-time.Minute))
			id := jobID(item.Key, "review")
			store.Data.JobFailures[id] = state.JobFailure{AcceptedAt: firstAt, Deadline: firstAt.Add(7 * time.Minute)}
			if held {
				accepted := selectedAcceptance(t, firstAt)
				store.Data.Accepted = &accepted
				failure := store.Data.JobFailures[id]
				failure.Attempts = 1
				store.Data.JobFailures[id] = failure
			}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			m := selectedMonitor(t, scanAt, gh, selectedHandler(func(Job) error { t.Fatal("expired action dispatched"); return nil }))
			if _, err := m.RunOnce(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			if len(gh.reactions) != 0 || len(gh.comments) != 1 || store.Data.Accepted != nil ||
				len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 {
				t.Fatalf("expired deadline renewed: reactions=%v comments=%+v state=%+v", gh.reactions, gh.comments, store.Data)
			}
		})
	}
}

func TestSelectedPreassignmentDeadlineErrorFinishesOriginalReply(t *testing.T) {
	firstAt := time.Now().UTC().Add(-9 * time.Minute)
	accepted := selectedAcceptance(t, firstAt)
	store := openState(t, firstAt.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: firstAt, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{permission: github.Permission{Permission: "write"}, currentItem: func(github.Item) (github.Item, error) {
		return selectedItem(firstAt), nil
	}}
	m := selectedMonitor(t, firstAt, gh, selectedHandler(func(Job) error { return context.DeadlineExceeded }))
	if err := m.executeSelected(t.Context(), store, jobFromAccepted(accepted), time.Now().UTC()); err != nil ||
		len(gh.comments) != 1 || store.Data.Accepted != nil || len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 {
		t.Fatalf("expired preparation renewed or lost original reply: err=%v state=%+v", err, store.Data)
	}
}

func TestSelectedFixedReplyCommandShapesReachHandler(t *testing.T) {
	for _, test := range []struct {
		name        string
		subjectType string
		commandName string
	}{
		{name: "review on issue", subjectType: "Issue", commandName: "review"},
		{name: "fix on pull request", subjectType: "PullRequest", commandName: "fix"},
	} {
		t.Run(test.name, func(t *testing.T) {
			at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
			notification := testNotification(at)
			notification.Subject.Type = test.subjectType
			if test.subjectType == "Issue" {
				notification.Subject.URL = "https://api.github.com/repos/example-owner/example-repo/issues/7"
			}
			item := selectedItem(at)
			item.Body = "@linkdataminion " + test.commandName
			gh := &fakeGitHub{notifications: []github.Notification{notification}, items: []github.Item{item},
				permission: github.Permission{Permission: "write"}}
			store := openState(t, at.Add(-time.Minute))
			var executions int
			m := selectedMonitor(t, at, gh, selectedHandler(func(job Job) error {
				executions++
				if job.SubjectType != test.subjectType || job.Command.Name != test.commandName || job.ID != jobID(item.Key, test.commandName) {
					t.Fatalf("fixed reply changed accepted command: %+v", job)
				}
				return nil
			}))
			if _, err := m.RunOnce(t.Context(), store); err != nil || executions != 1 ||
				len(store.Data.Processed) != 1 || store.Data.Accepted != nil {
				t.Fatalf("fixed reply was rejected: err=%v executions=%d state=%+v", err, executions, store.Data)
			}
		})
	}
}

func TestSelectedAssignedFailureIsTerminal(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := selectedItem(at)
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}}
	gh.currentItem = func(github.Item) (github.Item, error) { return item, nil }
	store := openState(t, at.Add(-time.Minute))
	workerErr := errors.New("worker failed")
	m := selectedMonitor(t, at, gh, selectedHandler(func(job Job) error {
		if store.Data.Accepted == nil || store.Data.Accepted.AttemptID != job.AttemptID {
			t.Fatalf("assignment lost reservation: %+v", store.Data.Accepted)
		}
		store.Data.Accepted.Assigned = true
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		return workerErr
	}))
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if len(gh.comments) != 1 || len(store.Data.Processed) != 1 || store.Data.Accepted != nil || len(store.Data.JobFailures) != 0 {
		t.Fatalf("assigned failure remained dispatchable: comments=%+v state=%+v", gh.comments, store.Data)
	}
}

func TestSelectedFailedReplyStopsBeforeAnotherAcceptance(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	first := selectedItem(at)
	second := selectedItem(at)
	second.Key, second.NodeID = "comment:2", "node:2"
	replyErr := errors.New("failure reply unavailable")
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{first, second},
		permission: github.Permission{Permission: "write"}, commentErr: replyErr}
	gh.currentItem = func(seed github.Item) (github.Item, error) {
		if seed.Key == first.Key {
			return first, nil
		}
		return second, nil
	}
	store := openState(t, at.Add(-time.Minute))
	var executions int
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
		executions++
		store.Data.Accepted.Assigned = true
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		return errors.New("worker failed")
	}))
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, replyErr) {
		t.Fatalf("reply error = %v", err)
	}
	if executions != 1 || len(gh.reactions) != 1 || store.Data.Accepted == nil ||
		store.Data.Accepted.JobID != jobID(first.Key, "review") || store.Data.JobFailures[store.Data.Accepted.JobID].Attempts != maxJobAttempts {
		t.Fatalf("failed reply let another acceptance through: executions=%d reactions=%v state=%+v", executions, gh.reactions, store.Data)
	}
	gh.commentErr = nil
	if _, err := m.RunOnce(t.Context(), store); err != nil || executions != 1 || len(gh.comments) != 0 {
		t.Fatalf("failure reply retried before due: err=%v executions=%d comments=%+v", err, executions, gh.comments)
	}
	store.Data.Accepted.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunOnce(t.Context(), store); err != nil || executions != 1 || len(gh.comments) != 1 || store.Data.Accepted != nil {
		t.Fatalf("recovery did not finish held reply: err=%v executions=%d comments=%+v state=%+v", err, executions, gh.comments, store.Data)
	}
}

func TestSelectedUnassignedRecoveryKeepsAcceptanceWithoutCapacity(t *testing.T) {
	at := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	accepted := selectedAcceptance(t, at)
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 2, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	unavailable := errors.New("capacity unavailable")
	handler := &capacityHandler{failAt: 1, err: unavailable}
	m := selectedMonitor(t, at, &fakeGitHub{beforeList: func() { t.Fatal("discovered before recovery") }}, handler)
	if _, err := m.RunOnce(t.Context(), reopened); !errors.Is(err, unavailable) {
		t.Fatalf("capacity error = %v", err)
	}
	if reopened.Data.Accepted == nil || reopened.Data.Accepted.AttemptID != accepted.AttemptID ||
		reopened.Data.JobFailures[accepted.JobID].Attempts != 2 || len(handler.jobs) != 0 {
		t.Fatalf("capacity failure renewed or released original acceptance: %+v", reopened.Data)
	}
}

func TestSelectedRecoveryRejectsChangedSourceOrAuthority(t *testing.T) {
	for _, test := range []struct {
		name       string
		changeItem func(*github.Item)
		changeGH   func(*fakeGitHub)
	}{
		{name: "edited", changeItem: func(item *github.Item) { item.Edited = true }},
		{name: "author", changeItem: func(item *github.Item) { item.Author.ID++ }},
		{name: "command", changeItem: func(item *github.Item) { item.Body = "@linkdataminion fix" }},
		{name: "source", changeItem: func(item *github.Item) { item.NodeID = "other-node" }},
		{name: "permission", changeGH: func(gh *fakeGitHub) { gh.permission.User.ID = 99 }},
		{name: "repository", changeGH: func(gh *fakeGitHub) {
			repo := fixtureRepository(github.Repository{FullName: "example-owner/example-repo"})
			repo.ID++
			gh.liveRepo = &repo
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			at := time.Now().UTC()
			store := openState(t, at.Add(-time.Minute))
			accepted := selectedAcceptance(t, at)
			store.Data.Accepted = &accepted
			store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			gh := &fakeGitHub{permission: github.Permission{Permission: "write"}}
			if test.changeGH != nil {
				test.changeGH(gh)
			}
			gh.currentItem = func(github.Item) (github.Item, error) {
				item := selectedItem(at)
				if test.changeItem != nil {
					test.changeItem(&item)
				}
				return item, nil
			}
			m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { t.Fatal("changed action dispatched"); return nil }))
			if _, err := m.RunOnce(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			if store.Data.Accepted != nil || len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 ||
				len(gh.reactions) != 0 || len(gh.comments) != 0 {
				t.Fatalf("changed source or authority was not discarded: %+v", store.Data)
			}
		})
	}
}

func TestSelectedRecoveryKeepsReservationOnTransientSourceError(t *testing.T) {
	at := time.Now().UTC()
	store := openState(t, at.Add(-time.Minute))
	accepted := selectedAcceptance(t, at)
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	lookupErr := errors.New("GitHub unavailable")
	gh := &fakeGitHub{items: []github.Item{selectedItem(at)}, permission: github.Permission{Permission: "write"}, currentItem: func(github.Item) (github.Item, error) {
		return github.Item{}, lookupErr
	}}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { t.Fatal("lookup error dispatched"); return nil }))
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, lookupErr) || store.Data.Accepted == nil ||
		store.Data.Accepted.AttemptID != accepted.AttemptID || store.Data.JobFailures[accepted.JobID].Attempts != 1 {
		t.Fatalf("transient lookup released original acceptance: err=%v state=%+v", err, store.Data)
	}
	gh.currentItem = func(github.Item) (github.Item, error) { return selectedItem(at), nil }
	m.Handler = selectedHandler(func(job Job) error {
		if job.AttemptID != accepted.AttemptID || !job.Deadline.Equal(accepted.Deadline) {
			t.Fatalf("retry renewed original acceptance: %+v", job)
		}
		return nil
	})
	if _, err := m.RunOnce(t.Context(), store); err != nil || store.Data.Accepted == nil || len(store.Data.Processed) != 0 {
		t.Fatalf("transient lookup retried before due: err=%v state=%+v", err, store.Data)
	}
	store.Data.Accepted.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunOnce(t.Context(), store); err != nil || len(store.Data.Processed) != 1 {
		t.Fatalf("transient lookup did not resume original action: err=%v state=%+v", err, store.Data)
	}
}

func TestSelectedAssignedRecoveryRepliesWithoutRedispatch(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	accepted := selectedAcceptance(t, at)
	accepted.Assigned = true
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1}
	store.Data.NextInviteCheckAt = at.Add(time.Hour)
	store.Data.NextPollAt = at.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	gh := &fakeGitHub{beforeList: func() { t.Fatal("invitation scan preceded recovery") },
		permission: github.Permission{Permission: "write"}, currentItem: func(github.Item) (github.Item, error) { return selectedItem(at), nil }}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { t.Fatal("assigned action redispatched"); return nil }))
	if _, err := m.RunOnce(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	if len(gh.comments) != 1 || gh.comments[0].Body != failureReply+"\n\n"+(Job{ID: accepted.JobID}).Marker() ||
		len(gh.verifiedCalls) != 1 || !gh.verifiedCalls[0] || len(reopened.Data.Processed) != 1 ||
		reopened.Data.Accepted != nil || len(reopened.Data.JobFailures) != 0 {
		t.Fatalf("assigned recovery did not terminalize original reply target: comments=%+v state=%+v", gh.comments, reopened.Data)
	}
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || gh.commentCalls != 1 {
		t.Fatalf("completed recovery repeated reply: err=%v replies=%d", err, gh.commentCalls)
	}
}

func TestSelectedUnassignedModelMarkerRecoversAsTerminal(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	store := openState(t, at.Add(-time.Minute))
	accepted := selectedAcceptance(t, at)
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, ModelCallStarted: true}
	store.Data.NextInviteCheckAt, store.Data.NextPollAt = at.Add(time.Hour), at.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{permission: github.Permission{Permission: "write"}, currentItem: func(github.Item) (github.Item, error) {
		return selectedItem(at), nil
	}}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { t.Fatal("model marker redispatched work"); return nil }))
	if _, err := m.RunOnce(t.Context(), store); err != nil || len(gh.comments) != 1 ||
		store.Data.Accepted != nil || len(store.Data.Processed) != 1 {
		t.Fatalf("unassigned model marker was refunded: err=%v state=%+v", err, store.Data)
	}
}

func TestSelectedAssignedRecoveryChecksRepositoryIdentityBeforeReply(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	store := openState(t, at.Add(-time.Minute))
	accepted := selectedAcceptance(t, at)
	accepted.Assigned = true
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	wrong := fixtureRepository(github.Repository{FullName: accepted.Repository})
	wrong.ID++
	gh := &fakeGitHub{liveRepo: &wrong}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { t.Fatal("assigned action redispatched"); return nil }))
	if _, err := m.RunOnce(t.Context(), store); err != nil || len(gh.comments) != 0 || store.Data.Accepted != nil || len(store.Data.Processed) != 1 {
		t.Fatalf("repository mismatch published failure reply: err=%v state=%+v", err, store.Data)
	}
}

func TestSelectedTerminalRecoveryRechecksSourceAndAuthority(t *testing.T) {
	for _, test := range []struct {
		name         string
		assigned     bool
		modelStarted bool
		expired      bool
		changeItem   func(*github.Item)
		changeGH     func(*fakeGitHub)
	}{
		{name: "assigned edited source", assigned: true, changeItem: func(item *github.Item) { item.Edited = true }},
		{name: "model started revoked permission", modelStarted: true, changeGH: func(gh *fakeGitHub) {
			gh.permission.Permission = "read"
		}},
		{name: "expired changed command", expired: true, changeItem: func(item *github.Item) {
			item.Body = "@linkdataminion fix"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			at := time.Now().UTC()
			if test.expired {
				at = at.Add(-9 * time.Minute)
			}
			accepted := selectedAcceptance(t, at)
			accepted.Assigned = test.assigned
			store := openState(t, at.Add(-time.Minute))
			store.Data.Accepted = &accepted
			store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, ModelCallStarted: test.modelStarted,
				AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			gh := &fakeGitHub{permission: github.Permission{Permission: "write"}}
			if test.changeGH != nil {
				test.changeGH(gh)
			}
			gh.currentItem = func(github.Item) (github.Item, error) {
				item := selectedItem(at)
				if test.changeItem != nil {
					test.changeItem(&item)
				}
				return item, nil
			}
			m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { t.Fatal("terminal action redispatched"); return nil }))
			if _, err := m.RunOnce(t.Context(), store); err != nil || len(gh.comments) != 0 ||
				store.Data.Accepted != nil || len(store.Data.Processed) != 1 {
				t.Fatalf("changed terminal source received failure reply: err=%v state=%+v", err, store.Data)
			}
		})
	}
}

func TestSelectedTerminalRecoveryRetainsTransientLookupAndSchedulesRetry(t *testing.T) {
	at := time.Now().UTC()
	accepted := selectedAcceptance(t, at)
	accepted.Assigned = true
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline}
	store.Data.NextPollAt = at.Add(-time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	lookupErr := errors.New("source lookup unavailable")
	gh := &fakeGitHub{items: []github.Item{selectedItem(at)}, permission: github.Permission{Permission: "write"}, currentItem: func(github.Item) (github.Item, error) {
		return github.Item{}, lookupErr
	}}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { t.Fatal("terminal action redispatched"); return nil }))
	before := time.Now().UTC()
	next, err := m.RunOnce(t.Context(), store)
	if !errors.Is(err, lookupErr) || next.Before(before.Add(time.Minute-time.Second)) ||
		store.Data.Accepted == nil || !store.Data.Accepted.RetryAt.Equal(next) ||
		!store.Data.Accepted.Deadline.Equal(accepted.Deadline) || len(gh.comments) != 0 {
		t.Fatalf("transient terminal lookup spun or cleared reservation: next=%s err=%v state=%+v", next, err, store.Data)
	}
	lookups := gh.currentChecks
	if next, err := m.RunOnce(t.Context(), store); err != nil || !next.Equal(store.Data.Accepted.RetryAt) || gh.currentChecks != lookups {
		t.Fatalf("one-pass poll retried before due time: next=%s err=%v lookups=%d", next, err, gh.currentChecks)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	if reopened.Data.Accepted == nil || !reopened.Data.Accepted.RetryAt.Equal(store.Data.Accepted.RetryAt) {
		t.Fatalf("retry schedule was not durable: %+v", reopened.Data)
	}
	if next, err := m.RunOnce(t.Context(), reopened); err != nil || !next.Equal(reopened.Data.Accepted.RetryAt) || gh.currentChecks != lookups {
		t.Fatalf("restart retried before due time: next=%s err=%v lookups=%d", next, err, gh.currentChecks)
	}
	reopened.Data.Accepted.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := reopened.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunOnce(t.Context(), reopened); !errors.Is(err, lookupErr) || gh.currentChecks != lookups+1 {
		t.Fatalf("due retry skipped transient lookup: err=%v lookups=%d", err, gh.currentChecks)
	}
	gh.currentItem = func(github.Item) (github.Item, error) { return selectedItem(at), nil }
	reopened.Data.Accepted.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := reopened.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || len(gh.comments) != 1 || reopened.Data.Accepted != nil {
		t.Fatalf("terminal lookup retry did not finish: err=%v state=%+v", err, reopened.Data)
	}
}

func TestSelectedTerminalRecoveryKeepsExistingVerifiedReply(t *testing.T) {
	at := time.Now().UTC()
	accepted := selectedAcceptance(t, at)
	accepted.Assigned = true
	store := openState(t, at.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reply := github.Item{Key: "review:published", Body: "Complete.\n\n" + (Job{ID: accepted.JobID}).Marker(),
		Author: github.User{Login: "linkdataminion"}, UpdatedAt: at}
	gh := &fakeGitHub{reviewReplies: []github.Item{reply}, currentItem: func(seed github.Item) (github.Item, error) {
		if seed.Key != reply.Key {
			return github.Item{}, errors.New("source is no longer available")
		}
		return reply, nil
	}}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { t.Fatal("completed action redispatched"); return nil }))
	if _, err := m.RunOnce(t.Context(), store); err != nil || gh.commentCalls != 0 ||
		store.Data.Accepted != nil || len(store.Data.Processed) != 1 {
		t.Fatalf("existing verified reply was ignored: err=%v state=%+v", err, store.Data)
	}
}

func TestSelectedAcceptanceSaveFailureStopsBeforeReaction(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	item := selectedItem(at)
	gh := &permissionBreakGitHub{fakeGitHub: &fakeGitHub{notifications: []github.Notification{testNotification(at)},
		items: []github.Item{item}, permission: github.Permission{Permission: "write"}}}
	gh.breakState = func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { t.Fatal("failed acceptance dispatched work"); return nil }))
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, ErrAcceptanceState) || len(gh.reactions) != 0 {
		t.Fatalf("acceptance save failed open: err=%v reactions=%v", err, gh.reactions)
	}
}

func TestSelectedAssignmentBarrierErrorStopsWithoutReplyOrRelease(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	store := openState(t, at.Add(-time.Minute))
	item := selectedItem(at)
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}}
	barrierErr := errors.Join(ErrAcceptanceState, errors.New("assignment save failed"))
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { return barrierErr }))
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, ErrAcceptanceState) ||
		store.Data.Accepted == nil || store.Data.JobFailures[store.Data.Accepted.JobID].Attempts != 1 || len(gh.comments) != 0 {
		t.Fatalf("ambiguous assignment was released or replied: err=%v state=%+v", err, store.Data)
	}
}

func TestSelectedReservedThirdAttemptResumesAfterRestart(t *testing.T) {
	at := time.Now().UTC()
	accepted := selectedAcceptance(t, at)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{
		Attempts: maxJobAttempts, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	gh := &fakeGitHub{permission: github.Permission{Permission: "write"}, currentItem: func(github.Item) (github.Item, error) {
		return selectedItem(at), nil
	}}
	var executions int
	m := selectedMonitor(t, at, gh, selectedHandler(func(job Job) error {
		executions++
		if job.AttemptID != accepted.AttemptID || !job.Deadline.Equal(accepted.Deadline) ||
			reopened.Data.JobFailures[job.ID].Attempts != maxJobAttempts {
			t.Fatalf("reserved third attempt changed: job=%+v state=%+v", job, reopened.Data)
		}
		return nil
	}))
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || executions != 1 || len(gh.comments) != 0 ||
		reopened.Data.Accepted != nil || len(reopened.Data.Processed) != 1 {
		t.Fatalf("unused third attempt was terminalized: err=%v executions=%d state=%+v", err, executions, reopened.Data)
	}
}

func TestSelectedThirdAttemptExpiryPersistsTerminalMarkerBeforeReply(t *testing.T) {
	at := time.Now().UTC().Add(-9 * time.Minute)
	accepted := selectedAcceptance(t, at)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{
		Attempts: maxJobAttempts, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	replyErr := errors.New("failure reply unavailable")
	gh := &fakeGitHub{permission: github.Permission{Permission: "write"}, commentErr: replyErr,
		currentItem: func(github.Item) (github.Item, error) { return selectedItem(at), nil }}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
		t.Fatal("expired reservation dispatched work")
		return nil
	}))
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, replyErr) || store.Data.Accepted == nil ||
		!store.Data.Accepted.Terminal || store.Data.JobFailures[accepted.JobID].Attempts != maxJobAttempts {
		t.Fatalf("terminal marker was not saved before reply: err=%v state=%+v", err, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	if reopened.Data.Accepted == nil || !reopened.Data.Accepted.Terminal {
		t.Fatalf("terminal marker was lost after restart: %+v", reopened.Data)
	}
	reopened.Data.Accepted.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := reopened.Save(); err != nil {
		t.Fatal(err)
	}
	gh.commentErr = nil
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || len(gh.comments) != 1 || reopened.Data.Accepted != nil {
		t.Fatalf("terminal retry redispatched or missed reply: err=%v state=%+v", err, reopened.Data)
	}
}

func TestSelectedDiscardRetainsPushedFixHead(t *testing.T) {
	at := time.Now().UTC()
	accepted := selectedAcceptance(t, at)
	accepted.SubjectType, accepted.CommandName, accepted.Action = "Issue", "fix", "fix"
	accepted.SourceURL = "https://github.com/example-owner/example-repo/issues/7#issuecomment-1"
	accepted.JobID = jobID(accepted.SourceKey, accepted.CommandName)
	accepted.Assigned = true
	const expectedSHA = "0123456789abcdef0123456789abcdef01234567"
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{
		Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
	}
	store.Data.PendingFixes[accepted.JobID] = state.PendingFix{SHA: expectedSHA}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{permission: github.Permission{Permission: "write"}, currentItem: func(github.Item) (github.Item, error) {
		item := selectedItem(at)
		item.Body, item.Edited = "@linkdataminion fix", true
		item.HTMLURL = accepted.SourceURL
		return item, nil
	}}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
		t.Fatal("terminal fix redispatched")
		return nil
	}))
	if _, err := m.RunOnce(t.Context(), store); err != nil || store.Data.Accepted != nil || len(gh.comments) != 0 ||
		store.Data.PendingFixes[accepted.JobID].SHA != expectedSHA {
		t.Fatalf("discard lost pushed fix head: err=%v state=%+v", err, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	if reopened.Data.PendingFixes[accepted.JobID].SHA != expectedSHA || len(reopened.Data.Processed) != 1 {
		t.Fatalf("restart lost pushed fix head: %+v", reopened.Data)
	}
}
