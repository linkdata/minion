package action

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

type checkpointOverlapGitHub struct {
	*actionMonitorGitHub
	invitationListed chan struct{}
	releaseSaves     <-chan struct{}
}

func (g *checkpointOverlapGitHub) ListRepositoryInvitations(ctx context.Context) ([]github.Invitation, error) {
	close(g.invitationListed)
	select {
	case <-g.releaseSaves:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (*checkpointOverlapGitHub) Repository(_ context.Context, name string) (github.Repository, error) {
	return github.Repository{ID: 1, FullName: name, DefaultBranch: "main", Owner: github.User{Login: "o", ID: 2}}, nil
}

type checkpointOverlapHandler struct {
	entered chan monitor.Job
	release <-chan struct{}
}

func (*checkpointOverlapHandler) WaitCapacity(context.Context) error { return nil }

func (h *checkpointOverlapHandler) Execute(ctx context.Context, job monitor.Job) error {
	h.entered <- job
	select {
	case <-h.release:
		return monitor.ErrAcceptanceState // Leave both checkpoints available for restart inspection.
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestExecutionBeforeCallOverlapsSelectedInvitationSave(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		statePath := filepath.Join(t.TempDir(), "state.json")
		store, err := state.Open(statePath, now.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
		inviteDue := now.Add(time.Minute)
		store.Data.NextInviteCheckAt = inviteDue
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}

		item := github.Item{Key: "comment:1", NodeID: "node:1", Body: "@linkdataminion review",
			HTMLURL: "https://github.com/o/r/pull/7#issuecomment-1", UpdatedAt: now.Add(-time.Second),
			Author: github.User{Login: "writer", ID: 7}}
		notification := github.Notification{ID: "thread-1", Repository: github.Repository{FullName: "o/r"},
			Subject: github.Subject{Type: "PullRequest", URL: "https://api.github.com/repos/o/r/pulls/7"}, UpdatedAt: item.UpdatedAt}
		cfg := config.Config{BotLogin: "linkdataminion", PollIntervalSeconds: 60, MaxRuntimeMinutes: 7,
			Commands: map[string]config.CommandMapping{"review": {Action: "review", Skills: []string{testReviewSkillURL}}},
			Repositories: []config.Repository{{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"review"},
				AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7}}}}
		releaseSaves := make(chan struct{})
		releaseExecute := make(chan struct{})
		defer func() {
			select {
			case <-releaseSaves:
			default:
				close(releaseSaves)
			}
		}()
		defer func() {
			select {
			case <-releaseExecute:
			default:
				close(releaseExecute)
			}
		}()
		gh := &checkpointOverlapGitHub{actionMonitorGitHub: &actionMonitorGitHub{items: []github.Item{item}, notification: notification},
			invitationListed: make(chan struct{}), releaseSaves: releaseSaves}
		handler := &checkpointOverlapHandler{entered: make(chan monitor.Job, 1), release: releaseExecute}
		m := &monitor.Monitor{Config: cfg, GitHub: gh, Handler: handler,
			SelectedExecutorID: "local", SelectedCommands: map[string]bool{"review": true}}
		runResult := make(chan error, 1)
		go func() { _, err := m.RunOnce(t.Context(), store); runResult <- err }()

		var selected monitor.Job
		select {
		case selected = <-handler.entered:
		case err := <-runResult:
			t.Fatalf("RunOnce returned before Execute: %v", err)
		}
		job := testJob(command.Review)
		job.Identity.JobID, job.Identity.AttemptID = selected.ID, selected.AttemptID
		sink := newExecutionCheckpoints(&Orchestrator{Store: store}, job)
		call := CallCheckpoint{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
			Call: CallUsage{Provider: "codex", Role: roleMain, Round: 1}}
		callReady := make(chan struct{})
		callResult := make(chan error, 1)
		go func() {
			close(callReady)
			<-releaseSaves
			callResult <- sink.BeforeCall(call)
		}()

		time.Sleep(time.Minute)
		synctest.Wait()
		<-gh.invitationListed
		<-callReady
		close(releaseSaves)
		if err := <-callResult; err != nil {
			t.Fatalf("BeforeCall: %v", err)
		}
		synctest.Wait()
		select {
		case err := <-runResult:
			t.Fatalf("RunOnce returned while Execute was held: %v", err)
		default:
		}
		var failure state.JobFailure
		var nextInviteCheckAt time.Time
		if err := store.WithLock(func() error {
			failure = store.Data.JobFailures[selected.ID]
			nextInviteCheckAt = store.Data.NextInviteCheckAt
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !failure.ModelCallStarted || !nextInviteCheckAt.After(inviteDue) {
			t.Fatalf("overlapping saves lost in-memory state: failure=%+v next invitation=%s",
				failure, nextInviteCheckAt)
		}
		data, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		var saved state.Data
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if !saved.JobFailures[selected.ID].ModelCallStarted || !saved.NextInviteCheckAt.Equal(nextInviteCheckAt) {
			t.Fatalf("overlapping saves lost durable state while Execute was held: failure=%+v next invitation=%s",
				saved.JobFailures[selected.ID], saved.NextInviteCheckAt)
		}

		close(releaseExecute)
		if err := <-runResult; !errors.Is(err, monitor.ErrAcceptanceState) {
			t.Fatalf("RunOnce after Execute release: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := state.Open(statePath, now.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := reopened.Close(); err != nil {
				t.Error(err)
			}
		})
		if !reopened.Data.JobFailures[selected.ID].ModelCallStarted ||
			!reopened.Data.NextInviteCheckAt.Equal(nextInviteCheckAt) {
			t.Fatalf("reopen lost overlapping saves: failure=%+v next invitation=%s",
				reopened.Data.JobFailures[selected.ID], reopened.Data.NextInviteCheckAt)
		}
	})
}
