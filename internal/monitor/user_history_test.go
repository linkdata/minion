package monitor

import (
	"errors"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

func TestUnusedSelectedAttemptRetainsHistoryIdentityUntilResolution(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	store := openState(t, at)
	accepted := &state.AcceptedAction{JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "local",
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2, SubjectType: "Issue", SubjectNumber: 7,
		SourceKey: "source", SourceNodeID: "source-node", SourceURL: "https://github.com/owner/repo/issues/7",
		Requester: "writer", RequesterID: 3, CommandName: "comment", Action: "comment", MainProvider: "codex",
		MainSettings: config.RoleModelConfig{Model: "o3", Effort: "high"}, AcceptedAt: at, Deadline: at.Add(time.Hour)}
	store.Data.Accepted = accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at, Deadline: accepted.Deadline,
		MainProvider: accepted.MainProvider, MainSettings: accepted.MainSettings}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	job := Job{ID: accepted.JobID, AttemptID: accepted.AttemptID, AcceptedAt: at, Deadline: accepted.Deadline,
		Repository: config.Repository{Name: accepted.Repository, ID: 1, OwnerID: 2}, SubjectType: "Issue", Number: 7,
		Source: github.Item{Key: accepted.SourceKey, NodeID: accepted.SourceNodeID, HTMLURL: accepted.SourceURL,
			Author: github.User{Login: accepted.Requester, ID: accepted.RequesterID}},
		Command: command.Command{Name: "comment", Action: command.Comment}, MainSettings: accepted.MainSettings}
	m := &Monitor{Config: config.Config{RemoteExecutor: &config.RemoteExecutorConfig{}}}
	if err := m.completeSelected(t.Context(), store, job, at.Add(time.Minute), errors.New("preparation unavailable")); err == nil {
		t.Fatal("unused attempt did not report preparation failure")
	}
	saved, err := store.ReadSavedDurable()
	if err != nil {
		t.Fatal(err)
	}
	retry := saved.JobFailures[job.ID].RetryAction
	if saved.Accepted != nil || retry.AttemptID != accepted.AttemptID || retry.Summary.RequesterID != 3 ||
		retry.Summary.RepositoryID != 1 || retry.Summary.OwnerID != 2 ||
		retry.Summary.SourceKey != accepted.SourceKey || retry.Summary.SourceNodeID != accepted.SourceNodeID ||
		retry.Summary.SourceURL != accepted.SourceURL ||
		retry.Summary.MainSettings != accepted.MainSettings {
		t.Fatalf("released attempt history lost: accepted=%+v retry=%+v", saved.Accepted, retry)
	}
	if err := store.WithLock(func() error {
		_, err := store.DecideRetryTerminalLocked(job.ID, "failed", at.Add(2*time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	saved, err = store.ReadSavedDurable()
	if err != nil || saved.TerminalDecisions[job.ID].AttemptID != accepted.AttemptID ||
		saved.TerminalDecisions[job.ID].Summary != retry.Summary {
		t.Fatalf("released retry terminal decision: decision=%+v err=%v", saved.TerminalDecisions[job.ID], err)
	}
}

func TestIgnoredRetryKeepsTerminalHistory(t *testing.T) {
	at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	store := openState(t, at)
	accepted := &state.AcceptedAction{JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "local",
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2, SubjectType: "Issue", SubjectNumber: 7,
		SourceKey: "source", SourceNodeID: "source-node", Requester: "writer", RequesterID: 3,
		CommandName: "comment", Action: "comment", MainProvider: "codex",
		MainSettings: config.RoleModelConfig{Model: "o3", Effort: "high"}, AcceptedAt: at,
		Deadline: at.Add(time.Hour)}
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: at,
		Deadline: accepted.Deadline, MainProvider: "codex", MainSettings: accepted.MainSettings,
		RetryAction: state.RetryAction{AttemptID: accepted.AttemptID, Summary: accepted.HistorySummary()}}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := retireIgnoredJob(store, accepted.JobID, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	saved, err := store.ReadSavedDurable()
	if err != nil || saved.Processed[accepted.JobID].IsZero() ||
		saved.JobFailures[accepted.JobID] != (state.JobFailure{}) ||
		saved.TerminalDecisions[accepted.JobID].AttemptID != accepted.AttemptID ||
		saved.TerminalDecisions[accepted.JobID].Summary != accepted.HistorySummary() ||
		saved.TerminalDecisions[accepted.JobID].Outcome != "expired" ||
		!saved.TerminalDecisions[accepted.JobID].DecidedAt.After(accepted.Deadline) {
		t.Fatalf("ignored retry history: state=%+v err=%v", saved, err)
	}
}
