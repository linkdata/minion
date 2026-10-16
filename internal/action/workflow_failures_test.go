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
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestExecuteSourceFailuresAndInvalidIncompleteResultsAreTerminal(t *testing.T) {
	for _, scenario := range []string{"oversized source", "unsupported source", "invalid incomplete result"} {
		t.Run(scenario, func(t *testing.T) {
			auditGitFixtureRemote(t)
			installActionTestSkills(t, testReviewSkillURL)
			calls, writes := 0, 0
			modelErr := errors.New("worker failed with invalid usage")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method != http.MethodGet:
					writes++
					t.Errorf("unexpected write %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				case r.URL.Path == "/repos/o/r/issues/7/comments":
					fmt.Fprint(w, `[]`)
				case r.URL.Path == "/repos/o/r":
					fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2},"default_branch":"main"}`)
				case r.URL.Path == "/repos/o/r/issues/7":
					fmt.Fprint(w, `{"title":"Task","body":"Inspect source"}`)
				case strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
					sha := strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/")
					switch scenario {
					case "oversized source":
						fmt.Fprintf(w, `{"sha":%q,"truncated":true,"tree":[]}`, sha)
					case "unsupported source":
						fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[{"path":"source","type":"blob","mode":"100600","size":0}]}`, sha)
					default:
						fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[]}`, sha)
					}
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := github.New("token")
			client.BaseURL, client.HTTPClient = server.URL, server.Client()
			orchestrator := Orchestrator{WorkRoot: t.TempDir(), GitHub: client, BotLogin: "bot", Commands: map[string]config.CommandMapping{"audit": {Action: "comment", Skills: []string{testReviewSkillURL}}}, Runner: Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
				calls++
				return aicli.Result{Tokens: usage.Tokens{Input: -1}}, modelErr
			}}}
			request := monitor.Job{ID: strings.Repeat("a", 64), SubjectType: "Issue", Number: 7, Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"audit"}}, Source: github.Item{Author: github.User{ID: 3, Login: "writer"}}, Command: command.Command{Name: "audit", Action: command.Comment}}
			err := orchestrator.Execute(context.Background(), request)
			if !errors.Is(err, monitor.ErrTerminalJob) || writes != 0 {
				t.Fatalf("source/result failure not terminal: calls=%d writes=%d err=%v", calls, writes, err)
			}
			wantCalls := 0
			switch scenario {
			case "oversized source":
				if !errors.Is(err, gitwork.ErrCheckoutTooLarge) {
					t.Fatalf("source size cause lost: %v", err)
				}
			case "unsupported source":
				if !errors.Is(err, gitwork.ErrUnsupportedSource) {
					t.Fatalf("unsupported source cause lost: %v", err)
				}
			default:
				wantCalls = 1
				if !errors.Is(err, modelErr) || !strings.Contains(err.Error(), "invalid incomplete action result") {
					t.Fatalf("incomplete worker cause lost: %v", err)
				}
			}
			if calls != wantCalls {
				t.Fatalf("model calls=%d want%d", calls, wantCalls)
			}
		})
	}
}

func TestPublishFixRejectsNonemptyReviewedChangesDroppedByGit(t *testing.T) {
	for _, stage := range []string{"import", "commit"} {
		t.Run(stage, func(t *testing.T) {
			base, _, _ := auditGitFixtureRemote(t)
			git, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			seen := filepath.Join(bin, "staged")
			script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *\" diff --cached --name-only \"*)\n if test %q = import || test -f %q; then exit 0; fi\n touch %q\n ;;\nesac\nexec %q \"$@\"\n", stage, seen, seen, git)
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			writes := 0
			repo := github.Repository{ID: 1, FullName: "o/r", Owner: github.User{ID: 2}, DefaultBranch: "main", Visibility: "public"}
			fork := github.Repository{ID: 3, FullName: "bot/r", Fork: true, Parent: &repo, Source: &repo, DefaultBranch: "main", Visibility: "public"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					t.Errorf("unexpected publication %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				switch r.URL.Path {
				case "/repos/o/r":
					fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2},"default_branch":"main","visibility":"public"}`)
				case "/repos/bot/r":
					fmt.Fprint(w, `{"id":3,"full_name":"bot/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"default_branch":"main","visibility":"public"}`)
				case "/repos/bot/r/actions/permissions":
					fmt.Fprint(w, `{"enabled":false}`)
				case "/repos/o/r/rulesets", "/repos/bot/r/rulesets":
					fmt.Fprint(w, `[]`)
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := github.New("token")
			client.BaseURL, client.HTTPClient = server.URL, server.Client()
			root := t.TempDir()
			checkout, err := gitwork.Clone(context.Background(), root, "o/r", "main", func(_ context.Context, sha string) (github.GitTree, error) {
				truncated := false
				return github.GitTree{SHA: sha, Truncated: &truncated, Tree: []github.GitTreeEntry{}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer checkout.Close()
			const branch = "minion/fix-reviewed"
			if err := checkout.NewBranch(context.Background(), branch); err != nil {
				t.Fatal(err)
			}
			job := portableJob(command.Fix)
			job.Source.Head = base
			prepared := &preparedAction{Job: job, checkout: checkout, repository: repo, fork: fork, branch: branch, request: monitor.Job{ID: strings.Repeat("a", 64), Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, Command: command.Command{Name: "fix", Action: command.Fix}}}
			orchestrator := Orchestrator{GitHub: client, BotLogin: "bot", WorkRoot: root}
			result := Result{Changes: []workspace.Change{{Path: "reviewed.txt", Mode: workspace.Regular, Data: []byte("reviewed change")}}}
			err = orchestrator.publishFix(context.Background(), prepared, result)
			if !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, gitwork.ErrNoChanges) || writes != 0 {
				t.Fatalf("dropped %s changes: writes=%d err=%v", stage, writes, err)
			}
		})
	}
}

func TestCompletedAuditRetentionFailureIsTerminal(t *testing.T) {
	base, _, _ := auditGitFixtureRemote(t)
	checkout, err := gitwork.Clone(context.Background(), t.TempDir(), "o/r", "main", func(_ context.Context, sha string) (github.GitTree, error) {
		truncated := false
		return github.GitTree{SHA: sha, Truncated: &truncated, Tree: []github.GitTreeEntry{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer checkout.Close()
	job := portableJob(command.SecurityAudit)
	job.Source.Head, job.Source.Files = base, nil
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: base, Answer: "Complete report"}
	metadata := []byte(fmt.Sprintf(`{"run_id":"test-run","repo":%q,"target":%q,"source_ref":%q,"profile":"standard","scope_paths":["."],"budget":null,"selected_companion_files":[],"prior_run_paths":[],"shared_file_owners":%s,"execution_policy":"sandboxed-source-and-local-only","run_status":"complete"}`, job.Identity.Repository, sourceIdentity(job), base, auditTestOwners))
	for _, name := range auditArtifacts {
		data := []byte("content")
		if name == "run-metadata.json" {
			data = metadata
		}
		result.Artifacts = append(result.Artifacts, workspace.File{Path: name, Mode: workspace.Regular, Data: data})
	}
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes++
		t.Errorf("publication continued after retention failure: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := github.New("token")
	client.BaseURL, client.HTTPClient = server.URL, server.Client()
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	orchestrator := Orchestrator{GitHub: client, WorkRoot: blocked, Runner: Runner{WorkRoot: t.TempDir()}}
	prepared := &preparedAction{Job: job, checkout: checkout, request: monitor.Job{Command: command.Command{Action: command.SecurityAudit}}}
	err = orchestrator.publish(context.Background(), prepared, result)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "retain completed audit") || writes != 0 {
		t.Fatalf("audit retention failure retried work or published: writes=%d err=%v", writes, err)
	}
}

func TestContextDescribesOmittedRevisionScope(t *testing.T) {
	for _, action := range []command.Action{command.Comment, command.Review} {
		t.Run(string(action), func(t *testing.T) {
			job := portableJob(action)
			job.Source.Omitted = []string{"docs"}
			if action == command.Review {
				job.Source.Files = []workspace.File{{Path: "docs/x", Mode: workspace.Regular}}
			}
			root := t.TempDir()
			if err := materializeContext(root, job.Source); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, "source.json"))
			if err != nil {
				t.Fatal(err)
			}
			var source struct {
				OmittedScope string `json:"omitted_scope"`
			}
			if err := json.Unmarshal(data, &source); err != nil {
				t.Fatal(err)
			}
			want := "head"
			if action == command.Review {
				want = "head_and_base"
			}
			if source.OmittedScope != want {
				t.Fatalf("omitted scope=%q want%q", source.OmittedScope, want)
			}
			prompt, err := (&Runner{}).actionPrompt(context.Background(), job, "/source", root, "")
			if err != nil || !strings.Contains(prompt, "the omitted list covers both head and base revisions") {
				t.Fatalf("omitted revision context missing: %s,%v", prompt, err)
			}
		})
	}
}
