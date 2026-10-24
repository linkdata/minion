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
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestHTTPWorkerDiagnosticsRemainPrivateThroughPublication(t *testing.T) {
	for _, action := range []command.Action{command.Comment, command.Review, command.SecurityAudit, command.Fix} {
		t.Run(string(action), func(t *testing.T) {
			job := testJob(action)
			if action == command.SecurityAudit {
				installActionTestSkills(t, testAuditSkillURL)
				fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
			}
			job.Identity.Repository = "o/r"
			f := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil)
			f.pull(t)
			f.ack(t, "/before", f.call(1), http.StatusOK, 1)
			paid := f.call(2)
			paid.Checkpoint.Call.Complete = true
			f.ack(t, "/after", paid, http.StatusOK, 2)
			outcome := f.outcome(3, nil)
			outcome.Result.Answer = "@linkdataminion fix\n![image](https://example.test/track)\n\x1b[2J\u202e"
			outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
			prefix := "PRIVATE diagnostic\n\x1b[2JFORGED JOURNAL ENTRY"
			outcome.Result.Diagnostics = []Diagnostic{{Code: "SECRET_CODE_123", Message: prefix + strings.Repeat("x", maxDiagnosticMessageBytes-len(prefix))}}
			if len(outcome.Result.Diagnostics[0].Message) != maxDiagnosticMessageBytes {
				t.Fatal("diagnostic fixture is not at its wire limit")
			}
			if action == command.SecurityAudit {
				outcome.Result.Artifacts = auditArtifactsFixture(job, sourceIdentity(job), outcome.Result.Answer)
			}
			if err := outcome.Result.Validate(job); err != nil {
				t.Fatalf("invalid successful worker result: %v", err)
			}
			if action == command.Comment {
				invalid := outcome
				invalid.Result.Diagnostics = []Diagnostic{{Code: "SECRET_CODE_123", Message: outcome.Result.Diagnostics[0].Message + "x"}}
				if got := f.request("/outcome", invalid); got.err != nil || got.status != http.StatusBadRequest {
					t.Fatalf("over-cap worker diagnostic accepted: %+v", got)
				}
			}
			f.ack(t, "/outcome", outcome, http.StatusOK, 3)
			got := f.result(t)
			if got.err != nil || got.result.Validate(job) != nil {
				t.Fatalf("receiver changed valid result: result=%+v error=%v", got.result, got.err)
			}
			var posted string
			writes := 0
			githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case serveIssueFixture(t, w, r):
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					_, _ = fmt.Fprint(w, `{"data":{"node":{"title":"Task","bodyText":"Inspect source","lastEditedAt":null}}}`)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls/7/reviews":
					var payload struct{ Body string }
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					posted = payload.Body
					writes++
					_, _ = fmt.Fprint(w, `{"id":1}`)
				case captureComment(t, w, r, &posted):
					writes++
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer githubServer.Close()
			var journal bytes.Buffer
			owner := Orchestrator{WorkRoot: t.TempDir(), GitHub: testGitHub(githubServer, "fixture-token"), Logger: log.New(&journal, "", 0)}
			request := usageJob()
			request.SubjectType = job.Identity.SubjectType
			request.Source.Author = github.User{ID: job.Identity.RequesterID, Login: job.Identity.Requester}
			request.Source.UpdatedAt = time.Now()
			request.Command = command.Command{Name: string(action), Action: action}
			if err := owner.publish(t.Context(), &preparedAction{job: job, request: request, issueNodeID: "issue-node"}, got.result); err != nil {
				t.Fatal(err)
			}
			_, parseErr := command.ParseForLoginCommands(posted, "linkdataminion", defaultActionTestCommands)
			answerOK := strings.Contains(posted, "```text\n> @linkdataminion fix") &&
				strings.Contains(posted, `\x1b`) && strings.Contains(posted, `\u202e`)
			if action == command.SecurityAudit {
				answerOK = strings.Contains(posted, "```text\n> Executor-reported security audit summary.") &&
					!strings.Contains(posted, "@linkdataminion fix") && !strings.Contains(posted, "https://example.test/track") &&
					!strings.Contains(posted, `\x1b`) && !strings.Contains(posted, `\u202e`)
			}
			if writes != 1 || !answerOK || strings.ContainsRune(posted, '\x1b') || strings.ContainsRune(posted, '\u202e') ||
				!errors.Is(parseErr, command.ErrNoCommand) ||
				strings.Contains(posted, "PRIVATE") || strings.Contains(posted, "SECRET_CODE_123") || strings.Contains(posted, "FORGED JOURNAL ENTRY") ||
				strings.Contains(journal.String(), "PRIVATE") || strings.Contains(journal.String(), "SECRET_CODE_123") ||
				strings.Contains(journal.String(), "FORGED JOURNAL ENTRY") || strings.ContainsRune(journal.String(), '\x1b') {
				t.Fatalf("worker diagnostic escaped or answer bypassed inert publication: writes=%d posted=%q journal=%q", writes, posted, journal.String())
			}
			path := filepath.Join(owner.WorkRoot, "worker-diagnostics", job.Identity.AttemptID+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var record retainedWorkerDiagnostics
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if record.Identity != job.Identity || record.SourceRef != job.Source.Head ||
				len(record.WorkerReportedDiagnostics) != 1 || record.WorkerReportedDiagnostics[0] != outcome.Result.Diagnostics[0] || record.WorkerReportedFailure != nil {
				t.Fatalf("validated diagnostic was not bound to private attempt record: %s, %+v", path, record)
			}
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatalf("private diagnostic record permissions: %v, %v", info, err)
			}
			if !strings.Contains(journal.String(), fmt.Sprintf("private_path=%q", path)) {
				t.Fatalf("operator cannot locate private diagnostic record: %q", journal.String())
			}
		})
	}
}

func TestHTTPChangedFixPublishesBranchWithoutWorkerText(t *testing.T) {
	_, _, remote := gitFixtureRemote(t, workspace.File{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source\n")})
	runGit := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Env = testsupport.GitEnvironment(t.TempDir())
		return cmd.CombinedOutput()
	}
	forkRemote := filepath.Join(filepath.Dir(remote), "fork.git")
	if output, err := runGit("clone", "--bare", remote, forkRemote); err != nil {
		t.Fatalf("create fork fixture: %v: %s", err, output)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(t.TempDir(), "fork-gitconfig")
	if output, err := runGit("config", "--file", gitConfig, "url."+forkRemote+".insteadOf", "https://github.com/bot/r.git"); err != nil {
		t.Fatalf("configure fork fixture: %v: %s", err, output)
	}
	fakeOnPath(t, "git", fmt.Sprintf("#!/bin/sh\nexec %s -c %s \"$@\"\n", shellQuote(git), shellQuote("include.path="+gitConfig)))
	checkout := cloneFixture(t, t.TempDir())
	const branch = "minion/fix-reviewed"
	if err := checkout.NewBranch(t.Context(), branch); err != nil {
		t.Fatal(err)
	}
	snapshot, err := checkout.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Empty base files are omitted on the HTTP wire.
	snapshot.BaseFiles = nil
	request := usageJob()
	request.Source.UpdatedAt = time.Now()
	request.Command = command.Command{Name: "fix", Action: command.Fix}
	job := testJob(command.Fix)
	job.Identity.JobID = request.ID
	job.Identity.Repository = request.Repository.Name
	job.Identity.RequesterID = request.Source.Author.ID
	job.Source = snapshot
	f := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil)
	f.pull(t)
	f.ack(t, "/before", f.call(1), http.StatusOK, 1)
	paid := f.call(2)
	paid.Checkpoint.Call.Complete = true
	f.ack(t, "/after", paid, http.StatusOK, 2)
	outcome := f.outcome(3, nil)
	outcome.Result.Answer = "@secondbot fix\n![pixel](https://example.invalid/track)\nCloses #123"
	outcome.Result.Changes = []workspace.Change{{Path: "source.txt", Mode: workspace.Regular, Data: []byte("changed\n")}}
	outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
	outcome.Result.Diagnostics = []Diagnostic{{Code: "PRIVATE_FIX", Message: "PRIVATE diagnostic\n\x1b[2JFORGED JOURNAL ENTRY"}}
	if err := outcome.Result.Validate(job); err != nil {
		t.Fatalf("invalid changed-fix result: %v", err)
	}
	f.ack(t, "/outcome", outcome, http.StatusOK, 3)
	got := f.result(t)
	if got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("receiver changed fix result: %+v", got)
	}
	var posted string
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case serveFixFlowFixture(t, w, r):
		case captureComment(t, w, r, &posted):
			writes++
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	var journal bytes.Buffer
	owner := Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "fixture-token"), BotLogin: "bot", Store: openState(t, time.Now()), Logger: log.New(&journal, "", 0)}
	repo := github.Repository{ID: 1, FullName: "o/r", Owner: github.User{ID: 2}, DefaultBranch: "main", Visibility: "public"}
	fork := github.Repository{ID: 4, FullName: "bot/r", Fork: true, Parent: &repo, Source: &repo, Visibility: "public"}
	prepared := &preparedAction{job: job, request: request, checkout: checkout, repository: repo, fork: fork, branch: branch, issueNodeID: "issue-node"}
	if err := owner.publish(t.Context(), prepared, got.result); err != nil {
		t.Fatal(err)
	}
	sha := owner.Store.Data.PendingFixes[request.ID].SHA
	want := fmt.Sprintf("Fix branch `%s` is ready in `bot/r` at `%s`. @writer, [open the pull request yourself](https://github.com/o/r/compare/main...bot:minion%%2Ffix-reviewed?expand=1) after reviewing the diff. Repository review rules, when enabled, can then evaluate your authorship.\n\n%s", branch, sha, request.Marker())
	if len(sha) != 40 || writes != 1 || posted != want || strings.Contains(posted, outcome.Result.Answer) ||
		strings.Contains(posted, "PRIVATE") || strings.Contains(posted, "FORGED") || strings.Contains(journal.String(), "PRIVATE") || strings.Contains(journal.String(), "FORGED") || strings.ContainsRune(journal.String(), '\x1b') {
		t.Fatalf("changed fix exposed worker text or lost branch reply: writes=%d posted=%q journal=%q", writes, posted, journal.String())
	}
	if output, err := runGit("--git-dir", forkRemote, "show", sha+":source.txt"); err != nil || string(output) != "changed\n" {
		t.Fatalf("receiver change was not pushed: %q, %v", output, err)
	}
	if output, err := runGit("--git-dir", forkRemote, "rev-parse", "refs/heads/"+branch); err != nil || strings.TrimSpace(string(output)) != sha {
		t.Fatalf("pushed branch does not point to recorded fix: %q, %v", output, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "worker-diagnostics", job.Identity.AttemptID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var record retainedWorkerDiagnostics
	if err := json.Unmarshal(data, &record); err != nil || !reflect.DeepEqual(record.WorkerReportedDiagnostics, outcome.Result.Diagnostics) {
		t.Fatalf("changed-fix diagnostic was not retained privately: %+v, %v", record, err)
	}
}

func TestHTTPOverRenderedAnswerRefusedBeforePublication(t *testing.T) {
	job := testJob(command.Comment)
	f := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil)
	f.pull(t)
	f.ack(t, "/before", f.call(1), http.StatusOK, 1)
	paid := f.call(2)
	paid.Checkpoint.Call.Complete = true
	f.ack(t, "/after", paid, http.StatusOK, 2)
	outcome := f.outcome(3, nil)
	outcome.Result.Answer = strings.Repeat("x", maxOutputBytes-len(inertModelOutput("\x1b"))+1) + "\x1b"
	outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
	if len(outcome.Result.Answer) > maxOutputBytes || len(inertModelOutput(outcome.Result.Answer)) != maxOutputBytes+1 ||
		outcome.Result.validate(job, false) != nil {
		t.Fatal("fixture does not isolate rendered publication overflow")
	}
	f.ack(t, "/outcome", outcome, http.StatusOK, 3)
	got := f.result(t)
	if got.err != nil || !reflect.DeepEqual(got.result, outcome.Result) {
		t.Fatalf("receiver changed raw worker result: %+v", got)
	}
	writes := 0
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes++
		t.Errorf("over-rendered answer reached GitHub: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer githubServer.Close()
	var journal bytes.Buffer
	owner := Orchestrator{WorkRoot: t.TempDir(), GitHub: testGitHub(githubServer, "fixture-token"), Logger: log.New(&journal, "", 0)}
	err := owner.publish(t.Context(), &preparedAction{job: job}, got.result)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "invalid action result") || writes != 0 ||
		strings.ContainsRune(journal.String(), '\x1b') {
		t.Fatalf("over-rendered answer bypassed publication gate: writes=%d error=%v journal=%q", writes, err, journal.String())
	}
}

func TestHTTPFailureAtWireTextLimitRemainsPrivate(t *testing.T) {
	job := testJob(command.Comment)
	f := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil)
	f.pull(t)
	outcome := f.outcome(1, nil)
	outcome.Failure = &httpFailure{Message: strings.Repeat("\x01", httpFailureMessageLimit)}
	f.ack(t, "/outcome", outcome, http.StatusOK, 1)
	got := f.result(t)
	if got.err == nil || strings.ContainsRune(got.err.Error(), '\x01') {
		t.Fatalf("maximum worker failure text reached the parent error or was lost: %v", got.err)
	}
	var journal bytes.Buffer
	owner := Orchestrator{WorkRoot: t.TempDir(), Logger: log.New(&journal, "", 0)}
	if err := owner.retainWorkerDiagnostics(job, got.result, got.err); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(owner.WorkRoot, "worker-diagnostics", job.Identity.AttemptID+".json")
	if strings.Count(journal.String(), "\n") != 1 || strings.ContainsRune(journal.String(), '\x01') ||
		!strings.Contains(journal.String(), fmt.Sprintf("private_path=%q", path)) {
		t.Fatalf("maximum worker failure escaped parent journal or lost private path: %q", journal.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record retainedWorkerDiagnostics
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record.WorkerReportedFailure, outcome.Failure) || record.Identity != job.Identity {
		t.Fatalf("maximum bounded failure was not retained intact: record=%+v", record)
	}
}

func TestHTTPWorkerFailureThroughOrchestratorAndMonitor(t *testing.T) {
	gitFixtureRemote(t)
	installActionTestSkills(t, testReviewSkillURL)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveIssueFixture(t, w, r) {
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	message := strings.Repeat("\x01", httpFailureMessageLimit-len("\nFORGED\x1b[2J")) + "\nFORGED\x1b[2J"
	failure := &httpFailure{Message: message, Terminal: true, Canceled: true, Deadline: true, Unsafe: true,
		OutcomeUnknown: true, RequestTransport: true, MissingExecutable: true, NoSpace: true,
		QuotaExceeded: true, OutputLimit: true}
	var journal bytes.Buffer
	parentLogger := log.New(&journal, "", 0)
	var jobs []Job
	root := t.TempDir()
	owner := &Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "fixture"), BotLogin: "linkdataminion", Main: "codex",
		Logger: parentLogger, Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{testReviewSkillURL}}}}
	owner.Executor = executorFunc(func(ctx context.Context, job Job, sink Checkpoints) (Result, error) {
		jobs = append(jobs, job)
		f := newHTTPServerTest(t, ctx, job, sink, nil)
		f.pull(t)
		outcome := f.outcome(1, nil)
		outcome.Result.Answer = "FORGED_PUBLIC_CANDIDATE"
		outcome.Failure = failure
		if len(jobs) == 1 {
			invalid := outcome
			invalid.Failure = &httpFailure{Message: message + "x"}
			if got := f.request("/outcome", invalid); got.err != nil || got.status != http.StatusBadRequest {
				t.Fatalf("over-cap worker failure accepted: %+v", got)
			}
		}
		f.ack(t, "/outcome", outcome, http.StatusOK, 1)
		got := f.result(t)
		if got.err == nil || got.result.Answer != outcome.Result.Answer {
			t.Fatalf("received outcome lost failure or raw candidate: %+v", got)
		}
		return got.result, got.err
	})
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	clock := now
	store := openState(t, now.Add(-time.Minute))
	gh := &httpFailureReplyGitHub{actionMonitorGitHub: &actionMonitorGitHub{
		items:        []github.Item{{Key: "request", NodeID: "request-node", Body: "@linkdataminion comment", Author: github.User{ID: 7, Login: "writer"}}},
		notification: github.Notification{Repository: github.Repository{FullName: "o/r"}, Subject: github.Subject{Type: "Issue", URL: "https://api.github.com/repos/o/r/issues/7"}, UpdatedAt: now},
	}}
	m := monitor.Monitor{Config: config.Config{BotLogin: "linkdataminion", Main: "codex", PollIntervalSeconds: 60,
		Commands:     map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{testReviewSkillURL}}},
		Repositories: []config.Repository{{Name: "o/r", ID: 1, OwnerID: 2, Actions: []string{"comment"}, AllowedActors: []string{"writer"}, AllowedActorIDs: map[string]int64{"writer": 7}}}},
		GitHub: gh, Handler: owner, Logger: parentLogger, Now: func() time.Time { return clock }}
	for attempt := 1; attempt <= 3; attempt++ {
		_, err := m.RunOnce(t.Context(), store)
		if (err == nil) != (attempt == 3) || len(jobs) != attempt {
			t.Fatalf("worker claims changed retry policy at attempt %d: jobs=%d state=%+v error=%v", attempt, len(jobs), store.Data, err)
		}
		if attempt < 3 && len(gh.replies) != 0 {
			t.Fatalf("worker failure replied before final attempt %d: replies=%q", attempt, gh.replies)
		}
		if count := strings.Count(journal.String(), fmt.Sprintf("failed (attempt %d/3)", attempt)); count != 1 {
			t.Fatalf("worker failure logged %d times at attempt %d: %q", count, attempt, journal.String())
		}
		clock = clock.Add(10 * time.Minute)
	}
	if len(gh.replies) != 1 || !strings.Contains(gh.replies[0], "could not complete this request") ||
		strings.Contains(gh.replies[0], "FORGED") || strings.Contains(gh.replies[0], message) ||
		strings.Contains(journal.String(), "FORGED") || strings.ContainsRune(journal.String(), '\x01') || strings.ContainsRune(journal.String(), '\x1b') {
		t.Fatalf("worker text escaped into public reply or journal: replies=%q journal=%q", gh.replies, journal.String())
	}
	for _, line := range strings.Split(strings.TrimSuffix(journal.String(), "\n"), "\n") {
		if len(line) > 1024 {
			t.Fatalf("hostile worker text expanded a parent log line: bytes=%d", len(line))
		}
	}
	for _, job := range jobs {
		data, err := os.ReadFile(filepath.Join(root, "worker-diagnostics", job.Identity.AttemptID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var record retainedWorkerDiagnostics
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		if record.Identity != job.Identity || record.SourceRef != job.Source.Head || !reflect.DeepEqual(record.WorkerReportedFailure, failure) {
			t.Fatalf("worker failure flags or text changed in private record: %+v", record)
		}
	}
}

func TestHTTPInvisibleRefutationCannotBecomePublication(t *testing.T) {
	for _, test := range []struct{ name, text string }{{"NUL", "\x00"}, {"zero-width space", "\u200b"}} {
		t.Run(test.name, func(t *testing.T) {
			owner := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
			want := privateReviewResult(owner.job)
			transport := newHTTPServerTest(t, t.Context(), owner.job, owner.sink, nil)
			transport.pull(t)
			sequence := uint64(1)
			transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: owner.review(nil, nil, "")}, http.StatusOK, sequence)
			for i, call := range want.Calls[:3] {
				sequence++
				transport.ack(t, "/before", httpCall{Sequence: sequence, Checkpoint: owner.call(callMetadata(call))}, http.StatusOK, sequence)
				sequence++
				transport.ack(t, "/after", httpCall{Sequence: sequence, Checkpoint: owner.call(call)}, http.StatusOK, sequence)
				sequence++
				if i != 2 {
					transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: owner.review(want.Calls[:i+1], nil, "")}, http.StatusOK, sequence)
					continue
				}
				path := filepath.Join(owner.owner.WorkRoot, "reviews", owner.job.Identity.AttemptID+".json")
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				invalid := owner.review(want.Calls[:i+1], []Refutation{{Round: 2, Text: test.text}}, "")
				ack := transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: invalid}, http.StatusInternalServerError, sequence)
				if ack.Failure == nil || !errors.Is(ack.Failure.err(), monitor.ErrTerminalJob) {
					t.Fatalf("invisible refutation reached private ledger: %+v", ack)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) || len(owner.sink.review.Refutations) != 0 {
					t.Fatalf("rejected refutation changed private ledger: error=%v", err)
				}
			}
			sequence++
			outcome := transport.outcome(sequence, errors.New("worker stopped after rejected review"))
			outcome.Result.Calls = want.Calls[:3]
			transport.ack(t, "/outcome", outcome, http.StatusOK, sequence)
			got := transport.result(t)
			if err := owner.sink.finish(got.result, got.err); !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("invalid review permitted completed result: %v", err)
			}
		})
	}
}

func TestHTTPPrivateRefutationAtWireLimitStaysInReviewRecord(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	f := newExecutionAdapterFixture(t, job)
	f.startUsage()
	var journal bytes.Buffer
	f.owner.Logger = log.New(&journal, "", 0)
	result := privateReviewResult(f.job)
	prefix := "PRIVATE_REFUTATION\x00\n\x1b[2J"
	result.Refutations[0].Text = prefix + strings.Repeat("x", maxOutputBytes-len(prefix))
	if err := result.Validate(f.job); err != nil {
		t.Fatal(err)
	}
	transport := newHTTPServerTest(t, t.Context(), f.job, f.sink, nil)
	transport.pull(t)
	sequence := uint64(1)
	transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: f.review(nil, nil, "")}, http.StatusOK, sequence)
	for i, call := range result.Calls {
		sequence++
		transport.ack(t, "/before", httpCall{Sequence: sequence, Checkpoint: f.call(callMetadata(call))}, http.StatusOK, sequence)
		sequence++
		transport.ack(t, "/after", httpCall{Sequence: sequence, Checkpoint: f.call(call)}, http.StatusOK, sequence)
		sequence++
		var refs []Refutation
		if call.Round >= 2 {
			refs = result.Refutations
		}
		review := f.review(result.Calls[:i+1], refs, "")
		if i == 2 {
			path := filepath.Join(f.owner.WorkRoot, "reviews", f.job.Identity.AttemptID+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			invalid := review
			invalid.Refutations = []Refutation{{Round: 2, Text: result.Refutations[0].Text + "x"}}
			if got := transport.request("/review", httpReview{Sequence: sequence, Checkpoint: invalid}); got.err != nil || got.status != http.StatusBadRequest {
				t.Fatalf("over-cap private refutation accepted: %+v", got)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected refutation changed saved review: %v", err)
			}
		}
		transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
	}
	sequence++
	transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: f.review(result.Calls, result.Refutations, result.CriticStopReason)}, http.StatusOK, sequence)
	sequence++
	transport.ack(t, "/outcome", httpOutcome{Sequence: sequence, Result: result}, http.StatusOK, sequence)
	got := transport.result(t)
	if got.err != nil || f.sink.finish(got.result, got.err) != nil {
		t.Fatalf("valid private refutation was not accepted: result=%+v error=%v", got.result, got.err)
	}
	saved := readPrivateReview(t, f.owner.WorkRoot)
	if !reflect.DeepEqual(saved.Refutations, result.Refutations) || strings.Contains(journal.String(), "PRIVATE_REFUTATION") || strings.ContainsRune(journal.String(), '\x1b') {
		t.Fatalf("private refutation changed or reached journal: saved=%+v journal=%q", saved.Refutations, journal.String())
	}
	var posted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveIssueFixture(t, w, r) && !captureComment(t, w, r, &posted) {
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	f.owner.GitHub = testGitHub(server, "fixture-token")
	request := usageJob()
	request.Command = command.Command{Name: "comment", Action: command.Comment}
	if err := f.owner.publish(t.Context(), &preparedAction{job: f.job, request: request}, got.result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(posted, "Public answer.") || strings.Contains(posted, "PRIVATE_REFUTATION") || strings.Contains(journal.String(), "PRIVATE_REFUTATION") {
		t.Fatalf("private refutation escaped publication: posted=%q journal=%q", posted, journal.String())
	}
}

func TestHTTPResultCannotPublishPrivateRefutation(t *testing.T) {
	for _, test := range []struct {
		name, private, answer string
	}{
		{"whole copy", "PRIVATE_MAIN_REFUTATION_TEXT", "Public reply.\nPRIVATE_MAIN_REFUTATION_TEXT"},
		{"short whole copy", "Secret abc123", "Secret abc123"},
		{"audit title quoted newline", "Private objection\nand evidence.", "Audit report\n"},
		{"audit title short whole copy", "Secret abc123", "Audit report\n"},
		{"rendered quote leader", "> UNIQUE_PRIVATE_PHRASE_STAYS_HERE", "UNIQUE_PRIVATE_PHRASE_STAYS_HERE"},
		{"Unicode simple fold", "PRIVATE Σ MARKER MUST STAY HERE", "PRIVATE ς MARKER MUST STAY HERE"},
		{"Unicode expanded fold", "The private Straße example should stay in review.", "Public reply. THE PRIVATE STRASSE EXAMPLE SHOULD STAY IN REVIEW."},
		{"canonical Unicode composition", "PRIVATE café OBJECTION MUST STAY HERE", "Public reply. PRIVATE cafe\u0301 OBJECTION MUST STAY HERE"},
		{"HTML entity in Fix Markdown", "Private objection must stay out of this change", "Private&#32;objection must stay out of this change"},
		{"case changed", "PRIVATE_MAIN_REFUTATION_TEXT", "Public reply.\nprivate_main_refutation_text"},
		{"bare Markdown quote leaders", "PRIVATE reviewer notes must stay private", "Public reply.\n>PRIVATE reviewer notes must stay private"},
		{"nested indented quote leaders", "PRIVATE reviewer notes must stay private", "Public reply.\n  > >PRIVATE reviewer notes must stay private"},
		{"collapsed whitespace", "The private refutation describes a missing access control check.\nThe same account cannot act for another user.", "Public reply. The private refutation describes a missing access control check. The same account cannot act for another user."},
		{"one substantive sentence", "The private refutation describes a missing access control check.\nThe same account cannot act for another user.", "Public reply. The same account cannot act for another user."},
		{"inline Markdown sentence", "The private refutation describes a missing access control check.\nThe same account cannot act for another user.", "Public reply: **The same account cannot act for another user.**"},
		{"Markdown bullet leader", "The private refutation describes a missing access control check.\nThe same account cannot act for another user.", "Public reply.\n- The same account cannot act for another user."},
		{"Markdown bullet after plain line", "The private refutation describes a missing access control check.\nThe same account cannot act for another user.", "Public reply\n- The same account cannot act for another user."},
	} {
		t.Run(test.name, func(t *testing.T) {
			action := command.Comment
			switch test.name {
			case "HTML entity in Fix Markdown":
				action = command.Fix
			case "audit title quoted newline", "audit title short whole copy":
				action = command.SecurityAudit
			}
			job := withClaudeCritic(testJob(action))
			f := newExecutionAdapterFixture(t, job)
			var journal bytes.Buffer
			f.owner.Logger = log.New(&journal, "", 0)
			result := privateReviewResult(f.job)
			result.Refutations[0].Text = test.private
			result.Answer = test.answer
			if action == command.Fix {
				result.Answer = "Public reply."
				result.Changes = []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte(test.answer)}}
			} else if action == command.SecurityAudit {
				result.Artifacts = auditArtifactsFixture(f.job, sourceIdentity(f.job), result.Answer)
				title, err := json.Marshal(test.private)
				if err != nil {
					t.Fatal(err)
				}
				findings := strings.Replace(auditCandidateFindingFixture, `"title":"Unverified boundary"`, `"title":`+string(title), 1)
				ledger := strings.Replace(auditCoveredLedgerFixture, `"status":"covered"`, `"status":"candidate"`, 1)
				ledger = strings.Replace(ledger, `"result_fingerprints":[]`, `"result_fingerprints":["F"]`, 1)
				for i := range result.Artifacts {
					switch result.Artifacts[i].Path {
					case "findings.json":
						result.Artifacts[i].Data = []byte(findings)
					case "coverage-ledger.json":
						result.Artifacts[i].Data = []byte(ledger)
					}
				}
			}
			if err := result.Validate(f.job); err != nil {
				t.Fatal(err)
			}
			transport := newHTTPServerTest(t, t.Context(), f.job, f.sink, nil)
			transport.pull(t)
			sequence := uint64(1)
			transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: f.review(nil, nil, "")}, http.StatusOK, sequence)
			for i, call := range result.Calls {
				sequence++
				transport.ack(t, "/before", httpCall{Sequence: sequence, Checkpoint: f.call(callMetadata(call))}, http.StatusOK, sequence)
				sequence++
				transport.ack(t, "/after", httpCall{Sequence: sequence, Checkpoint: f.call(call)}, http.StatusOK, sequence)
				sequence++
				var refs []Refutation
				if call.Round >= 2 {
					refs = result.Refutations
				}
				transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: f.review(result.Calls[:i+1], refs, "")}, http.StatusOK, sequence)
			}
			sequence++
			transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: f.review(result.Calls, result.Refutations, result.CriticStopReason)}, http.StatusOK, sequence)
			sequence++
			transport.ack(t, "/outcome", httpOutcome{Sequence: sequence, Result: result}, http.StatusOK, sequence)
			got := transport.result(t)
			if got.err != nil || f.sink.finish(got.result, got.err) != nil {
				t.Fatalf("private result was not accepted by HTTP: %+v", got)
			}
			if saved := readPrivateReview(t, f.owner.WorkRoot); !reflect.DeepEqual(saved.Refutations, result.Refutations) {
				t.Fatalf("private review checkpoint was not retained: %+v", saved)
			}
			var posted string
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveIssueFixture(t, w, r) {
					return
				}
				if captureComment(t, w, r, &posted) {
					writes++
					return
				}
				t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			f.owner.GitHub = testGitHub(server, "fixture-token")
			request := usageJob()
			request.Command = command.Command{Name: string(action), Action: action}
			err := f.owner.publish(t.Context(), &preparedAction{job: f.job, request: request}, got.result)
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "publication contains private review evidence") || writes != 0 ||
				posted != "" || strings.Contains(err.Error(), result.Refutations[0].Text) || strings.Contains(journal.String(), result.Refutations[0].Text) {
				t.Fatalf("private refutation crossed publication boundary: error=%v writes=%d posted=%q journal=%q", err, writes, posted, journal.String())
			}
		})
	}
}

func TestHTTPQuotedRoundTwoRefutationStopsOrchestratorPublication(t *testing.T) {
	gitFixtureRemote(t)
	const private = "The first private reviewer finding stays internal. The second private reviewer finding must not enter the public reply."
	const copied = "The second private reviewer finding must not enter the public reply."
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveIssueFixture(t, w, r) {
			return
		}
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	workRoot, usageDir := t.TempDir(), filepath.Join(t.TempDir(), "usage")
	var journal bytes.Buffer
	e := &Orchestrator{WorkRoot: workRoot, Usage: &usage.Store{Dir: usageDir}, GitHub: testGitHub(server, "token"), BotLogin: "bot", Adversarial: "claude",
		Logger:   log.New(&journal, "", 0),
		Commands: map[string]config.CommandMapping{"comment": {Action: string(command.Comment), Skills: []string{skills.BuiltinAntiPromptInjection}}},
	}
	e.Executor = executorFunc(func(ctx context.Context, job Job, checkpoints Checkpoints) (Result, error) {
		transport := newHTTPServerTest(t, ctx, job, checkpoints, nil)
		transport.pull(t)
		result := privateReviewResult(job)
		result.Refutations[0].Text = private
		result.Answer = `Public reply: "` + copied + `"`
		for i := range result.Calls {
			result.Calls[i].Tokens, result.Calls[i].Complete = usage.Tokens{Input: int64(i + 1), Output: 1}, true
		}
		if err := result.Validate(job); err != nil {
			t.Fatalf("invalid refutation echo fixture: %v", err)
		}
		sequence := uint64(1)
		binding := CallCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
		review := ReviewCheckpoint{Version: job.Version, Identity: job.Identity, SourceRef: job.Source.Head}
		transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
		sequence++
		for i, call := range result.Calls {
			binding.Call = callMetadata(call)
			transport.ack(t, "/before", httpCall{Sequence: sequence, Checkpoint: binding}, http.StatusOK, sequence)
			sequence++
			binding.Call = call
			transport.ack(t, "/after", httpCall{Sequence: sequence, Checkpoint: binding}, http.StatusOK, sequence)
			sequence++
			review.Calls = result.Calls[:i+1]
			if call.Round >= 2 {
				review.Refutations = result.Refutations
			}
			transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
			sequence++
		}
		review.CriticStopReason = result.CriticStopReason
		transport.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: review}, http.StatusOK, sequence)
		sequence++
		outcome := transport.outcome(sequence, nil)
		outcome.Result = result
		transport.ack(t, "/outcome", outcome, http.StatusOK, sequence)
		got := transport.result(t)
		return got.result, got.err
	})
	request := usageJob()
	request.Command = command.Command{Name: "comment", Action: command.Comment}
	request.Repository.Actions = []string{"comment"}
	err := e.Execute(t.Context(), request)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "publication contains private review evidence") || writes.Load() != 0 ||
		strings.Contains(err.Error(), private) || strings.Contains(journal.String(), private) {
		t.Fatalf("refutation echo crossed publication boundary: error=%v writes=%d journal=%q", err, writes.Load(), journal.String())
	}
	if saved := readPrivateReview(t, workRoot); len(saved.Refutations) != 1 || saved.Refutations[0].Text != private || len(saved.Calls) != 5 {
		t.Fatalf("private review evidence was lost: %+v", saved)
	}
	records, err := usage.ReadAll(usageDir)
	if err != nil || len(records) != 1 || records[0].Status != "failed" || records[0].Tokens != (usage.Tokens{Input: 15, Output: 5}) ||
		records[0].CodexCalls != 2 || records[0].ClaudeCalls != 3 || !records[0].UsageComplete {
		t.Fatalf("paid usage evidence was lost: records=%+v error=%v", records, err)
	}
}
