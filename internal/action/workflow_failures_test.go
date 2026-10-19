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
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestReplyFailureClassificationBeforeAndAfterModel(t *testing.T) {
	for _, kind := range []string{"model result", "recovery", "fixed reply"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/repos/o/r" {
					_, _ = fmt.Fprint(w, publicRepositoryJSON)
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
			store := openState(t, time.Now())
			client := testGitHub(server, "fixture-token")
			request := usageJob()
			request.Source = github.Item{}
			request.Command = command.Command{Action: command.Fix}
			prepared := &preparedAction{request: request, repository: github.Repository{FullName: "o/r", DefaultBranch: "main"}, fork: github.Repository{FullName: "bot/r"}}
			orchestrator := Orchestrator{GitHub: client, Store: store, BotLogin: "bot"}
			var err error
			switch kind {
			case "fixed reply":
				prepared.reply = "Review needs a pull request."
				err = orchestrator.publish(context.Background(), prepared, Result{})
			case "recovery":
				prepared.branch = "minion/fix-recorded"
				store.Data.PendingFixes[request.ID] = strings.Repeat("b", 40)
				err = orchestrator.publish(context.Background(), prepared, Result{})
			default:
				err = orchestrator.publishFix(context.Background(), prepared, Result{Answer: "No changes."})
			}
			var status *github.StatusError
			if errors.Is(err, monitor.ErrPostModelPublication) != (kind != "fixed reply") || errors.Is(err, monitor.ErrTerminalJob) || !errors.As(err, &status) || status.StatusCode != http.StatusBadGateway {
				t.Fatalf("reply lost publication classification or cause: %v", err)
			}
		})
	}
}

func TestExecuteSourceFailuresAndInvalidIncompleteResultsAreTerminal(t *testing.T) {
	for _, scenario := range []string{"oversized source", "unsupported source", "invalid incomplete result", "output limit", "too many files", "diff too large"} {
		t.Run(scenario, func(t *testing.T) {
			gitFixtureRemote(t)
			installActionTestSkills(t, testReviewSkillURL)
			calls, writes := 0, 0
			modelErr := errors.New("worker failed with invalid usage")
			// Inject native/CommitAndPush causes to isolate Execute terminal classification.
			limitCause := map[string]error{"output limit": aicli.ErrOutputLimit, "too many files": gitwork.ErrTooManyFiles, "diff too large": gitwork.ErrDiffTooLarge}[scenario]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method != http.MethodGet:
					writes++
					t.Errorf("unexpected write %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
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
				case serveIssueFixture(t, w, r):
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := testGitHub(server, "token")
			orchestrator := Orchestrator{WorkRoot: t.TempDir(), GitHub: client, BotLogin: "bot", Commands: map[string]config.CommandMapping{"audit": {Action: "comment", Skills: []string{testReviewSkillURL}}}, Runner: Runner{runModel: func(context.Context, modelRun) (aicli.Result, error) {
				calls++
				if limitCause != nil {
					return aicli.Result{}, fmt.Errorf("wrapped native limit: %w", limitCause)
				}
				return aicli.Result{Tokens: usage.Tokens{Input: -1}}, modelErr
			}}}
			request := usageJob()
			request.Repository.Actions = []string{"audit"}
			request.Source.Author.ID = 3
			request.Command = command.Command{Name: "audit", Action: command.Comment}
			err := orchestrator.Execute(context.Background(), request)
			if !errors.Is(err, monitor.ErrTerminalJob) || writes != 0 {
				t.Fatalf("source/result failure not terminal: calls=%d writes=%d err=%v", calls, writes, err)
			}
			wantCalls := 0
			switch scenario {
			case "oversized source":
				if !errors.Is(err, gitwork.ErrCheckoutTooLarge) || errors.Is(err, gitwork.ErrUnsupportedSource) {
					t.Fatalf("source size cause lost: %v", err)
				}
			case "unsupported source":
				if !errors.Is(err, gitwork.ErrUnsupportedSource) || errors.Is(err, gitwork.ErrCheckoutTooLarge) {
					t.Fatalf("unsupported source cause lost: %v", err)
				}
			case "output limit", "too many files", "diff too large":
				wantCalls = 1
				if !errors.Is(err, limitCause) {
					t.Fatalf("terminal native limit lost cause: %v", err)
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

func TestPrepareFixClassifiesUnsupportedLayoutBeforeBranchOrModels(t *testing.T) {
	files := []workspace.File{{Path: "README", Mode: workspace.Regular}, {Path: "readme", Mode: workspace.Regular}}
	gitFixtureRemote(t, files...)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	branched := filepath.Join(t.TempDir(), "branched")
	fakeOnPath(t, "git", fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *\" switch --create \"*) printf created > %s;; esac\nexec %s \"$@\"\n", shellQuote(branched), shellQuote(git)))
	store := openState(t, time.Now())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case serveFixFlowFixture(t, w, r, files...):
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	orchestrator := &Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "token"), BotLogin: "bot", Store: store,
		Commands: map[string]config.CommandMapping{"fix": {Action: "fix", Skills: []string{skills.BuiltinAntiPromptInjection}}},
		Runner:   Runner{runModel: forbidModel(t)},
	}
	request := usageJob()
	request.Repository.Actions = []string{"fix"}
	request.Source.Author.ID = 3
	request.Source.UpdatedAt = time.Now()
	request.Command = command.Command{Name: "fix", Action: command.Fix}
	if err := attemptFixture(orchestrator).perform(testActionContext(t), request); !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, gitwork.ErrUnsupportedSource) || errors.Is(err, gitwork.ErrCheckoutTooLarge) {
		t.Fatalf("unsupported fix layout lost classification: %v", err)
	}
	if _, err := os.Stat(branched); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported layout created a branch: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("unsupported source left a checkout: %v, %v", entries, err)
	}
}

func TestPrepareRetainsCheckoutForRetryAfterCleanupFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	base, _, _ := gitFixtureRemote(t)
	root := t.TempDir()
	t.Cleanup(func() {
		if err := os.Chmod(root, 0700); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/") {
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || !entries[0].IsDir() || !strings.HasPrefix(entries[0].Name(), "minion-checkout-") {
				t.Errorf("metadata requested without isolated checkout: entries=%v error=%v", entries, err)
				http.Error(w, "missing fixture checkout", http.StatusInternalServerError)
				return
			}
			// Snapshot work can still use the checkout; only removing its parent entry fails.
			if err := os.Chmod(root, 0500); err != nil {
				t.Error(err)
				http.Error(w, "cannot restrict fixture parent", http.StatusInternalServerError)
				return
			}
			writeTreeFixture(t, w, strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/trees/"))
			return
		}
		if !serveIssueFixture(t, w, r) {
			t.Errorf("unexpected preparation request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	request := usageJob()
	request.Command = command.Command{Name: "inspect", Action: command.Comment}
	var journal bytes.Buffer
	orchestrator := attemptFixture(&Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "token"), Logger: log.New(&journal, "", 0),
		Commands: map[string]config.CommandMapping{"inspect": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
	})
	prepared, err := orchestrator.prepare(testActionContext(t), request)
	if err != nil || prepared == nil || prepared.checkout == nil || !prepared.needsExecution() {
		t.Fatalf("cleanup failure discarded prepared work or retry handle: prepared=%+v error=%v", prepared, err)
	}
	if err := prepared.job.Validate(); err != nil || prepared.job.Source.Head != base || len(prepared.job.Source.Repository) == 0 {
		t.Fatalf("cleanup failure invalidated portable job: source=%s error=%v", prepared.job.Source.Head, err)
	}
	if !strings.Contains(journal.String(), "prepared checkout cleanup failed; final retry scheduled:") || !strings.Contains(journal.String(), "remove checkout:") || !strings.Contains(journal.String(), "permission denied") {
		t.Fatalf("prepare cleanup failure was not exercised/logged: %s", &journal)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "minion-checkout-") {
		t.Fatalf("failed cleanup did not retain checkout entry: entries=%v error=%v", entries, err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepared.checkout.Close(); err != nil {
		t.Fatalf("retained checkout could not retry cleanup: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("successful cleanup retry left owned files: entries=%v error=%v", entries, err)
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
	store := openState(t, time.Now())
	request := usageJob()
	request.Repository.Actions = []string{"fix"}
	request.Source.Author.ID = 3
	request.Source.UpdatedAt = time.Now()
	request.Command = command.Command{Name: "fix", Action: command.Fix}
	comments, modelCalls := 0, 0
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = fmt.Fprint(w, `[]`)
		case serveFixFlowFixture(t, w, r):
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
		Runner: Runner{runModel: func(context.Context, modelRun) (aicli.Result, error) {
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

func TestPublishFixRevalidatesForkAndImportBeforePush(t *testing.T) {
	for _, stage := range []string{"import", "commit", "fork changed", "canceled import"} {
		t.Run(stage, func(t *testing.T) {
			base, _, _ := gitFixtureRemote(t)
			git, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			seen := filepath.Join(bin, "staged")
			publication := filepath.Join(bin, "publication")
			script := fmt.Sprintf(`#!/bin/sh
case " $* " in
 *" add --all "*)
  if test %s = 'canceled import'; then touch %s; sleep 30; exit 1; fi
  ;;
 *" diff --cached --raw "*)
  if test %s = import; then touch %s; exit 0; fi
  ;;
 *" diff --cached --name-only "*)
  if test %s = commit; then touch %s; exit 0; fi
  ;;
 *" commit --no-verify "*|*" push --no-verify "*)
  touch %s; exit 73
  ;;
esac
exec %s "$@"
`, shellQuote(stage), shellQuote(seen), shellQuote(stage), shellQuote(seen), shellQuote(stage), shellQuote(seen), shellQuote(publication), shellQuote(git))
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
				if serveSafeForkChecks(w, r, "bot/r", false) {
					return
				}
				switch r.URL.Path {
				case "/repos/o/r":
					_, _ = fmt.Fprint(w, publicRepositoryJSON)
				case "/repos/bot/r":
					currentFork := fork
					if stage == "fork changed" {
						currentFork.ID++
					}
					_ = json.NewEncoder(w).Encode(currentFork)
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := testGitHub(server, "token")
			root := t.TempDir()
			checkout := cloneFixture(t, root)
			const branch = "minion/fix-reviewed"
			if err := checkout.NewBranch(context.Background(), branch); err != nil {
				t.Fatal(err)
			}
			fakeOnPath(t, "git", script)
			job := testJob(command.Fix)
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
				return
			}
			if !errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, monitor.ErrPostModelPublication) || writes != 0 || (stage == "commit" && !errors.Is(err, gitwork.ErrNoChanges)) || (stage == "import" && !strings.Contains(err.Error(), "staged paths do not match reviewed changes")) {
				t.Fatalf("dropped %s changes: writes=%d err=%v", stage, writes, err)
			}
			if _, err := os.Stat(seen); err != nil {
				t.Fatalf("fixture did not drop the %s diff: %v", stage, err)
			}
			if _, err := os.Stat(publication); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("dropped %s changes reached commit or push: %v", stage, err)
			}
		})
	}
}

func TestCompletedAuditRetentionFailureIsTerminal(t *testing.T) {
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
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
	orchestrator := Orchestrator{GitHub: client, WorkRoot: blocked}
	prepared := &preparedAction{job: job, request: monitor.Job{Command: command.Command{Action: command.SecurityAudit}}}
	err := orchestrator.publish(context.Background(), prepared, result)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "retain completed audit") || writes != 0 {
		t.Fatalf("audit retention failure retried work or published: writes=%d err=%v", writes, err)
	}
}

func TestCanceledPublicationRetainsValidatedAudit(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprintf("valid=%t", valid), func(t *testing.T) {
			job := testJob(command.SecurityAudit)
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
			wantPrefix := "incomplete-"
			if valid {
				wantPrefix = "complete-"
			}
			if err != nil || len(dirs) != 1 || !strings.HasPrefix(dirs[0].Name(), wantPrefix) {
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
			job := testJob(action)
			job.Source.Omitted = []string{"docs"}
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
		})
	}
}
