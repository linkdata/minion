package action

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestWorkerDiagnosticsStayInPrivateAttemptRecord(t *testing.T) {
	job := testJob(command.Comment)
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Diagnostics: []Diagnostic{{Code: "cleanup_failed", Message: "Execution workspace cleanup failed; operator attention is required."}, {Code: "cleanup_recovery", Message: "SECRET cleanup\nFORGED ENTRY"}}}
	var journal bytes.Buffer
	owner := Orchestrator{WorkRoot: t.TempDir(), Logger: log.New(&journal, "", 0)}
	failure := (&httpFailure{Message: "SECRET failure\nFORGED ENTRY", Unsafe: true}).workerErr()
	path := filepath.Join(owner.WorkRoot, "worker-diagnostics", job.Identity.AttemptID+".json")
	if err := owner.retainWorkerDiagnostics(job, result, failure); err != nil {
		t.Fatalf("private attempt record missing: path=%q error=%v", path, err)
	}
	canonicalRoot, err := workspace.CanonicalPath(owner.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	loggedPath := filepath.Join(canonicalRoot, "worker-diagnostics", job.Identity.AttemptID+".json")
	if strings.Contains(journal.String(), "SECRET") || strings.Contains(journal.String(), "FORGED") ||
		!strings.Contains(journal.String(), "count=2") || !strings.Contains(journal.String(), fmt.Sprintf("private_path=%q", loggedPath)) {
		t.Fatalf("worker text reached journal or record location is hidden: %q", journal.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record retainedWorkerDiagnostics
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Identity != job.Identity || record.SourceRef != job.Source.Head ||
		!reflect.DeepEqual(record.WorkerReportedFailure, &httpFailure{Message: "SECRET failure\nFORGED ENTRY", Unsafe: true}) ||
		!reflect.DeepEqual(record.WorkerReportedDiagnostics, result.Diagnostics) || record.CleanupUnverifiedWorkRoot != "" {
		t.Fatalf("private worker evidence changed: record=%+v", record)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("private record permissions: %v, %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm() != 0700 {
		t.Fatalf("private directory permissions: %v, %v", info, err)
	}
	if err := owner.retainWorkerDiagnostics(job, result, failure); err != nil {
		t.Fatalf("same attempt changed its private record: %v", err)
	}
	result.Diagnostics[1].Message = "different report"
	if err := owner.retainWorkerDiagnostics(job, result, failure); !errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("conflicting worker record was accepted: %v", err)
	}
}

func TestWorkerDiagnosticsRecordUnverifiedLocalCleanupWithoutWorkerClaims(t *testing.T) {
	job := testJob(command.Comment)
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head}
	var journal bytes.Buffer
	owner := Orchestrator{WorkRoot: t.TempDir(), Logger: log.New(&journal, "", 0)}
	if err := owner.retainWorkerDiagnostics(job, result, errLocalWorkerCleanupUnverified); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(owner.WorkRoot, "worker-diagnostics", job.Identity.AttemptID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var record retainedWorkerDiagnostics
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.CanonicalPath(owner.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	if record.Identity != job.Identity || record.SourceRef != job.Source.Head || record.CleanupUnverifiedWorkRoot != root || record.WorkerReportedFailure != nil || len(record.WorkerReportedDiagnostics) != 0 {
		t.Fatalf("parent recovery scope was lost or forged worker claims: %+v", record)
	}
	if !strings.Contains(journal.String(), "cleanup_unverified=true") || !strings.Contains(journal.String(), "private_path=") {
		t.Fatalf("parent recovery scope was not logged as private evidence: %q", journal.String())
	}
}

func TestWorkerDiagnosticsRetrySyncsIdenticalRecord(t *testing.T) {
	job := testJob(command.Comment)
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Diagnostics: []Diagnostic{{Code: "private", Message: "worker message"}}}
	root, err := workspace.CanonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := Orchestrator{WorkRoot: root}
	if err := e.retainWorkerDiagnostics(job, result, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "worker-diagnostics", job.Identity.AttemptID+".json")
	originalSync := syncPrivateRecordDirectories
	t.Cleanup(func() { syncPrivateRecordDirectories = originalSync })
	calls := 0
	syncPrivateRecordDirectories = func(dirs ...string) error {
		calls++
		if !reflect.DeepEqual(dirs, []string{filepath.Dir(path), root}) {
			t.Errorf("wrong directories synced: %q", dirs)
		}
		return syscall.EIO
	}
	if err := e.retainWorkerDiagnostics(job, result, nil); !errors.Is(err, monitor.ErrPostModelPublication) ||
		errors.Is(err, monitor.ErrTerminalJob) || !errors.Is(err, syscall.EIO) || calls != 1 {
		t.Fatalf("identical record skipped failed sync: calls=%d error=%v", calls, err)
	}
	syncPrivateRecordDirectories = func(dirs ...string) error {
		calls++
		return originalSync(dirs...)
	}
	if err := e.retainWorkerDiagnostics(job, result, nil); err != nil {
		t.Fatalf("retry did not sync retained record: %v", err)
	}
	if calls != 2 {
		t.Fatalf("retry skipped directory sync: calls=%d", calls)
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
	blocked := filepath.Join(root, "worker-diagnostics")
	if err := os.WriteFile(blocked, []byte("existing file"), 0600); err != nil {
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
	err := owner.publish(t.Context(), &preparedAction{job: job, request: request}, result)
	if !errors.Is(err, monitor.ErrTerminalJob) || strings.Contains(err.Error(), "SECRET") || strings.Contains(journal.String(), "SECRET") || writes.Load() != 0 {
		t.Fatalf("private save failure did not stop publication safely: error=%v journal=%q", err, journal.String())
	}
	if data, err := os.ReadFile(blocked); err != nil || string(data) != "existing file" {
		t.Fatalf("private save failure changed existing file: %q, %v", data, err)
	}
}
