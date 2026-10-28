package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
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
		JobID: "job-1", AttemptID: "attempt-1", ExecutorID: action.LocalExecutorID,
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

	var mu sync.Mutex
	var requests []string
	var reply string
	publications := 0
	previousTransport := http.DefaultTransport
	http.DefaultTransport = startupTransport(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
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
	snapshot := func() ([]string, string, int) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests), reply, publications
	}

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
	observedRequests, observedReply, observedPublications := snapshot()
	if observedPublications != 1 || !strings.Contains(observedReply, "Saved answer") || !strings.HasSuffix(observedReply, "<!-- linkdataminion:job-1 -->") {
		t.Fatalf("stored result was not published once: requests=%v reply=%q", observedRequests, observedReply)
	}
	graphql := slices.Index(observedRequests, "POST /graphql")
	permission := slices.Index(observedRequests, "GET /repos/owner/repo/collaborators/writer/permission")
	publish := slices.Index(observedRequests, "POST /repos/owner/repo/issues/7/comments")
	if graphql < 0 || permission < 0 || publish < 0 || graphql >= publish || permission >= publish {
		t.Fatalf("recovery skipped source or permission revalidation before publication: requests=%v", observedRequests)
	}
	store, err = state.Open(statePath, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	present, err := store.HasAcceptedResult()
	if err != nil || present || store.Data.Accepted != nil || store.Data.Processed[accepted.JobID].IsZero() || len(store.Data.JobFailures) != 0 {
		t.Fatalf("recovered result was not durably completed: present=%t err=%v state=%+v", present, err, store.Data)
	}
	if runtime.GOOS == "windows" {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		return // The next preflight uses a POSIX shell CLI fixture.
	}

	// Repeat recovery with -loop. Its later preflight resolves the default
	// model; the worker startup must use that resolved selection.
	store.Data.Accepted = &accepted
	store.Data.JobFailures[accepted.JobID] = state.JobFailure{Attempts: 1, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline}
	delete(store.Data.Processed, accepted.JobID)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAcceptedResult(data); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Main = "claude"
	for _, name := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		t.Setenv(name, "")
	}
	claude := `#!/bin/sh
case " $* " in
  *' auth status --json '*) printf '%s\n' '{"loggedIn":true}'; exit 0 ;;
  *' --minion-unsupported-capability-probe '*) printf '%s\n' 'unknown option --minion-unsupported-capability-probe' >&2; exit 2 ;;
  *' --resume '*)
    next=false
    for arg do
      if $next; then id=$arg; break; fi
      [ "$arg" != --resume ] || next=true
    done
    printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0}}'
    printf 'No conversation found with session ID: %s\n' "$id" >&2
    exit 1 ;;
esac
input=$(/bin/cat)
case "$input" in *minion-initialize*) printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"minion-initialize","response":{"models":[{"value":"default","resolvedModel":"claude-test"}]}}}' ;; *) exit 90 ;; esac
`
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "claude"), []byte(claude), 0700); err != nil {
		t.Fatal(err)
	}
	code, output = runStartupFixtureWithRun(t, cfg, func() int {
		return runWithGitCheck(func(context.Context, string) error {
			gitChecks++
			mu.Lock()
			requests = append(requests, "git preflight")
			mu.Unlock()
			return nil
		})
	}, "-mode=orchestrate", "-loop")
	if code != 1 || gitChecks != 1 || !strings.Contains(output, "Claude settings: model=claude-test") ||
		!strings.Contains(output, "start selected worker:") || strings.Contains(output, "worker main selection requires a resolved model") {
		t.Fatalf("loop did not use resolved service config after recovery: exit=%d git_checks=%d log=%q", code, gitChecks, output)
	}
	observedRequests, _, observedPublications = snapshot()
	firstPublish := slices.Index(observedRequests, "POST /repos/owner/repo/issues/7/comments")
	secondPublish := -1
	if firstPublish >= 0 {
		if next := slices.Index(observedRequests[firstPublish+1:], "POST /repos/owner/repo/issues/7/comments"); next >= 0 {
			secondPublish = firstPublish + 1 + next
		}
	}
	preflight := slices.Index(observedRequests, "git preflight")
	if observedPublications != 2 || secondPublish < 0 || preflight < 0 || secondPublish >= preflight {
		t.Fatalf("loop did not recover the saved result before preflight: requests=%v publications=%d", observedRequests, observedPublications)
	}
}
