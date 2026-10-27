package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/action"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

func TestStartupPublishesStoredResultBeforeSelectedWorkerPreflight(t *testing.T) {
	isolateHostPaths(t, t.TempDir())
	markers := startupCommandMarkers(t)
	t.Setenv("GH_TOKEN", "offline-token")
	for _, name := range []string{"GITHUB_TOKEN", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(name, "")
	}

	root := t.TempDir()
	statePath := filepath.Join(root, "state.json")
	at := time.Now().UTC().Add(-time.Minute)
	accepted := state.AcceptedAction{
		JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "local",
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
		SubjectType: "Issue", SubjectNumber: 7,
		SourceKey: "issue-comment:42", SourceNodeID: "source-node",
		SourceURL: "https://github.com/owner/repo/issues/7#issuecomment-42", SourceUpdatedAt: at,
		Requester: "writer", RequesterID: 7, CommandName: "comment", Action: "comment",
		AcceptedAt: at, Deadline: at.Add(time.Hour), Assigned: true,
	}
	job := action.Job{
		Version: action.ProtocolVersion,
		Identity: action.Identity{JobID: accepted.JobID, AttemptID: accepted.AttemptID, ExecutorID: accepted.ExecutorID,
			Repository: accepted.Repository, RepositoryID: accepted.RepositoryID, OwnerID: accepted.OwnerID,
			Requester: accepted.Requester, RequesterID: accepted.RequesterID, Number: accepted.SubjectNumber, SubjectType: accepted.SubjectType},
		Command: accepted.CommandName, Action: command.Comment,
		Source: workspace.Snapshot{Head: strings.Repeat("a", 40), Repository: []byte("saved source bundle")},
		Main:   action.RoleSettings{Provider: "codex", Skills: []string{skills.BuiltinAntiPromptInjection}}, RuntimeBudget: time.Minute,
	}
	result := action.Result{Version: action.ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Answer: "Saved answer", Calls: []action.CallUsage{{Provider: "codex", Role: "main", Round: 1, Complete: true}}}
	record := action.AcceptedResultRecord{Accepted: accepted, Job: job, Result: result}
	if err := record.Validate(); err != nil {
		t.Fatalf("invalid saved result fixture: %v", err)
	}
	store, err := state.Open(statePath, at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
	store.Data.NextPollAt = at.Add(time.Hour)
	store.Data.NextInviteCheckAt = at.Add(time.Hour)
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAcceptedResult(data); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var requests []string
	var reply string
	publications := 0
	previousTransport := http.DefaultTransport
	http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
		key := request.Method + " " + request.URL.Path
		requests = append(requests, key)
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Content-Type", "application/json")
		switch key {
		case "GET /user":
			_, _ = recorder.WriteString(`{"id":8,"login":"bot"}`)
		case "GET /repos/owner/repo":
			_, _ = recorder.WriteString(`{"id":1,"full_name":"owner/repo","default_branch":"main","owner":{"id":2}}`)
		case "GET /repos/owner/repo/issues/7":
			_, _ = recorder.WriteString(`{"number":7}`)
		case "POST /graphql":
			_, _ = recorder.WriteString(`{"data":{"node":{"body":"@bot comment","author":{"id":7,"login":"writer"}}}}`)
		case "GET /repos/owner/repo/collaborators/writer/permission":
			_, _ = recorder.WriteString(`{"permission":"write","user":{"id":7,"login":"writer"}}`)
		case "GET /repos/owner/repo/issues/7/comments":
			_, _ = recorder.WriteString(`[]`)
		case "POST /repos/owner/repo/issues/7/comments":
			publications++
			var input struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			reply = input.Body
			recorder.WriteHeader(http.StatusCreated)
			_, _ = recorder.WriteString(`{"id":99}`)
		default:
			t.Errorf("unexpected GitHub request: %s", key)
			return nil, errors.New("unexpected GitHub request")
		}
		return recorder.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	cfg := config.Config{BotLogin: "bot", StatePath: statePath, WorkRoot: root,
		Commands:       map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
		AlwaysOnSkills: []string{},
		Repositories: []config.Repository{{Name: "owner/repo", ID: 1, OwnerID: 2, Actions: []string{"comment"},
			AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7}}},
	}
	gitChecks := 0
	code, output := runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error {
			gitChecks++
			return errors.New("unexpected Git preflight")
		})
	}, "-mode=orchestrate")
	if code != 0 || gitChecks != 0 || strings.Contains(output, "preflight:") || strings.Contains(output, "start selected worker:") {
		t.Fatalf("stored result recovery ran preflight or worker: exit=%d git_checks=%d log=%q", code, gitChecks, output)
	}
	assertNoStartupCommands(t, markers)
	if publications != 1 || !strings.Contains(reply, "Saved answer") || !strings.HasSuffix(reply, "<!-- linkdataminion:job-1 -->") {
		t.Fatalf("stored result was not published once: requests=%v reply=%q", requests, reply)
	}
	store, err = state.Open(statePath, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	present, err := store.HasAcceptedResult()
	if err != nil || present || store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || len(store.Data.JobFailures) != 0 {
		t.Fatalf("recovered result was not durably completed: present=%t err=%v state=%+v", present, err, store.Data)
	}
}
