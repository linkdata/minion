package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
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
	if present, err := store.HasAcceptedResult(); err != nil || !present {
		t.Fatalf("reconciliation removed result before monitor state save: present=%t error=%v", present, err)
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
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, workroot.ErrUnsafe) || reads.Load() != 1 || writes.Load() != 0 || rootChecks != 1 {
		t.Fatalf("unsafe work root did not stop new publication: reads=%d writes=%d root_checks=%d error=%v", reads.Load(), writes.Load(), rootChecks, err)
	}
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
	if err := orchestrator.PublishRecovered(t.Context(), request); !errors.Is(err, monitor.ErrAcceptanceState) || requests.Load() != 0 {
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
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: t.TempDir(),
		Runner: Runner{runModel: forbidModel(t)}}
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
	request.Source.UpdatedAt = store.Data.Accepted.SourceUpdatedAt
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
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: root,
		Runner: Runner{runModel: forbidModel(t)}}
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

func TestPublishRecoveredFixKeepsCheckpointAfterMissingBranchLookup(t *testing.T) {
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
	t.Setenv("PATH", t.TempDir()) // An unexpected rebuild needs Git and must fail this test.
	orchestrator := Orchestrator{Store: store, GitHub: testGitHub(server, "token"), BotLogin: "bot", WorkRoot: t.TempDir(),
		Runner: Runner{runModel: forbidModel(t)}}
	if err := orchestrator.PublishRecovered(t.Context(), request); err == nil || !strings.Contains(err.Error(), "refusing to replace the checkpoint") ||
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
	if err := orchestrator.PublishRecovered(t.Context(), request); err != nil || replies.Load() != 1 ||
		branchLookups.Load() != 2 || !strings.Contains(comment, sha) || !strings.HasSuffix(comment, request.Marker()) || store.Data.PendingFixes[request.ID].SHA != sha {
		t.Fatalf("matching checkpoint was not reused: branch_lookups=%d replies=%d comment=%q checkpoint=%+v error=%v", branchLookups.Load(), replies.Load(), comment, store.Data.PendingFixes[request.ID], err)
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
		Runner: Runner{runModel: forbidModel(t)}}
	err := orchestrator.PublishRecovered(t.Context(), request)
	if err == nil || !strings.Contains(err.Error(), "without a matching recorded bot commit") || writes.Load() != 0 {
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
				Runner: Runner{runModel: forbidModel(t)}}
			err := orchestrator.PublishRecovered(t.Context(), request)
			if err == nil || !strings.Contains(err.Error(), "fix destination changed since accepted result") || writes.Load() != 0 {
				t.Fatalf("changed destination was used: writes=%d error=%v", writes.Load(), err)
			}
			if present, err := store.HasAcceptedResult(); err != nil || !present {
				t.Fatalf("destination drift lost accepted result: present=%t error=%v", present, err)
			}
		})
	}
}
