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
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
)

func TestHTTPWorkerDiagnosticsRemainPrivateThroughPublication(t *testing.T) {
	job := testJob(command.Comment)
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
	outcome.Result.Diagnostics = []Diagnostic{{Code: "SECRET_CODE_123", Message: "PRIVATE diagnostic\n\x1b[2JFORGED JOURNAL ENTRY"}}
	if err := outcome.Result.Validate(job); err != nil {
		t.Fatalf("invalid successful worker result: %v", err)
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
	request.Command = command.Command{Name: "comment", Action: command.Comment}
	if err := owner.publish(context.Background(), &preparedAction{job: job, request: request}, got.result); err != nil {
		t.Fatal(err)
	}
	_, parseErr := command.ParseForLoginCommands(posted, "linkdataminion", defaultActionTestCommands)
	if writes != 1 || !strings.Contains(posted, "```text\n> @linkdataminion fix") ||
		!strings.Contains(posted, `\x1b`) || !strings.Contains(posted, `\u202e`) || strings.ContainsRune(posted, '\x1b') || strings.ContainsRune(posted, '\u202e') ||
		!errors.Is(parseErr, command.ErrNoCommand) ||
		strings.Contains(posted, "PRIVATE") || strings.Contains(posted, "SECRET_CODE_123") ||
		strings.Contains(journal.String(), "PRIVATE") || strings.Contains(journal.String(), "SECRET_CODE_123") {
		t.Fatalf("worker diagnostic escaped or answer bypassed inert publication: writes=%d posted=%q journal=%q", writes, posted, journal.String())
	}
	path := filepath.Join(owner.WorkRoot, "worker-diagnostics", job.Identity.AttemptID+".json")
	data, err := os.ReadFile(path)
	var record retainedWorkerDiagnostics
	if err != nil || json.Unmarshal(data, &record) != nil || record.Identity != job.Identity || record.SourceRef != job.Source.Head ||
		len(record.WorkerReportedDiagnostics) != 1 || record.WorkerReportedDiagnostics[0] != outcome.Result.Diagnostics[0] || record.WorkerReportedFailure != nil {
		t.Fatalf("validated diagnostic was not bound to private attempt record: %s, %+v, %v", path, record, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private diagnostic record permissions: %v, %v", info, err)
	}
	if !strings.Contains(journal.String(), fmt.Sprintf("private_path=%q", path)) {
		t.Fatalf("operator cannot locate private diagnostic record: %q", journal.String())
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
	var record retainedWorkerDiagnostics
	if err != nil || json.Unmarshal(data, &record) != nil || record.WorkerReportedFailure == nil ||
		record.WorkerReportedFailure.Message != outcome.Failure.Message || record.Identity != job.Identity {
		t.Fatalf("maximum bounded failure was not retained intact: record=%+v error=%v", record, err)
	}
}
