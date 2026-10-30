package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type selectedHandler func(Job) error

func (f selectedHandler) Execute(_ context.Context, job Job) error { return f(job) }

func (selectedHandler) WaitCapacity(context.Context) error { return nil }

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
	return &Monitor{Config: cfg, GitHub: gh, BotID: 8, Handler: handler, Now: func() time.Time { return at }, SelectedExecutorID: "local"}
}

func TestSelectedFixRestartPreservesUnknownSourceTimestamp(t *testing.T) {
	at := time.Now().UTC()
	item := selectedItem(time.Time{})
	item.Body = "@linkdataminion fix"
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	reactionErr := errors.New("reaction unavailable")
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}, reactionErr: reactionErr}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
		t.Fatal("failed reaction dispatched fix")
		return nil
	}))
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, reactionErr) || store.Data.Accepted == nil ||
		!store.Data.Accepted.SourceUpdatedAt.IsZero() {
		t.Fatalf("first acceptance invented a source timestamp: err=%v accepted=%+v", err, store.Data.Accepted)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	if reopened.Data.Accepted == nil || !reopened.Data.Accepted.SourceUpdatedAt.IsZero() {
		t.Fatalf("restart invented a source timestamp: %+v", reopened.Data.Accepted)
	}
	reopened.Data.Accepted.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := reopened.Save(); err != nil {
		t.Fatal(err)
	}
	gh.reactionErr = nil
	gh.currentItem = func(github.Item) (github.Item, error) { return item, nil }
	m.Handler = selectedHandler(func(job Job) error {
		if !job.Source.UpdatedAt.IsZero() || job.Command.Name != "fix" {
			t.Fatalf("recovered fix changed its unknown timestamp: %+v", job)
		}
		return nil
	})
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || len(reopened.Data.Processed) != 1 {
		t.Fatalf("fix recovery failed: err=%v state=%+v", err, reopened.Data)
	}
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
	at := time.Now().UTC()
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

func TestSelectedPreparationFailureConsumesAttemptAndReleasesAcceptance(t *testing.T) {
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
	if store.Data.Accepted != nil || failure.Attempts != 1 || failure.AcceptedAt.IsZero() || failure.Deadline.IsZero() || len(store.Data.Processed) != 0 {
		t.Fatalf("failed preparation did not consume an attempt and release acceptance: %+v", store.Data)
	}
	retryAt := advanceToRetry(at, store)
	m.Now = func() time.Time { return retryAt }
	m.Handler = selectedHandler(func(job Job) error {
		if job.AttemptID == firstAttempt || !job.AcceptedAt.Equal(failure.AcceptedAt) || !job.Deadline.Equal(failure.Deadline) ||
			store.Data.JobFailures[job.ID].Attempts != 2 {
			t.Fatalf("preparation retry renewed original deadline: job=%+v first=%+v", job, failure)
		}
		return nil
	})
	if _, err := m.RunOnce(t.Context(), store); err != nil || len(store.Data.Processed) != 1 {
		t.Fatalf("preparation retry failed: err=%v state=%+v", err, store.Data)
	}
}

func TestSelectedMissingSkillAfterAcceptanceRetainsAttemptAcrossRestart(t *testing.T) {
	at := time.Now().UTC()
	item := selectedItem(at)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}}
	m := selectedMonitor(t, at, gh, nil)
	skillPath := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "review", "SKILL.md")
	skillText, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	skillErr := errors.New("required skill is not installed")
	var original state.AcceptedAction
	m.Handler = selectedHandler(func(job Job) error {
		if err := store.WithLock(func() error {
			if store.Data.Accepted == nil || store.Data.Accepted.AttemptID != job.AttemptID {
				return fmt.Errorf("handler started without saved acceptance: %+v", store.Data.Accepted)
			}
			original = *store.Data.Accepted
			return nil
		}); err != nil {
			return err
		}
		if err := os.Remove(skillPath); err != nil {
			return err
		}
		return skillErr
	})
	next, err := m.RunOnce(t.Context(), store)
	if !errors.Is(err, skillErr) || store.Data.Accepted == nil || !next.Equal(store.Data.Accepted.RetryAt) ||
		store.Data.Accepted.AttemptID != original.AttemptID || !store.Data.Accepted.AcceptedAt.Equal(original.AcceptedAt) ||
		!store.Data.Accepted.Deadline.Equal(original.Deadline) || store.Data.Accepted.Assigned || store.Data.Accepted.Terminal ||
		store.Data.JobFailures[original.JobID].Attempts != 1 || store.Data.JobFailures[original.JobID].ModelCallStarted ||
		len(store.Data.Processed) != 0 || len(store.Data.PendingNotifications) != 1 ||
		store.Data.PendingNotifications[notificationKey(gh.notifications[0])].Failures != 0 {
		t.Fatalf("missing skill released accepted attempt: next=%s err=%v state=%+v", next, err, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	if reopened.Data.Accepted == nil || reopened.Data.Accepted.AttemptID != original.AttemptID ||
		!reopened.Data.Accepted.AcceptedAt.Equal(original.AcceptedAt) || !reopened.Data.Accepted.Deadline.Equal(original.Deadline) ||
		reopened.Data.JobFailures[original.JobID].Attempts != 1 ||
		len(reopened.Data.PendingNotifications) != 1 {
		t.Fatalf("restart lost accepted attempt: %+v", reopened.Data)
	}
	m.Handler = selectedHandler(func(Job) error { t.Fatal("missing skill reached handler after restart"); return nil })
	reopened.Data.Accepted.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := reopened.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunOnce(t.Context(), reopened); err == nil || reopened.Data.Accepted == nil ||
		reopened.Data.Accepted.AttemptID != original.AttemptID || reopened.Data.JobFailures[original.JobID].Attempts != 1 {
		t.Fatalf("missing skill did not keep reservation after restart: err=%v state=%+v", err, reopened.Data)
	}
	if err := os.WriteFile(skillPath, skillText, 0600); err != nil {
		t.Fatal(err)
	}
	reopened.Data.Accepted.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := reopened.Save(); err != nil {
		t.Fatal(err)
	}
	gh.currentItem = func(github.Item) (github.Item, error) { return item, nil }
	var resumed int
	m.Handler = selectedHandler(func(job Job) error {
		resumed++
		if job.AttemptID != original.AttemptID || !job.AcceptedAt.Equal(original.AcceptedAt) ||
			!job.Deadline.Equal(original.Deadline) || reopened.Data.JobFailures[job.ID].Attempts != 1 ||
			reopened.Data.Accepted == nil || reopened.Data.Accepted.Assigned {
			t.Fatalf("restored skill minted a new attempt: job=%+v state=%+v", job, reopened.Data)
		}
		return nil
	})
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || resumed != 1 ||
		len(gh.notificationCalls) != 1 || reopened.Data.Accepted != nil || len(reopened.Data.Processed) != 1 {
		t.Fatalf("restored skill did not resume original attempt: err=%v resumed=%d state=%+v", err, resumed, reopened.Data)
	}
}

func TestSelectedInfrastructureFailureRefundsUnassignedAttempt(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := selectedItem(at)
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}}
	store := openState(t, at.Add(-time.Minute))
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { return ErrRequestTransport }))
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, ErrRequestTransport) {
		t.Fatalf("infrastructure error = %v", err)
	}
	failure := store.Data.JobFailures[jobID(item.Key, "review")]
	if store.Data.Accepted != nil || failure.Attempts != 0 || failure.AcceptedAt.IsZero() || failure.Deadline.IsZero() ||
		len(store.Data.Processed) != 0 || len(gh.comments) != 0 {
		t.Fatalf("infrastructure failure consumed an unassigned attempt: %+v", store.Data)
	}
}

func TestSelectedInterruptedTerminalFailureRefundsUnassignedAttempt(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := selectedItem(at)
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}}
	store := openState(t, at.Add(-time.Minute))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
		cancel()
		return errors.Join(ErrTerminalJob, context.Canceled)
	}))
	if _, err := m.RunOnce(ctx, store); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrTerminalJob) {
		t.Fatalf("interrupted terminal error = %v", err)
	}
	failure := store.Data.JobFailures[jobID(item.Key, "review")]
	if store.Data.Accepted != nil || failure.Attempts != 0 || failure.AcceptedAt.IsZero() || failure.Deadline.IsZero() ||
		len(store.Data.Processed) != 0 || len(gh.comments) != 0 {
		t.Fatalf("interrupted terminal failure consumed an unassigned attempt: %+v", store.Data)
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

func TestSelectedUncertainReplyDoesNotBlockOrRepeatAnotherAcceptance(t *testing.T) {
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
		if err := store.WithLock(func() error {
			store.Data.Accepted.Assigned = true
			return store.Save()
		}); err != nil {
			return err
		}
		return errors.New("worker failed")
	}))
	if _, err := m.RunOnce(t.Context(), store); err != nil {
		t.Fatalf("reply error = %v", err)
	}
	firstID, secondID := jobID(first.Key, "review"), jobID(second.Key, "review")
	if executions != 2 || len(gh.reactions) != 2 || store.Data.Accepted != nil ||
		store.Data.Processed[firstID].IsZero() || store.Data.Processed[secondID].IsZero() || gh.commentCalls != 2 ||
		store.Data.PublicationWrites[firstID].Status != state.PublicationWritePending ||
		store.Data.PublicationWrites[secondID].Status != state.PublicationWritePending {
		t.Fatalf("uncertain reply blocked or repeated another acceptance: executions=%d reactions=%v state=%+v", executions, gh.reactions, store.Data)
	}
	gh.commentErr = nil
	nextPoll := store.Data.NextPollAt
	if nextPoll.IsZero() {
		t.Fatal("first pass did not schedule another notification poll")
	}
	m.Now = func() time.Time { return nextPoll.Add(time.Second) }
	if _, err := m.RunOnce(t.Context(), store); err != nil || executions != 2 || gh.commentCalls != 2 ||
		store.Data.TerminalDecisions[firstID].Outcome != "failed" || store.Data.TerminalDecisions[secondID].Outcome != "failed" {
		t.Fatalf("later poll repeated work or changed terminal outcomes: err=%v executions=%d posts=%d state=%+v",
			err, executions, gh.commentCalls, store.Data)
	}
}

func TestSelectedUnassignedRecoveryKeepsAcceptanceWithoutCapacity(t *testing.T) {
	at := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	accepted := selectedAcceptance(t, at)
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 2, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
	store.Data.NextInviteCheckAt = at.Add(time.Hour)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	unavailable := errors.New("capacity unavailable")
	handler := &capacityHandler{failAt: 1, err: unavailable}
	m := selectedMonitor(t, at, &fakeGitHub{beforeList: func() { t.Fatal("invitation checked before due time") }}, handler)
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

func TestPreWorkerAssignedRecoveryRepliesWithoutRedispatch(t *testing.T) {
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
	m.SelectedExecutorID = "" // Recovery runs before a new worker attaches.
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

func TestSelectedIssueRecoveryChecksOriginalThread(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		for _, status := range []int{http.StatusMovedPermanently, http.StatusNotFound, http.StatusOK} {
			name := "unassigned"
			if terminal {
				name = "terminal"
			}
			t.Run(fmt.Sprintf("%s/%d", name, status), func(t *testing.T) {
				at := time.Now().UTC()
				accepted := selectedAcceptance(t, at)
				accepted.SubjectType = "Issue"
				accepted.SourceURL = "https://github.com/example-owner/example-repo/issues/7#issuecomment-1"
				accepted.Terminal = terminal
				path := filepath.Join(t.TempDir(), "state.json")
				store := openStateAt(t, path, at.Add(-time.Minute))
				store.Data.Accepted = &accepted
				attempts := 1
				if terminal {
					attempts = maxJobAttempts
				}
				store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: attempts, AcceptedAt: at, Deadline: accepted.Deadline}
				if err := store.Save(); err != nil {
					t.Fatal(err)
				}
				item := selectedItem(at)
				item.HTMLURL = accepted.SourceURL
				var detailsCalls, dispatches int
				gh := &fakeGitHub{permission: github.Permission{Permission: "write"},
					currentItem: func(github.Item) (github.Item, error) { return item, nil },
					issueDetails: func(repo string, number int) (github.Issue, error) {
						detailsCalls++
						if repo != accepted.Repository || number != accepted.SubjectNumber {
							t.Fatalf("checked wrong issue: %s #%d", repo, number)
						}
						if status != http.StatusOK {
							return github.Issue{}, &github.StatusError{Method: http.MethodGet, Path: "/repos/example-owner/example-repo/issues/7", StatusCode: status}
						}
						return github.Issue{ID: 77, NodeID: "issue-node", Number: 7, HTMLURL: "https://github.com/example-owner/example-repo/issues/7"}, nil
					},
				}
				m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { dispatches++; return nil }))
				_, err := m.RunOnce(t.Context(), store)
				if detailsCalls == 0 {
					t.Fatal("accepted issue was not checked at its original repository")
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store = openStateAt(t, path, at.Add(-time.Minute))
				if status == http.StatusNotFound && !terminal {
					var got *github.StatusError
					if !errors.As(err, &got) || got.StatusCode != status || store.Data.Accepted == nil ||
						store.Data.Accepted.AttemptID != accepted.AttemptID || store.Data.Accepted.Terminal != terminal ||
						!store.Data.Accepted.AcceptedAt.Equal(accepted.AcceptedAt) || !store.Data.Accepted.Deadline.Equal(accepted.Deadline) ||
						!store.Data.Accepted.RetryAt.After(time.Now()) || store.Data.JobFailures[accepted.JobID].Attempts != attempts ||
						len(store.Data.Processed) != 0 || dispatches != 0 || len(gh.comments) != 0 {
						t.Fatalf("404 did not preserve accepted retry: err=%v dispatches=%d comments=%v state=%+v", err, dispatches, gh.comments, store.Data)
					}
					return
				}
				_, processed := store.Data.Processed[accepted.JobID]
				if err != nil || store.Data.Accepted != nil || !processed || len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 {
					t.Fatalf("issue recovery result: err=%v state=%+v", err, store.Data)
				}
				if status == http.StatusNotFound {
					if dispatches != 0 || len(gh.comments) != 0 {
						t.Fatalf("terminal 404 posted a reply: dispatches=%d comments=%v", dispatches, gh.comments)
					}
					return
				}
				if status == http.StatusMovedPermanently {
					if dispatches != 0 || len(gh.comments) != 0 {
						t.Fatalf("transferred issue dispatched or replied: dispatches=%d comments=%v", dispatches, gh.comments)
					}
				} else if gh.currentChecks == 0 || terminal && (dispatches != 0 || len(gh.comments) != 1) || !terminal && (dispatches != 1 || len(gh.comments) != 0) {
					t.Fatalf("live issue did not recover normally: dispatches=%d comments=%v", dispatches, gh.comments)
				}
			})
		}
	}
}

func TestSelectedRecoveryChecksOriginalRepository(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusGone, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			at := time.Now().UTC()
			accepted := selectedAcceptance(t, at)
			path := filepath.Join(t.TempDir(), "state.json")
			store := openStateAt(t, path, at.Add(-time.Minute))
			store.Data.Accepted = &accepted
			store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			gh := &fakeGitHub{repositoryErr: &github.StatusError{Method: http.MethodGet,
				Path: "/repos/example-owner/example-repo", StatusCode: status}}
			m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
				t.Fatal("unavailable original repository dispatched")
				return nil
			}))
			_, err := m.RunOnce(t.Context(), store)
			if gh.repoChecks == 0 || len(gh.reactions) != 0 {
				t.Fatalf("original repository check was bypassed: checks=%d reactions=%v", gh.repoChecks, gh.reactions)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openStateAt(t, path, at.Add(-time.Minute))
			if status == http.StatusNotFound {
				var got *github.StatusError
				if !errors.As(err, &got) || got.StatusCode != status || reopened.Data.Accepted == nil ||
					reopened.Data.Accepted.AttemptID != accepted.AttemptID || !reopened.Data.Accepted.Deadline.Equal(accepted.Deadline) ||
					!reopened.Data.Accepted.RetryAt.After(time.Now()) || len(reopened.Data.Processed) != 0 {
					t.Fatalf("ambiguous repository 404 lost accepted retry: err=%v state=%+v", err, reopened.Data)
				}
				return
			}
			if _, done := reopened.Data.Processed[accepted.JobID]; err != nil || !done || reopened.Data.Accepted != nil ||
				len(reopened.Data.JobFailures) != 0 {
				t.Fatalf("moved or gone repository retained accepted action: err=%v state=%+v", err, reopened.Data)
			}
		})
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

func TestSelectedTerminalRecoverySkipsRemoteLookupAfterLocalRevocation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Monitor)
	}{
		{"repository removed", func(m *Monitor) { m.Config.Repositories[0].Name = "other/repo" }},
		{"action disabled", func(m *Monitor) { m.Config.Repositories[0].Actions = []string{"fix"} }},
		{"actor removed", func(m *Monitor) {
			m.Config.Repositories[0].AllowedActors = []string{"other"}
			m.Config.Repositories[0].AllowedActorIDs = map[string]int64{"other": 9}
		}},
		{"command removed", func(m *Monitor) { delete(m.Config.Commands, "review") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			at := time.Now().UTC()
			accepted := selectedAcceptance(t, at)
			accepted.Assigned = true
			store := openState(t, at.Add(-time.Minute))
			store.Data.Accepted = &accepted
			store.Data.JobFailures[accepted.JobID] = state.JobFailure{
				Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
			}
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			gh := &fakeGitHub{beforeVerify: func(context.Context, bool) error {
				t.Fatal("remote reply lookup preceded local eligibility check")
				return nil
			}}
			m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
				t.Fatal("terminal action redispatched")
				return nil
			}))
			test.change(m)
			if _, err := m.RunOnce(t.Context(), store); err != nil || store.Data.Accepted != nil ||
				len(store.Data.Processed) != 1 || len(store.Data.JobFailures) != 0 || len(gh.comments) != 0 {
				t.Fatalf("locally ineligible action blocked recovery: err=%v state=%+v", err, store.Data)
			}
		})
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

func TestSelectedTerminalRecoveryFinishesAfterTransientLookup(t *testing.T) {
	at := time.Now().UTC()
	accepted := selectedAcceptance(t, at)
	accepted.Assigned = true
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	lookupErr := errors.New("source lookup unavailable")
	gh := &fakeGitHub{items: []github.Item{selectedItem(at)}, permission: github.Permission{Permission: "write"},
		currentItem: func(github.Item) (github.Item, error) { return github.Item{}, lookupErr }}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
		t.Fatal("terminal action redispatched")
		return nil
	}))
	if _, err := m.RunOnce(t.Context(), store); err != nil || store.Data.Accepted != nil ||
		store.Data.Processed[accepted.JobID].IsZero() || len(gh.comments) != 0 || len(store.Data.PublicationWrites) != 0 {
		t.Fatalf("transient terminal lookup did not finish without POST: err=%v state=%+v", err, store.Data)
	}
	lookups := gh.currentChecks
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	gh.currentItem = func(github.Item) (github.Item, error) { return selectedItem(at), nil }
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || gh.currentChecks != lookups ||
		reopened.Data.Processed[accepted.JobID].IsZero() || len(gh.comments) != 0 {
		t.Fatalf("restart rediscovered skipped reply: err=%v state=%+v", err, reopened.Data)
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
		Author: github.User{Login: "linkdataminion", ID: 8}, UpdatedAt: at}
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

func TestSelectedTerminalRestartWithUnknownSourceTimeFindsExistingReply(t *testing.T) {
	at := time.Now().UTC()
	accepted := selectedAcceptance(t, at)
	accepted.SourceUpdatedAt = time.Time{}
	accepted.Assigned = true
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	reply := github.Item{Key: "review:published", NodeID: "reply-node", Body: "Complete.\n\n" + (Job{ID: accepted.JobID}).Marker(),
		Author: github.User{Login: "linkdataminion", ID: 8}, UpdatedAt: at.Add(-time.Minute)}
	gh := &fakeGitHub{reviewReplies: []github.Item{reply}, permission: github.Permission{Permission: "write"},
		currentItem: func(seed github.Item) (github.Item, error) {
			if seed.Key == reply.Key {
				return reply, nil
			}
			return selectedItem(time.Time{}), nil
		}}
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
		t.Fatal("terminal action redispatched")
		return nil
	}))
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || len(gh.verifiedSince) != 1 ||
		!gh.verifiedSince[0].IsZero() || gh.commentCalls != 0 || reopened.Data.Accepted != nil || len(reopened.Data.Processed) != 1 {
		t.Fatalf("unknown source time missed existing reply: err=%v since=%v comments=%d state=%+v",
			err, gh.verifiedSince, gh.commentCalls, reopened.Data)
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

func TestSelectedThirdAttemptExpiryKeepsOutcomeAfterUncertainReply(t *testing.T) {
	at := time.Now().UTC().Add(-9 * time.Minute)
	accepted := selectedAcceptance(t, at)
	now := accepted.Deadline.Add(time.Second)
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
	m := selectedMonitor(t, now, gh, selectedHandler(func(Job) error {
		t.Fatal("expired reservation dispatched work")
		return nil
	}))
	if _, err := m.RunOnce(t.Context(), store); err != nil || store.Data.Accepted != nil ||
		store.Data.Processed[accepted.JobID].IsZero() ||
		store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWritePending ||
		store.Data.TerminalDecisions[accepted.JobID].Outcome != "expired" {
		t.Fatalf("terminal outcome or uncertain reply was lost: err=%v state=%+v", err, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	if reopened.Data.Accepted != nil || reopened.Data.Processed[accepted.JobID].IsZero() ||
		reopened.Data.TerminalDecisions[accepted.JobID].Outcome != "expired" {
		t.Fatalf("terminal outcome was lost after restart: %+v", reopened.Data)
	}
	gh.commentErr = nil
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || gh.commentCalls != 1 || len(gh.comments) != 0 {
		t.Fatalf("restart repeated uncertain reply: err=%v state=%+v", err, reopened.Data)
	}
}

func TestSelectedThirdAttemptTerminalFailureDoesNotRedispatchAfterRestart(t *testing.T) {
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
	replyErr := errors.New("failure reply unavailable")
	gh := &fakeGitHub{permission: github.Permission{Permission: "write"}, commentErr: replyErr,
		currentItem: func(github.Item) (github.Item, error) { return selectedItem(at), nil }}
	var executions int
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error {
		executions++
		return errors.Join(ErrTerminalJob, errors.New("preassignment failure"))
	}))
	if _, err := m.RunOnce(t.Context(), store); err != nil || executions != 1 ||
		store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() ||
		store.Data.TerminalDecisions[accepted.JobID].Outcome != "failed" ||
		store.Data.PublicationWrites[accepted.JobID].Status != state.PublicationWritePending || len(gh.comments) != 0 {
		t.Fatalf("terminal preassignment failure lost its outcome: err=%v executions=%d state=%+v", err, executions, store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStateAt(t, path, at.Add(-time.Minute))
	if reopened.Data.Accepted != nil || reopened.Data.Processed[accepted.JobID].IsZero() ||
		reopened.Data.TerminalDecisions[accepted.JobID].Outcome != "failed" {
		t.Fatalf("terminal outcome was not durable: %+v", reopened.Data)
	}
	gh.commentErr = nil
	m.Handler = selectedHandler(func(Job) error {
		t.Fatal("terminal attempt redispatched after restart")
		return nil
	})
	if _, err := m.RunOnce(t.Context(), reopened); err != nil || executions != 1 ||
		gh.commentCalls != 1 || len(gh.comments) != 0 || reopened.Data.Accepted != nil {
		t.Fatalf("terminal recovery repeated work or reply: err=%v executions=%d state=%+v", err, executions, reopened.Data)
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
