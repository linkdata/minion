package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/workroot"
	"github.com/linkdata/minion/internal/workspace"
)

func savedRecoveryFixture(t *testing.T, job Job, result Result) (*state.Store, string, monitor.Job) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	accepted := acceptedForJob(job)
	accepted.Assigned = true
	store.Data.Accepted = accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	record := AcceptedResultRecord{Accepted: *accepted, Job: job, Result: result}
	if job.Action == command.Fix && len(result.Changes) != 0 {
		record.FixDestination = &FixDestination{RepositoryID: job.Identity.RepositoryID, DefaultBranch: "main", Visibility: "public", ForkID: 4, ForkFullName: "bot/r"}
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("invalid saved recovery fixture: %v", err)
	}
	data, err := httpEncode(record, int(AcceptedResultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAcceptedResult(data); err != nil {
		t.Fatal(err)
	}
	accepted.ResultReady = true
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	request := monitor.Job{
		Repository:  config.Repository{Name: accepted.Repository, ID: accepted.RepositoryID, OwnerID: accepted.OwnerID},
		SubjectType: accepted.SubjectType, Number: accepted.SubjectNumber,
		Source: github.Item{Key: accepted.SourceKey, NodeID: accepted.SourceNodeID, HTMLURL: accepted.SourceURL,
			UpdatedAt: accepted.SourceUpdatedAt, Author: github.User{ID: accepted.RequesterID, Login: accepted.Requester}},
		Command: command.Command{Name: accepted.CommandName, Action: job.Action},
		ID:      accepted.JobID, AttemptID: accepted.AttemptID, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
	}
	return store, path, request
}

func completedRecoveryResult(job Job) Result {
	return Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Saved complete answer",
		Calls: []CallUsage{{Provider: job.Main.Provider, Model: job.Main.Model, Effort: job.Main.Effort, Role: roleMain, Round: 1, Complete: true}}}
}

func TestPublishRecoveredRequiresGitHubClient(t *testing.T) {
	job := testJob(command.Comment)
	store, _, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	if err := (&Orchestrator{Store: store}).PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrAcceptanceState) || !strings.Contains(err.Error(), "GitHub client is missing") {
		t.Fatalf("missing GitHub client did not stop recovery with state intact: %v", err)
	}
}

func TestPublishRecoveredRejectsUncommittedSidecar(t *testing.T) {
	job := testJob(command.Comment)
	store, _, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	store.Data.Accepted.ResultReady = false
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("test sidecar: present=%t error=%v", present, err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	e := &Orchestrator{Store: store, GitHub: testGitHub(server, "token")}
	if err := e.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrAcceptanceState) || requests.Load() != 0 {
		t.Fatalf("uncommitted sidecar was publishable: err=%v requests=%d", err, requests.Load())
	}
	if saved, err := e.hasSavedResult(job); err != nil || saved {
		t.Fatalf("uncommitted sidecar authorized retry: saved=%t error=%v", saved, err)
	}
}

func TestPublishRecoveredRejectsExpiredOriginalDeadline(t *testing.T) {
	job := testJob(command.Comment)
	store, _, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	data, err := store.ReadAcceptedResult(AcceptedResultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	record, err := DecodeAcceptedResultRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	request.Deadline = time.Now().Add(-time.Minute)
	request.AcceptedAt = request.Deadline.Add(-time.Minute)
	store.Data.Accepted.Deadline, store.Data.Accepted.AcceptedAt = request.Deadline, request.AcceptedAt
	record.Accepted = *store.Data.Accepted
	data, err = httpEncode(record, int(AcceptedResultMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAcceptedResult(data); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Errorf("expired recovery reached GitHub: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	e := &Orchestrator{Store: store, GitHub: testGitHub(server, "token")}
	if err := e.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrTerminalJob) ||
		!errors.Is(err, context.DeadlineExceeded) || requests.Load() != 0 {
		t.Fatalf("expired recovery was publishable: err=%v requests=%d", err, requests.Load())
	}
}

func TestPublishRecoveredReconcilesPostedMarkerWithoutModelOrSecondWrite(t *testing.T) {
	job := testJob(command.Comment)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var reads, writes atomic.Int32
	var rootChecks int
	markerBody := "Completed before restart\n\n" + request.Marker()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && !(r.Method == http.MethodPost && r.URL.Path == "/graphql") {
			writes.Add(1)
			t.Errorf("recovered marker caused a second write: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		switch r.URL.Path {
		case "/repos/owner/repo/issues/7/comments":
			_, _ = fmt.Fprintf(w, `[{"id":1,"node_id":"reply-node","body":%q,"user":{"login":"linkdataminion"},"updated_at":%q}]`, markerBody, request.Source.UpdatedAt.Add(time.Second).UTC().Format(time.RFC3339))
		case "/graphql":
			_, _ = fmt.Fprintf(w, `{"data":{"node":{"body":%q,"author":{"login":"linkdataminion"}}}}`, markerBody)
		default:
			t.Errorf("unexpected recovery read: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "linkdataminion", WorkRoot: t.TempDir(),
		Runner: Runner{runModel: forbidModel(t)}, CheckWorkRoot: func() error { rootChecks++; return workroot.ErrUnsafe }}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || reads.Load() != 2 || writes.Load() != 0 || rootChecks != 0 {
		t.Fatalf("posted marker was not reconciled: reads=%d writes=%d root_checks=%d error=%v", reads.Load(), writes.Load(), rootChecks, err)
	}
	if decision := store.Data.TerminalDecisions[request.ID]; decision.Outcome != "succeeded" || !store.Data.Accepted.Terminal {
		t.Fatalf("reconciled publication did not record success before monitor cleanup: %+v", store.Data)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("reconciliation removed result before monitor state save: present=%t error=%v", present, err)
	}
}

func TestSelectedExecuteAlreadyAcknowledgedPersistsSuccessWithoutWorker(t *testing.T) {
	job := testJob(command.Comment)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	request.Repository.Actions = []string{"comment"}
	markerBody := "Completed before restart\n\n" + request.Marker()
	var reads, writes, executions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && !(r.Method == http.MethodPost && r.URL.Path == "/graphql") {
			writes.Add(1)
			t.Errorf("already acknowledged action wrote again: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		switch r.URL.Path {
		case "/repos/owner/repo/issues/7/comments":
			_, _ = fmt.Fprintf(w, `[{"id":1,"node_id":"reply-node","body":%q,"user":{"login":"linkdataminion"},"updated_at":%q}]`, markerBody, request.Source.UpdatedAt.Add(time.Second).UTC().Format(time.RFC3339))
		case "/graphql":
			_, _ = fmt.Fprintf(w, `{"data":{"node":{"body":%q,"author":{"login":"linkdataminion"}}}}`, markerBody)
		default:
			t.Errorf("unexpected read: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	e := &Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "linkdataminion",
		assignmentRequired: true,
		Commands:           map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
		Executor: executorFunc(func(context.Context, Job, Checkpoints) (Result, error) {
			executions.Add(1)
			return Result{}, errors.New("already acknowledged action ran worker")
		}),
	}
	if err := e.Execute(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 2 || writes.Load() != 0 || executions.Load() != 0 ||
		store.Data.TerminalDecisions[request.ID].Outcome != "succeeded" {
		t.Fatalf("reconciled action ran again or lacked success: reads=%d writes=%d executions=%d state=%+v",
			reads.Load(), writes.Load(), executions.Load(), store.Data)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Data.TerminalDecisions[request.ID].Outcome != "succeeded" {
		t.Fatalf("reconciled success was not durable: %+v", reopened.Data.TerminalDecisions)
	}
}

func TestPublishRecoveredReviewReconcilesPostedReview(t *testing.T) {
	job := testJob(command.Review)
	store, _, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	type postedReview struct{ body, commit string }
	var posted atomic.Pointer[postedReview]
	var timelineReads, provenanceReads, reviewWrites atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var input struct{ Query string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			if strings.Contains(input.Query, "timelineItems") {
				timelineReads.Add(1)
				var nodes []map[string]any
				if review := posted.Load(); review != nil {
					nodes = []map[string]any{{"id": "review-node", "fullDatabaseId": "1", "body": review.body,
						"author": map[string]string{"login": "bot"}, "state": "COMMENTED", "submittedAt": request.Source.UpdatedAt.Add(time.Second).UTC().Format(time.RFC3339)}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
					"pullRequest": map[string]any{"timelineItems": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}}}}})
				return
			}
			provenanceReads.Add(1)
			review := posted.Load()
			if review == nil {
				t.Error("review provenance checked before publication")
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"node": map[string]any{
				"body": review.body, "author": map[string]string{"login": "bot"}}}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"owner/repo","owner":{"id":2,"login":"owner"},"default_branch":"main","visibility":"public"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls/7/reviews":
			var input struct {
				Body   string `json:"body"`
				Commit string `json:"commit_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			reviewWrites.Add(1)
			posted.Store(&postedReview{body: input.Body, commit: input.Commit})
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			t.Errorf("unexpected review recovery request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", BotID: 8, WorkRoot: t.TempDir(),
		Runner: Runner{runModel: forbidModel(t)}, ValidatePublication: permitSelectedPublication}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	// A separate state file models restart before the first publication decision was saved.
	retryStore, _, retryRequest := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	orchestrator.Store = retryStore
	if err := orchestrator.PublishRecovered(t.Context(), retryRequest); err != nil {
		t.Fatal(err)
	}
	review := posted.Load()
	if review == nil || review.commit != job.Source.Head || !strings.HasSuffix(review.body, "\n\n"+request.Marker()) ||
		reviewWrites.Load() != 1 || timelineReads.Load() != 2 || provenanceReads.Load() != 1 {
		t.Fatalf("recovered review was not reconciled: review=%+v writes=%d timelines=%d provenance=%d", review,
			reviewWrites.Load(), timelineReads.Load(), provenanceReads.Load())
	}
}

func TestPublishRecoveredChecksWorkRootBeforeNewPublication(t *testing.T) {
	job := testJob(command.Comment)
	store, _, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	var reads, writes atomic.Int32
	var rootChecks int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/issues/7/comments" {
			reads.Add(1)
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		writes.Add(1)
		t.Errorf("unsafe work root allowed publication: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: t.TempDir(),
		CheckWorkRoot: func() error { rootChecks++; return workroot.ErrUnsafe }}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, workroot.ErrUnsafe) || errors.Is(err, monitor.ErrTerminalJob) || reads.Load() != 1 || writes.Load() != 0 || rootChecks != 1 {
		t.Fatalf("unsafe work root did not stop new publication: reads=%d writes=%d root_checks=%d error=%v", reads.Load(), writes.Load(), rootChecks, err)
	}
}

func TestPublishRecoveredBoundsEachAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := testJob(command.Comment)
		store, _, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/issues/7/comments" {
				t.Errorf("unexpected recovery request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
				return
			}
			<-r.Context().Done()
		}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), MaxRuntime: 20 * time.Millisecond}
		started := time.Now()
		if err := orchestrator.PublishRecovered(ctx, request); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) >= time.Second {
			t.Fatalf("recovery did not stop at its attempt budget: elapsed=%s error=%v", time.Since(started), err)
		}
		if present, err := store.HasAcceptedResult(); err != nil || !present {
			t.Fatalf("bounded recovery lost its saved result: present=%t error=%v", present, err)
		}
	})
}

func TestPublishRecoveredRejectsTamperedResultSidecarBeforeGitHub(t *testing.T) {
	job := testJob(command.Comment)
	store, path, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	data, err := os.ReadFile(path + ".result")
	if err != nil {
		t.Fatal(err)
	}
	var record AcceptedResultRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.Result.Identity.JobID = "foreign-job"
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".result", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Errorf("tampered result reached GitHub: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: t.TempDir()}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrAcceptanceState) || !strings.Contains(err.Error(), "decode accepted result") || requests.Load() != 0 {
		t.Fatalf("tampered sidecar was published: requests=%d error=%v", requests.Load(), err)
	}
}

func TestPublishRecoveredNoChangeFixRepliesWithoutFork(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	job.Identity.Repository = "o/r"
	store, _, request := savedRecoveryFixture(t, job, completedRecoveryResult(job))
	var comment string
	var replies atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = fmt.Fprint(w, `{"node_id":"issue-node"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var query struct{ Query string }
			if err := json.NewDecoder(r.Body).Decode(&query); err != nil || query.Query != `query($id: ID!) { node(id: $id) { ... on Issue { title bodyText lastEditedAt } } }` {
				t.Errorf("unexpected issue lookup: query=%q error=%v", query.Query, err)
				http.Error(w, "unexpected query", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, `{"data":{"node":{"title":"Task","bodyText":"Inspect source","lastEditedAt":null}}}`)
		case captureComment(t, w, r, &comment):
			replies.Add(1)
		default:
			t.Errorf("no-change fix requested fork or branch: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", BotID: 8, WorkRoot: t.TempDir(),
		Runner: Runner{runModel: forbidModel(t)}, ValidatePublication: permitSelectedPublication}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || replies.Load() != 1 ||
		!strings.Contains(comment, "AI returned a response without changing files.") || !strings.Contains(comment, "Saved complete answer") ||
		!strings.HasSuffix(comment, request.Marker()) {
		t.Fatalf("saved no-change fix was not published: replies=%d comment=%q error=%v", replies.Load(), comment, err)
	}
}

func TestPublishRecoveredNoChangeFixStillValidatesPublication(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	job.Identity.Repository = "o/r"
	result := completedRecoveryResult(job)
	result.Answer = "configured-secret"
	store, _, request := savedRecoveryFixture(t, job, result)
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments" {
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7" {
			_, _ = fmt.Fprint(w, `{"node_id":"issue-node"}`)
			return
		}
		writes.Add(1)
		t.Errorf("invalid no-change result reached GitHub publication: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: t.TempDir(),
		Runner: Runner{Codex: config.CodexConfig{APIKey: "configured-secret"}, runModel: forbidModel(t)}}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrTerminalJob) ||
		!strings.Contains(err.Error(), "configured provider key") || writes.Load() != 0 {
		t.Fatalf("invalid no-change result was published: writes=%d error=%v", writes.Load(), err)
	}
}

func TestPublishRecoveredFixUsesPinnedSourceAfterDefaultMoves(t *testing.T) {
	base, _, remote := gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("before\n")})
	root := t.TempDir()
	checkout, err := gitwork.Clone(t.Context(), root, "o/r", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := checkout.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkout.Close(); err != nil {
		t.Fatal(err)
	}
	if snapshot.Head != base {
		t.Fatalf("pinned source is not original base: got=%s want=%s", snapshot.Head, base)
	}
	source := filepath.Join(filepath.Dir(remote), "source")
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("new default tip\n"), 0600); err != nil {
		t.Fatal(err)
	}
	modelGit(t, source, "add", "source.txt")
	modelGit(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "move default")
	modelGit(t, source, "push", remote, "HEAD:refs/heads/main")
	moved := strings.TrimSpace(modelGit(t, source, "rev-parse", "HEAD"))
	forkRemote := filepath.Join(filepath.Dir(remote), "fork.git")
	modelGit(t, source, "clone", "--bare", remote, forkRemote)
	modelGit(t, source, "config", "--file", filepath.Join(filepath.Dir(remote), "gitconfig"), "url."+forkRemote+".insteadOf", "https://github.com/bot/r.git")
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	job.Identity.Repository = "o/r"
	job.Source = snapshot
	result := completedRecoveryResult(job)
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("reviewed change\n")}}
	store, _, request := savedRecoveryFixture(t, job, result)
	var comment string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case serveFixFlowFixture(t, w, r):
		case captureComment(t, w, r, &comment):
		default:
			t.Errorf("unexpected recovered fix request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", BotID: 8, WorkRoot: root,
		Runner: Runner{runModel: forbidModel(t)}, ValidatePublication: permitSelectedPublication}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	branch := "refs/heads/minion/fix-7-aaaaaaaaaaaa"
	tip := strings.TrimSpace(modelGit(t, source, "--git-dir="+forkRemote, "rev-parse", branch))
	parent := strings.TrimSpace(modelGit(t, source, "--git-dir="+forkRemote, "rev-parse", branch+"^"))
	content := strings.TrimSpace(modelGit(t, source, "--git-dir="+forkRemote, "show", branch+":source.txt"))
	if parent != base || parent == moved || content != "reviewed change" || store.Data.PendingFixes[request.ID].SHA != tip ||
		!strings.Contains(comment, request.Marker()) {
		t.Fatalf("replayed fix used moved default: tip=%s parent=%s base=%s moved=%s content=%q checkpoint=%+v comment=%q", tip, parent, base, moved, content, store.Data.PendingFixes[request.ID], comment)
	}
}

func TestPublishRecoveredFixRevalidatesBeforePush(t *testing.T) {
	_, _, remote := gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("before\n")})
	root := t.TempDir()
	checkout, err := gitwork.Clone(t.Context(), root, "o/r", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := checkout.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkout.Close(); err != nil {
		t.Fatal(err)
	}
	forkRemote := filepath.Join(filepath.Dir(remote), "fork.git")
	source := filepath.Join(filepath.Dir(remote), "source")
	modelGit(t, source, "clone", "--bare", remote, forkRemote)
	modelGit(t, source, "config", "--file", filepath.Join(filepath.Dir(remote), "gitconfig"), "url."+forkRemote+".insteadOf", "https://github.com/bot/r.git")
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	job.Identity.Repository = "o/r"
	job.Source = snapshot
	result := completedRecoveryResult(job)
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("after\n")}}
	store, _, request := savedRecoveryFixture(t, job, result)
	var comment string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case serveFixFlowFixture(t, w, r):
		case captureComment(t, w, r, &comment):
		default:
			t.Errorf("unexpected fix publication request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	denied := errors.New("requester no longer has permission")
	var checks int
	e := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: root,
		ValidatePublication: func(_ context.Context, got monitor.Job, requireAssigned bool) error {
			checks++
			if got.ID != request.ID || !requireAssigned {
				t.Errorf("fix push checked the wrong reservation: job=%+v assigned=%t", got, requireAssigned)
			}
			return denied
		}}
	if err := e.PublishRecovered(t.Context(), request); !errors.Is(err, denied) || checks != 1 || comment != "" {
		t.Fatalf("rejected fix reached publication: err=%v checks=%d comment=%q", err, checks, comment)
	}
	if output, err := testsupport.ModelGit(t, source, "--git-dir="+forkRemote, "rev-parse", "refs/heads/minion/fix-7-aaaaaaaaaaaa"); err == nil {
		t.Fatalf("rejected fix reached fork remote: %s", output)
	}
}

func TestPublishRecoveredFixReconcilesLostPushResponseWithoutSecondPush(t *testing.T) {
	_, _, remote := gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("before\n")})
	root := t.TempDir()
	checkout, err := gitwork.Clone(t.Context(), root, "o/r", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := checkout.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkout.Close(); err != nil {
		t.Fatal(err)
	}
	forkRemote := filepath.Join(filepath.Dir(remote), "fork.git")
	source := filepath.Join(filepath.Dir(remote), "source")
	modelGit(t, source, "clone", "--bare", remote, forkRemote)
	gitConfig := filepath.Join(filepath.Dir(remote), "gitconfig")
	modelGit(t, source, "config", "--file", gitConfig, "url."+forkRemote+".insteadOf", "https://github.com/bot/r.git")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	pushCount := filepath.Join(t.TempDir(), "push-count")
	fakeOnPath(t, "git", fmt.Sprintf("#!/bin/sh\nset -eu\ncase \" $* \" in *\" push --no-verify \"*) printf x >> %s; %s -c %s \"$@\"; exit 1 ;; esac\nexec %s -c %s \"$@\"\n",
		shellQuote(pushCount), shellQuote(realGit), shellQuote("include.path="+gitConfig), shellQuote(realGit), shellQuote("include.path="+gitConfig)))

	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	job.Identity.Repository = "o/r"
	job.Source = snapshot
	result := completedRecoveryResult(job)
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("reviewed change\n")}}
	store, path, request := savedRecoveryFixture(t, job, result)
	branch := "minion/fix-7-aaaaaaaaaaaa"
	var comment string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/bot/r/branches/"+branch:
			sha, err := exec.Command(realGit, "--git-dir="+forkRemote, "rev-parse", "refs/heads/"+branch).Output()
			if err != nil {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprintf(w, `{"name":%q,"commit":{"sha":%q}}`, branch, strings.TrimSpace(string(sha)))
		case serveFixFlowFixture(t, w, r):
		case captureComment(t, w, r, &comment):
		default:
			t.Errorf("unexpected recovered fix request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", BotID: 8, WorkRoot: root,
		Runner: Runner{runModel: forbidModel(t)}, ValidatePublication: permitSelectedPublication}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) || comment != "" {
		t.Fatalf("lost push response was treated as completed: comment=%q error=%v", comment, err)
	}
	issued := store.Data.PublicationWrites[fixPushWriteKey(request.ID)]
	recorded := store.Data.PendingFixes[request.ID].SHA
	if issued.Status != state.PublicationWritePending || issued.Repository != "bot/r" || issued.Branch != branch || issued.SHA != recorded ||
		issued.AttemptID != request.AttemptID || !workspace.ValidSHA(recorded) {
		t.Fatalf("lost push response lacks exact durable checkpoint: push=%+v pending=%+v", issued, store.Data.PendingFixes[request.ID])
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	orchestrator.Store = store
	t.Setenv("PATH", t.TempDir()) // Recovery must not run Git or issue another push.
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil {
		t.Fatalf("accepted remote push was not reconciled after restart: %v", err)
	}
	count, err := os.ReadFile(pushCount)
	if err != nil || string(count) != "x" || !strings.Contains(comment, recorded) ||
		store.Data.PublicationWrites[fixPushWriteKey(request.ID)].Status != state.PublicationWriteObserved {
		t.Fatalf("recovery pushed twice or lost branch reply: pushes=%q comment=%q write=%+v error=%v", count, comment,
			store.Data.PublicationWrites[fixPushWriteKey(request.ID)], err)
	}
}

func TestPublishRecoveredFixKeepsCheckpointAfterMissingBranchAndGitFailure(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	job.Identity.Repository = "o/r"
	result := completedRecoveryResult(job)
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("after\n")}}
	store, path, request := savedRecoveryFixture(t, job, result)
	sha := strings.Repeat("b", 40)
	store.Data.PendingFixes[request.ID] = state.PendingFix{SHA: sha}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var comment string
	var replies, branchLookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case serveSafeForkChecks(w, r, "bot/r", false):
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = fmt.Fprint(w, `{"node_id":"issue-node"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/bot/r":
			_, _ = fmt.Fprint(w, `{"id":4,"full_name":"bot/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"visibility":"public"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/bot/r/branches/minion/fix-7-aaaaaaaaaaaa":
			if branchLookups.Add(1) == 1 {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprintf(w, `{"name":"minion/fix-7-aaaaaaaaaaaa","commit":{"sha":%q}}`, sha)
		case captureComment(t, w, r, &comment):
			replies.Add(1)
		default:
			t.Errorf("unexpected branch reuse request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("PATH", t.TempDir()) // A recorded SHA must never invoke Git for another push.
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", BotID: 8, WorkRoot: t.TempDir(),
		Runner: Runner{runModel: forbidModel(t)}, ValidatePublication: permitSelectedPublication}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrPostModelPublication) || !strings.Contains(err.Error(), "no matching remote branch") ||
		replies.Load() != 0 || branchLookups.Load() != 1 || store.Data.PendingFixes[request.ID].SHA != sha {
		t.Fatalf("missing branch lookup lost checkpoint: replies=%d checkpoint=%+v error=%v", replies.Load(), store.Data.PendingFixes[request.ID], err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	orchestrator.Store = store
	wrong := store.Data.PendingFixes[request.ID]
	wrong.Cap = &state.CriticCap{MaxRounds: 1, HasBlockers: true}
	store.Data.PendingFixes[request.ID] = wrong
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrAcceptanceState) ||
		replies.Load() != 0 || branchLookups.Load() != 2 || store.Data.PendingFixes[request.ID] != wrong {
		t.Fatalf("matching branch reused with different review decision: lookups=%d replies=%d checkpoint=%+v error=%v", branchLookups.Load(), replies.Load(), store.Data.PendingFixes[request.ID], err)
	}
	wrong.Cap = nil
	store.Data.PendingFixes[request.ID] = wrong
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || replies.Load() != 1 ||
		branchLookups.Load() != 3 || !strings.Contains(comment, sha) || !strings.HasSuffix(comment, request.Marker()) || store.Data.PendingFixes[request.ID].SHA != sha {
		t.Fatalf("matching checkpoint was not reused: branch_lookups=%d replies=%d comment=%q checkpoint=%+v error=%v", branchLookups.Load(), replies.Load(), comment, store.Data.PendingFixes[request.ID], err)
	}
}

func TestPublishRecoveredFixChecksGitBeforeRestoringSource(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	job.Identity.Repository = "o/r"
	result := completedRecoveryResult(job)
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("after\n")}}
	store, _, request := savedRecoveryFixture(t, job, result)
	fakeOnPath(t, "git", "#!/bin/sh\nprintf 'git version 2.44.0\\n'\n")
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case serveSafeForkChecks(w, r, "bot/r", false):
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = fmt.Fprint(w, `{"node_id":"issue-node"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/bot/r":
			_, _ = fmt.Fprint(w, `{"id":4,"full_name":"bot/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"visibility":"public"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/bot/r/branches/minion/fix-7-aaaaaaaaaaaa":
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected recovered fix request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	e := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: t.TempDir(),
		Runner: Runner{runModel: forbidModel(t)}, ValidatePublication: permitSelectedPublication}
	if err := e.PublishRecovered(t.Context(), request); err == nil || errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "Git 2.45.1 or later") || writes.Load() != 0 {
		t.Fatalf("unchecked Git reached recovery publication: writes=%d error=%v", writes.Load(), err)
	}
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("Git preflight discarded saved result: present=%t error=%v", present, err)
	}
}

func TestPublishRecoveredFixRejectsUnrecordedBranchSHA(t *testing.T) {
	job := testJob(command.Fix)
	job.Identity.JobID = strings.Repeat("a", 64)
	job.Identity.Repository = "o/r"
	result := completedRecoveryResult(job)
	result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("after\n")}}
	store, _, request := savedRecoveryFixture(t, job, result)
	store.Data.PendingFixes[request.ID] = state.PendingFix{SHA: strings.Repeat("b", 40)}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case serveSafeForkChecks(w, r, "bot/r", false):
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, publicRepositoryJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = fmt.Fprint(w, `{"node_id":"issue-node"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/bot/r":
			_, _ = fmt.Fprint(w, `{"id":4,"full_name":"bot/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"visibility":"public"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/bot/r/branches/minion/fix-7-aaaaaaaaaaaa":
			_, _ = fmt.Fprintf(w, `{"name":"minion/fix-7-aaaaaaaaaaaa","commit":{"sha":%q}}`, strings.Repeat("c", 40))
		default:
			t.Errorf("unexpected mismatched branch request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: t.TempDir(),
		Runner: Runner{runModel: forbidModel(t)}, ValidatePublication: permitSelectedPublication}
	err := orchestrator.PublishRecovered(t.Context(), request)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "without a matching recorded bot commit") || writes.Load() != 0 {
		t.Fatalf("foreign branch was reused: writes=%d error=%v", writes.Load(), err)
	}
}

func TestPublishRecoveredFixRejectsDestinationDrift(t *testing.T) {
	for _, test := range []struct {
		name          string
		defaultBranch string
		forkID        int64
		private       bool
		visibility    string
	}{
		{name: "default branch", defaultBranch: "release", forkID: 4, visibility: "public"},
		{name: "fork identity", defaultBranch: "main", forkID: 5, visibility: "public"},
		{name: "visibility", defaultBranch: "main", forkID: 4, private: true, visibility: "private"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := testJob(command.Fix)
			job.Identity.JobID = strings.Repeat("a", 64)
			job.Identity.Repository = "o/r"
			result := completedRecoveryResult(job)
			result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("after\n")}}
			store, _, request := savedRecoveryFixture(t, job, result)
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
					_, _ = fmt.Fprint(w, `[]`)
				case serveSafeForkChecks(w, r, "bot/r", test.private):
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
					_, _ = fmt.Fprintf(w, `{"id":1,"full_name":"o/r","owner":{"id":2,"login":"o"},"default_branch":%q,"private":%t,"visibility":%q}`, test.defaultBranch, test.private, test.visibility)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/bot/r":
					_, _ = fmt.Fprintf(w, `{"id":%d,"full_name":"bot/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"private":%t,"visibility":%q}`, test.forkID, test.private, test.visibility)
				case serveFixFlowFixture(t, w, r):
				default:
					t.Errorf("unexpected request after destination drift: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			t.Setenv("PATH", t.TempDir()) // A destination mismatch must stop before Git publication.
			orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: t.TempDir(),
				Runner: Runner{runModel: forbidModel(t)}, ValidatePublication: permitSelectedPublication}
			err := orchestrator.PublishRecovered(t.Context(), request)
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "fix destination changed since accepted result") || writes.Load() != 0 {
				t.Fatalf("changed destination was used: writes=%d error=%v", writes.Load(), err)
			}
			if present, err := store.HasAcceptedResult(); err != nil || !present {
				t.Fatalf("direct publisher cleared receipt before monitor handled terminal error: present=%t error=%v", present, err)
			}
		})
	}
}
