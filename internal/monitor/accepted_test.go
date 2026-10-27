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
}

func (g *reactionProbeGitHub) ReactEyes(ctx context.Context, nodeID string) error {
	if g.beforeReaction != nil {
		g.beforeReaction()
	}
	return g.fakeGitHub.ReactEyes(ctx, nodeID)
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

func TestSelectedReactionFailureReleasesUnusedAcceptance(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := selectedItem(at)
	reactionErr := errors.New("reaction unavailable")
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}, reactionErr: reactionErr}
	store := openState(t, at.Add(-time.Minute))
	m := selectedMonitor(t, at, gh, selectedHandler(func(Job) error { t.Fatal("reaction failure dispatched work"); return nil }))
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, reactionErr) {
		t.Fatalf("reaction error = %v", err)
	}
	if store.Data.Accepted != nil || len(store.Data.JobFailures) != 0 || len(store.Data.Processed) != 0 {
		t.Fatalf("unused acceptance remained reserved: %+v", store.Data)
	}
}

func TestSelectedPreparationFailureReleasesUnusedAcceptance(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := selectedItem(at)
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}}
	store := openState(t, at.Add(-time.Minute))
	preparationErr := errors.New("preparation unavailable")
	m := selectedMonitor(t, at, gh, selectedHandler(func(job Job) error {
		if store.Data.Accepted == nil || store.Data.Accepted.JobID != job.ID || store.Data.Accepted.Assigned {
			t.Fatalf("preparation did not see its unassigned reservation: %+v", store.Data.Accepted)
		}
		return preparationErr
	}))
	if _, err := m.RunOnce(t.Context(), store); !errors.Is(err, preparationErr) {
		t.Fatalf("preparation error = %v", err)
	}
	if store.Data.Accepted != nil || len(store.Data.JobFailures) != 0 || len(store.Data.Processed) != 0 {
		t.Fatalf("unused preparation retained acceptance: %+v", store.Data)
	}
}

func TestSelectedAssignedFailureIsTerminal(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	item := selectedItem(at)
	gh := &fakeGitHub{notifications: []github.Notification{testNotification(at)}, items: []github.Item{item},
		permission: github.Permission{Permission: "write"}}
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
	if _, err := m.RunOnce(t.Context(), store); err != nil || executions != 1 || len(gh.comments) != 1 || store.Data.Accepted != nil {
		t.Fatalf("recovery did not finish held reply: err=%v executions=%d comments=%+v state=%+v", err, executions, gh.comments, store.Data)
	}
}

func TestSelectedUnassignedRecoveryReleasesBeforeCapacity(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStateAt(t, path, at.Add(-time.Minute))
	accepted := selectedAcceptance(t, at)
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 2}
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
	if reopened.Data.Accepted != nil || reopened.Data.JobFailures[accepted.JobID].Attempts != 1 || len(handler.jobs) != 0 {
		t.Fatalf("unassigned recovery did not release exactly one attempt: %+v", reopened.Data)
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
	gh := &fakeGitHub{beforeList: func() { t.Fatal("invitation scan preceded recovery") }}
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
	gh := &fakeGitHub{}
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
	if _, err := m.RunOnce(t.Context(), store); err == nil || len(gh.comments) != 0 || store.Data.Accepted == nil {
		t.Fatalf("repository mismatch published or released accepted action: err=%v state=%+v", err, store.Data)
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
