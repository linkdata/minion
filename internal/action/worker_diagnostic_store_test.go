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
	"sync/atomic"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestWorkerDiagnosticsStayInPrivateAttemptRecord(t *testing.T) {
	job := testJob(command.Comment)
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Diagnostics: []Diagnostic{{Code: "SECRET_CODE_123", Message: "SECRET cleanup\nFORGED ENTRY"}}}
	var journal bytes.Buffer
	owner := Orchestrator{WorkRoot: t.TempDir(), Logger: log.New(&journal, "", 0)}
	failure := (&httpFailure{Message: "SECRET failure\nFORGED ENTRY", Unsafe: true}).workerErr()
	path := filepath.Join(owner.WorkRoot, "worker-diagnostics", job.Identity.AttemptID+".json")
	if err := owner.retainWorkerDiagnostics(job, result, failure); err != nil {
		t.Fatalf("private attempt record missing: path=%q error=%v", path, err)
	}
	if strings.Contains(journal.String(), "SECRET") || strings.Contains(journal.String(), "FORGED") ||
		!strings.Contains(journal.String(), "count=1") || !strings.Contains(journal.String(), path) {
		t.Fatalf("worker text reached journal or record location is hidden: %q", journal.String())
	}
	data, err := os.ReadFile(path)
	var record retainedWorkerDiagnostics
	if err != nil || json.Unmarshal(data, &record) != nil || record.Identity != job.Identity || record.SourceRef != job.Source.Head ||
		record.WorkerReportedFailure == nil || record.WorkerReportedFailure.Message != "SECRET failure\nFORGED ENTRY" ||
		len(record.WorkerReportedDiagnostics) != 1 || record.WorkerReportedDiagnostics[0] != result.Diagnostics[0] {
		t.Fatalf("private worker evidence changed: record=%+v error=%v", record, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private record permissions: %v, %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("private directory permissions: %v, %v", info, err)
	}
	if err := owner.retainWorkerDiagnostics(job, result, failure); err != nil {
		t.Fatalf("same attempt changed its private record: %v", err)
	}
	result.Diagnostics[0].Message = "different report"
	if err := owner.retainWorkerDiagnostics(job, result, failure); !errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("conflicting worker record was accepted: %v", err)
	}
}

func TestPrivateWorkerDiagnosticSaveFailureBlocksPublication(t *testing.T) {
	job := testJob(command.Comment)
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "candidate",
		Calls:       []CallUsage{{Provider: job.Main.Provider, Model: job.Main.Model, Effort: job.Main.Effort, Role: roleMain, Round: 1, Complete: true}},
		Diagnostics: []Diagnostic{{Code: "private", Message: "SECRET private detail"}}}
	if err := result.Validate(job); err != nil {
		t.Fatalf("baseline result would not be publishable: %v", err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "worker-diagnostics"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "worker-diagnostics"), 0755); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveIssueFixture(t, w, r) {
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments" {
			writes.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":1}`)
			return
		}
		t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	var journal bytes.Buffer
	owner := Orchestrator{WorkRoot: root, GitHub: testGitHub(server, "fixture-token"), Logger: log.New(&journal, "", 0)}
	request := usageJob()
	request.Command = command.Command{Name: "comment", Action: command.Comment}
	err := owner.publish(context.Background(), &preparedAction{job: job, request: request}, result)
	if !errors.Is(err, monitor.ErrTerminalJob) || strings.Contains(err.Error(), "SECRET") || strings.Contains(journal.String(), "SECRET") || writes.Load() != 0 {
		t.Fatalf("private save failure did not stop publication safely: error=%v journal=%q", err, journal.String())
	}
	if entries, err := os.ReadDir(filepath.Join(root, "worker-diagnostics")); err != nil || len(entries) != 0 {
		t.Fatalf("private save failure left a published record: %v, %v", entries, err)
	}
}
