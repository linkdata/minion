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
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
)

func TestHTTPWorkerDiagnosticsRemainPrivateThroughPublication(t *testing.T) {
	for _, action := range []command.Action{command.Comment, command.Review, command.SecurityAudit, command.Fix} {
		t.Run(string(action), func(t *testing.T) {
			job := testJob(action)
			job.Identity.Repository = "o/r"
			f := newHTTPServerTest(t, context.Background(), job, runnerExecutionCheckpoints{}, nil)
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
			if err := owner.publish(context.Background(), &preparedAction{job: job, request: request, issueNodeID: "issue-node"}, got.result); err != nil {
				t.Fatal(err)
			}
			_, parseErr := command.ParseForLoginCommands(posted, "linkdataminion", defaultActionTestCommands)
			if writes != 1 || !strings.Contains(posted, "```text\n> @linkdataminion fix") ||
				!strings.Contains(posted, `\x1b`) || !strings.Contains(posted, `\u202e`) || strings.ContainsRune(posted, '\x1b') || strings.ContainsRune(posted, '\u202e') ||
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
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("private diagnostic record permissions: %v, %v", info, err)
			}
			if !strings.Contains(journal.String(), fmt.Sprintf("private_path=%q", path)) {
				t.Fatalf("operator cannot locate private diagnostic record: %q", journal.String())
			}
		})
	}
}

func TestHTTPOverRenderedAnswerRefusedBeforePublication(t *testing.T) {
	job := testJob(command.Comment)
	f := newHTTPServerTest(t, context.Background(), job, runnerExecutionCheckpoints{}, nil)
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
	err := owner.publish(context.Background(), &preparedAction{job: job}, got.result)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "invalid action result") || writes != 0 ||
		strings.ContainsRune(journal.String(), '\x1b') {
		t.Fatalf("over-rendered answer bypassed publication gate: writes=%d error=%v journal=%q", writes, err, journal.String())
	}
}

func TestHTTPFailureAtWireTextLimitRemainsPrivate(t *testing.T) {
	job := testJob(command.Comment)
	f := newHTTPServerTest(t, context.Background(), job, runnerExecutionCheckpoints{}, nil)
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
		Descendants: true, Unconfirmed: true, ScopeUnconfirmed: true, Exhausted: true,
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
		_, err := m.RunOnce(context.Background(), store)
		if (err == nil) != (attempt == 3) || len(jobs) != attempt || store.Data.UnconfirmedProcessJob != "" {
			t.Fatalf("worker claims changed retry policy at attempt %d: jobs=%d state=%+v error=%v", attempt, len(jobs), store.Data, err)
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
	transport := newHTTPServerTest(t, context.Background(), f.job, f.sink, nil)
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
	if err := f.owner.publish(context.Background(), &preparedAction{job: f.job, request: request}, got.result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(posted, "Public answer.") || strings.Contains(posted, "PRIVATE_REFUTATION") || strings.Contains(journal.String(), "PRIVATE_REFUTATION") {
		t.Fatalf("private refutation escaped publication: posted=%q journal=%q", posted, journal.String())
	}
}
