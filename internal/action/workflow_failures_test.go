package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestFixReplyFailureConsumesPublicationAttempt(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovery=%t", recovery), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/repos/o/r" {
					_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2}}`)
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments" {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			}()
			client := testGitHub(server, "fixture-token")
			request := monitor.Job{ID: strings.Repeat("a", 64), SubjectType: "Issue", Number: 7,
				Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, Command: command.Command{Action: command.Fix}}
			prepared := &preparedAction{request: request, repository: github.Repository{FullName: "o/r", DefaultBranch: "main"}, fork: github.Repository{FullName: "bot/r"}}
			orchestrator := Orchestrator{GitHub: client, Store: store, BotLogin: "bot"}
			if recovery {
				prepared.branch = "minion/fix-recorded"
				store.Data.PendingFixes[request.ID] = strings.Repeat("b", 40)
				err = orchestrator.publish(context.Background(), prepared, Result{})
			} else {
				err = orchestrator.publishFix(context.Background(), prepared, Result{Answer: "No changes."})
			}
			var status *github.StatusError
			if !errors.Is(err, monitor.ErrPostModelPublication) || errors.Is(err, monitor.ErrTerminalJob) || !errors.As(err, &status) || status.StatusCode != http.StatusBadGateway {
				t.Fatalf("reply lost publication classification or cause: %v", err)
			}
		})
	}
}

func TestExecuteSourceFailuresAndInvalidIncompleteResultsAreTerminal(t *testing.T) {
	for _, scenario := range []string{"oversized source", "unsupported source", "invalid incomplete result"} {
		t.Run(scenario, func(t *testing.T) {
			gitFixtureRemote(t)
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
					_, _ = fmt.Fprint(w, `[]`)
				case r.URL.Path == "/repos/o/r":
					_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2},"default_branch":"main"}`)
				case r.URL.Path == "/repos/o/r/issues/7":
					_, _ = fmt.Fprint(w, `{"title":"Task","body":"Inspect source"}`)
				case strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
					sha := strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/")
					switch scenario {
					case "oversized source":
						_, _ = fmt.Fprintf(w, `{"sha":%q,"truncated":true,"tree":[]}`, sha)
					case "unsupported source":
						_, _ = fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[{"path":"source","type":"blob","mode":"100600","size":0}]}`, sha)
					default:
						writeTreeFixture(t, w, sha)
					}
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := testGitHub(server, "token")
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

func TestExecuteLogsCheckoutCleanupFailureAfterSuccessfulPublication(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	gitFixtureRemote(t)
	installActionTestSkills(t, testFixSkillURL)
	root := t.TempDir()
	t.Cleanup(func() {
		if err := os.Chmod(root, 0700); err != nil {
			t.Error(err)
		}
	})
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	request := monitor.Job{ID: strings.Repeat("a", 64), SubjectType: "Issue", Number: 7,
		Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"fix"}},
		Source:     github.Item{UpdatedAt: time.Now(), Author: github.User{ID: 3, Login: "writer"}},
		Command:    command.Command{Name: "fix", Action: command.Fix},
	}
	comments, modelCalls := 0, 0
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2},"default_branch":"main","visibility":"public"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/bot/r":
			_, _ = fmt.Fprint(w, `{"id":4,"full_name":"bot/r","fork":true,"parent":{"id":1,"full_name":"o/r"},"source":{"id":1,"full_name":"o/r"},"visibility":"public"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/bot/r/actions/permissions":
			_, _ = fmt.Fprint(w, `{"enabled":false}`)
		case r.Method == http.MethodGet && (r.URL.Path == "/repos/o/r/pulls" || strings.HasSuffix(r.URL.Path, "/rulesets") || r.URL.Path == "/repos/o/r/issues/7/comments"):
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/bot/r/branches/minion/fix-7-aaaaaaaaaaaa":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = fmt.Fprint(w, `{"node_id":"issue-node","title":"Task","body":"Inspect source"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = fmt.Fprint(w, `{"data":{"node":{"title":"Task","bodyText":"Inspect source","lastEditedAt":null}}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/"):
			writeTreeFixture(t, w, strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/"))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			var payload struct{ Body string }
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			comments++
			body = payload.Body
			// Publication has reached the server. Lock only the checkout's parent;
			// cleanup can restore the checkout itself but cannot remove its entry.
			if err := os.Chmod(root, 0500); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := testGitHub(server, "fixture-token")
	var journal bytes.Buffer
	orchestrator := Orchestrator{WorkRoot: root, GitHub: client, BotLogin: "bot", Store: store, Logger: log.New(&journal, "", 0),
		Commands: map[string]config.CommandMapping{"fix": {Action: "fix", Skills: []string{testFixSkillURL}}},
		Runner: Runner{WorkRoot: t.TempDir(), runModel: func(context.Context, modelRun) (aicli.Result, error) {
			modelCalls++
			return aicli.Result{Answer: "No change is needed."}, nil
		}},
	}
	if err := orchestrator.Execute(context.Background(), request); err != nil {
		t.Fatalf("successful publication became a failed action: %v", err)
	}
	if modelCalls != 1 || comments != 1 || !strings.Contains(body, "No change is needed.") || !strings.HasSuffix(body, request.Marker()) {
		t.Fatalf("publication did not complete: modelCalls=%d comments=%d body=%q", modelCalls, comments, body)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "minion-checkout-") {
		t.Fatalf("fixture did not leave a checkout after failed cleanup: entries=%v error=%v", entries, err)
	}
	if !strings.Contains(journal.String(), "close prepared action:") || !strings.Contains(journal.String(), "remove checkout:") || !strings.Contains(journal.String(), "permission denied") {
		t.Fatalf("checkout cleanup failure was not logged: %s", &journal)
	}
}

func TestPublishFixRejectsNonemptyReviewedChangesDroppedByGit(t *testing.T) {
	for _, stage := range []string{"import", "commit", "fork changed", "canceled import"} {
		t.Run(stage, func(t *testing.T) {
			base, _, _ := gitFixtureRemote(t)
			git, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			seen := filepath.Join(bin, "staged")
			script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *\" add --all \"*)\n if test %q = 'canceled import'; then touch %q; sleep 30; exit 1; fi\n ;;\n *\" diff --cached --name-only \"*)\n if test %q = import || test -f %q; then exit 0; fi\n touch %q\n ;;\nesac\nexec %q \"$@\"\n", stage, seen, stage, seen, seen, git)
			fakeOnPath(t, "git", script)
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
					_, _ = fmt.Fprint(w, `{"id":1,"full_name":"o/r","owner":{"id":2},"default_branch":"main","visibility":"public"}`)
				case "/repos/bot/r":
					currentFork := fork
					if stage == "fork changed" {
						currentFork.ID++
					}
					_ = json.NewEncoder(w).Encode(currentFork)
				case "/repos/bot/r/actions/permissions":
					_, _ = fmt.Fprint(w, `{"enabled":false}`)
				case "/repos/o/r/rulesets", "/repos/bot/r/rulesets":
					_, _ = fmt.Fprint(w, `[]`)
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := testGitHub(server, "token")
			root := t.TempDir()
			checkout, err := gitwork.Clone(context.Background(), root, "o/r", "main", emptyTreeFixture, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := checkout.Close(); err != nil {
					t.Error(err)
				}
			}()
			const branch = "minion/fix-reviewed"
			if err := checkout.NewBranch(context.Background(), branch); err != nil {
				t.Fatal(err)
			}
			job := portableJob(command.Fix)
			job.Source.Head = base
			prepared := &preparedAction{job: job, checkout: checkout, repository: repo, fork: fork, branch: branch, request: monitor.Job{ID: strings.Repeat("a", 64), Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, Command: command.Command{Name: "fix", Action: command.Fix}}}
			orchestrator := Orchestrator{GitHub: client, BotLogin: "bot", WorkRoot: root}
			result := Result{Changes: []workspace.Change{{Path: "reviewed.txt", Mode: workspace.Regular, Data: []byte("reviewed change")}}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if stage == "canceled import" {
				go func() {
					ticker := time.NewTicker(5 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
							if _, err := os.Stat(seen); err == nil {
								cancel()
								return
							}
						}
					}
				}()
			}
			err = orchestrator.publishFix(ctx, prepared, result)
			if stage == "canceled import" {
				if !errors.Is(err, context.Canceled) || errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, monitor.ErrPostModelPublication) || writes != 0 {
					t.Fatalf("interrupted import became terminal: writes=%d err=%v", writes, err)
				}
				return
			}
			if stage == "fork changed" {
				if err == nil || !strings.Contains(err.Error(), "repository or fork changed") || !errors.Is(err, monitor.ErrPostModelPublication) || writes != 0 {
					t.Fatalf("changed fork reached publication: writes=%d err=%v", writes, err)
				}
				if _, statErr := os.Stat(seen); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("changed fork reached import/commit: %v", statErr)
				}
				return
			}
			if !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, monitor.ErrPostModelPublication) || writes != 0 || (stage == "commit" && !errors.Is(err, gitwork.ErrNoChanges)) || (stage == "import" && !strings.Contains(err.Error(), "staged paths do not match reviewed changes")) {
				t.Fatalf("dropped %s changes: writes=%d err=%v", stage, writes, err)
			}
		})
	}
}

func TestCompletedAuditRetentionFailureIsTerminal(t *testing.T) {
	base, _, _ := gitFixtureRemote(t)
	checkout, err := gitwork.Clone(context.Background(), t.TempDir(), "o/r", "main", emptyTreeFixture, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	job := portableJob(command.SecurityAudit)
	job.Source.Head, job.Source.Files = base, nil
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: base, Answer: "content", Calls: []CallUsage{{Provider: job.Main.Provider, Role: "main", Round: 1}}}
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
	client := testGitHub(server, "token")
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	orchestrator := Orchestrator{GitHub: client, WorkRoot: blocked, Runner: Runner{WorkRoot: t.TempDir()}}
	prepared := &preparedAction{job: job, checkout: checkout, request: monitor.Job{Command: command.Command{Action: command.SecurityAudit}}}
	err = orchestrator.publish(context.Background(), prepared, result)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "retain completed audit") || writes != 0 {
		t.Fatalf("audit retention failure retried work or published: writes=%d err=%v", writes, err)
	}
}

func TestCanceledPublicationRetainsValidatedAudit(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprintf("valid=%t", valid), func(t *testing.T) {
			job := portableJob(command.SecurityAudit)
			result := completeAuditResult(job)
			if !valid {
				result.Calls[0].Provider = "unknown"
			}
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writes++
				t.Errorf("canceled audit reached GitHub: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			client := testGitHub(server, "fixture-token")
			var journal bytes.Buffer
			e := Orchestrator{GitHub: client, WorkRoot: t.TempDir(), Logger: log.New(&journal, "", 0)}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := e.publish(ctx, &preparedAction{job: job}, result)
			if valid && !errors.Is(err, context.Canceled) || !valid && !errors.Is(err, monitor.ErrTerminalJob) || writes != 0 {
				t.Fatalf("canceled audit: valid=%t writes=%d err=%v", valid, writes, err)
			}
			dirs, err := os.ReadDir(filepath.Join(e.WorkRoot, "audits"))
			if err != nil || len(dirs) != 1 || strings.HasPrefix(dirs[0].Name(), "incomplete-") == valid {
				t.Fatalf("audit retention: dirs=%v err=%v", dirs, err)
			}
			data, err := os.ReadFile(filepath.Join(e.WorkRoot, "audits", dirs[0].Name(), "REPORT.md"))
			if err != nil || string(data) != result.Answer {
				t.Fatalf("retained report changed: %q %v", data, err)
			}
			if !strings.Contains(journal.String(), "reports:") || strings.Contains(journal.String(), "role=main") != valid {
				t.Fatalf("audit evidence missing or invalid: %s", journal.String())
			}
		})
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
			prompt, err := actionPrompt(context.Background(), job, "/source", root, "", "")
			if err != nil || !strings.Contains(prompt, "the omitted list covers both head and merge-base revisions") {
				t.Fatalf("omitted revision context missing: %s,%v", prompt, err)
			}
		})
	}
}
