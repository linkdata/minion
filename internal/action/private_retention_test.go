package action

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/usage"
)

func TestPrunePrivateRecordsAfterTerminalDecision(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-privateRecordRetention)
	cutoffID := strings.Repeat("a", 32)
	attemptIDs := map[string]string{
		"cutoff": cutoffID, "lost": "lost.json-attempt", "fresh": "fresh-attempt",
		"accepted": "accepted-attempt", "pending": "pending-attempt", "active": "active-attempt",
	}
	base := t.TempDir()
	root := filepath.Join(base, "work")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(base, "state.json")
	store, err := state.Open(statePath, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	store.Data.TerminalDecisions = map[string]state.TerminalDecision{
		"cutoff":   {AttemptID: cutoffID, Outcome: "succeeded", DecidedAt: cutoff},
		"lost":     {AttemptID: attemptIDs["lost"], Outcome: "lost", DecidedAt: cutoff.Add(-time.Nanosecond)},
		"fresh":    {AttemptID: attemptIDs["fresh"], Outcome: "failed", DecidedAt: cutoff.Add(time.Nanosecond)},
		"accepted": {AttemptID: attemptIDs["accepted"], Outcome: "succeeded", DecidedAt: cutoff},
		"pending":  {AttemptID: attemptIDs["pending"], Outcome: "failed", DecidedAt: cutoff},
	}
	store.Data.Accepted = &state.AcceptedAction{
		JobID: "accepted", AttemptID: attemptIDs["accepted"], ExecutorID: "local",
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
		SubjectType: "Issue", SubjectNumber: 7, SourceKey: "source", SourceNodeID: "node",
		Requester: "writer", RequesterID: 3, CommandName: "review", Action: "review",
		AcceptedAt: cutoff.Add(-time.Minute), Deadline: cutoff.Add(time.Minute), ResultReady: true, Terminal: true,
	}
	if err := store.Data.Accepted.Validate(); err != nil {
		t.Fatal(err)
	}
	store.Data.PublicationWrites = map[string]state.PublicationWrite{
		"pending": {
			AttemptID: attemptIDs["pending"], Kind: "comment", BotLogin: "minion", BotID: 8,
			Repository: "owner/repo", Number: 7, Marker: "<!-- linkdataminion:pending -->",
			Since: cutoff, StartedAt: cutoff, Deadline: cutoff.Add(time.Minute), Status: state.PublicationWritePending,
		},
	}
	store.Data.Processed["cutoff"] = cutoff
	store.Data.PendingFixes["cutoff"] = state.PendingFix{SHA: "0123456789abcdef0123456789abcdef01234567"}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(statePath, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	paths := make(map[string][]string)
	for _, id := range []string{"cutoff", "lost", "fresh", "accepted", "pending", "active"} {
		artifactID := attemptIDs[id]
		completeDir := filepath.Join(root, "audits", "complete-"+artifactID)
		stageDir := filepath.Join(root, "audits", ".complete-"+artifactID)
		incompleteDir := filepath.Join(root, "audits", "incomplete-"+artifactID+"+abc")
		validationDir := filepath.Join(root, auditValidationTempPattern(artifactID)+"abc")
		reportsDir := filepath.Join(root, auditReportsDirPrefix+artifactID+"+abc")
		reviewReportsDir := filepath.Join(root, reviewReportsDirPrefix+artifactID+"+abc")
		historyDir := filepath.Join(root, reviewHistoryDirPrefix+artifactID+"+abc")
		paths[id] = []string{
			filepath.Join(root, "attempt-records", artifactID+".jsonl"),
			filepath.Join(root, "reviews", artifactID+".json"),
			filepath.Join(root, "reviews", ".record+"+artifactID+".json+12345"),
			filepath.Join(root, "worker-diagnostics", artifactID+".json"),
			filepath.Join(root, "worker-diagnostics", ".record+"+artifactID+".json+12345"),
			filepath.Join(completeDir, "REPORT.md"),
			filepath.Join(completeDir, "FINDINGS.json"),
			filepath.Join(stageDir, "REPORT.md"),
			filepath.Join(incompleteDir, "INCOMPLETE.txt"),
			filepath.Join(validationDir, "REPORT.md"),
			filepath.Join(reportsDir, "REPORT.md"),
			filepath.Join(reportsDir, "findings.json"),
			filepath.Join(reviewReportsDir, "REPORT.md"),
			filepath.Join(historyDir, "refutations.json"),
		}
		for _, path := range paths[id] {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("partial private evidence\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		paths[id] = append(paths[id], completeDir, stageDir, incompleteDir, validationDir, reportsDir, reviewReportsDir, historyDir)
	}
	legacy := filepath.Join(root, "audits", "incomplete-owner-repo-7-abc", "INCOMPLETE.txt")
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelated := []string{
		filepath.Join(root, "reviews", ".record+unrelated.json+12345"),
		filepath.Join(root, "reviews", ".record-"+cutoffID+"-12345"),
		filepath.Join(root, auditValidationDirPrefix+"legacy", "REPORT.md"),
		filepath.Join(root, auditReportsDirPrefix+"legacy", "REPORT.md"),
		filepath.Join(root, reviewReportsDirPrefix+"legacy", "REPORT.md"),
		filepath.Join(root, reviewHistoryDirPrefix+"legacy", "refutations.json"),
		filepath.Join(root, auditReportsDirPrefix+"unrelated+abc", "REPORT.md"),
	}
	for _, path := range unrelated {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("unrelated or legacy"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	usageStore := usage.Store{Dir: filepath.Join(base, "usage")}
	usageRecord := usage.Record{
		ID: cutoffID, JobID: "cutoff", Repo: "owner/repo", User: "writer", Action: "review",
		ExecutorID: "local", SourceRef: strings.Repeat("b", 40), UsageSource: "executor_reported",
		Provider: "codex", Model: "default", StartedAt: cutoff.Add(-time.Minute), FinishedAt: &cutoff,
		Status: "failed", CodexCalls: 1, Tokens: usage.Tokens{Input: 7, Output: 2}, UsageComplete: true,
	}
	if err := usageStore.Save(usageRecord); err != nil {
		t.Fatal(err)
	}
	resultPath := statePath + ".result"
	if err := os.WriteFile(resultPath, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	pruner := &Orchestrator{WorkRoot: root}
	if err := pruner.PrunePrivateRecords(t.Context(), store, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cutoff", "lost"} {
		checkPrivateRetentionPaths(t, paths[id], false)
	}
	for _, id := range []string{"fresh", "accepted", "pending", "active"} {
		checkPrivateRetentionPaths(t, paths[id], true)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy audit was removed: %v", err)
	}
	checkPrivateRetentionPaths(t, unrelated, true)
	if got, found, err := usageStore.Load(cutoffID); err != nil || !found || !reflect.DeepEqual(got, usageRecord) {
		t.Fatalf("billing record changed: found=%t record=%+v error=%v", found, got, err)
	}
	if data, err := os.ReadFile(resultPath); err != nil || string(data) != "keep" {
		t.Fatalf("result sidecar changed: %q, %v", data, err)
	}
	if after, err := os.ReadFile(statePath); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("retention changed durable state: %v", err)
	}

	store.Data.Accepted = nil
	write := store.Data.PublicationWrites["pending"]
	write.Status, write.ResolvedAt = state.PublicationWriteObserved, now
	store.Data.PublicationWrites["pending"] = write
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := pruner.PrunePrivateRecords(t.Context(), store, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"accepted", "pending"} {
		checkPrivateRetentionPaths(t, paths[id], false)
	}
	checkPrivateRetentionPaths(t, paths["fresh"], true)
	checkPrivateRetentionPaths(t, paths["active"], true)
}

func checkPrivateRetentionPaths(t *testing.T, paths []string, wantPresent bool) {
	t.Helper()
	for _, path := range paths {
		_, err := os.Stat(path)
		if wantPresent && err != nil || !wantPresent && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("record %s: present=%t error=%v", path, wantPresent, err)
		}
	}
}

func TestPrunePublishingAuditValidationScratchAfterCleanupFailure(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	fakeOnPath(t, "node", "#!/bin/sh\nexit 0\n")
	job := testJob(command.SecurityAudit)
	root := t.TempDir()
	orchestrator := &Orchestrator{WorkRoot: root}
	original := removeAuditValidationDir
	t.Cleanup(func() { removeAuditValidationDir = original })
	var scratch string
	removeAuditValidationDir = func(path string) error {
		scratch = path
		return errors.New("simulated scratch cleanup failure")
	}
	if err := orchestrator.validateAuditForPublication(t.Context(), job, completeAuditResult(job).Artifacts); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(scratch), auditValidationTempPattern(job.Identity.AttemptID)) {
		t.Fatalf("publishing scratch has no attempt identity: %q", scratch)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("simulated cleanup failure left no scratch: %v", err)
	}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	store := openState(t, now.Add(-privateRecordRetention))
	store.Data.TerminalDecisions[job.Identity.JobID] = state.TerminalDecision{
		AttemptID: job.Identity.AttemptID, Outcome: "succeeded", DecidedAt: now.Add(-privateRecordRetention),
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.PrunePrivateRecords(t.Context(), store, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired publishing scratch survived: %v", err)
	}
}

func TestPrunedPartialWorkerLogCannotBeRecreatedByLateUpload(t *testing.T) {
	root := t.TempDir()
	job := testJob(command.Comment)
	f := newHTTPServerTestWithStopServerRoot(t, t.Context(), job, runnerExecutionCheckpoints{}, nil,
		func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, time.Millisecond, root)
	f.pull(t)
	status := Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
		Sequence: 1, LogSequence: 1, WorkerState: "busy", Phase: "prepare"}
	f.ack(t, "/status", httpObservation{Status: status, Logs: []attemptRecord{httpTestRecord(job, 1, "partial discussion")}}, http.StatusOK, 1)
	record := filepath.Join(root, "attempt-records", job.Identity.AttemptID+".jsonl")
	if data, err := os.ReadFile(record); err != nil || !bytes.Contains(data, []byte("partial discussion")) {
		t.Fatalf("partial log was not saved: %q, %v", data, err)
	}
	// The worker never sends its final outcome. Closing the selected executor
	// fences its upload routes, while the saved partial log remains private.
	if err := f.executor.Close(); err != nil {
		t.Fatal(err)
	}
	if result := f.result(t); !errors.Is(result.err, monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("missing final upload was not classified as lost: %v", result.err)
	}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	store := openState(t, now.Add(-privateRecordRetention))
	store.Data.TerminalDecisions[job.Identity.JobID] = state.TerminalDecision{
		AttemptID: job.Identity.AttemptID, Outcome: "lost", DecidedAt: now.Add(-privateRecordRetention),
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := (&Orchestrator{WorkRoot: root}).PrunePrivateRecords(t.Context(), store, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial log survived expiry: %v", err)
	}
	status.Sequence, status.LogSequence = 2, 2
	for _, upload := range []struct {
		path string
		body any
	}{
		{"/status", httpObservation{Status: status, Logs: []attemptRecord{httpTestRecord(job, 2, "late log")}}},
		{"/outcome", f.outcome(1, nil)},
	} {
		response := f.request(upload.path, upload.body)
		if response.err != nil || response.status != http.StatusConflict {
			t.Fatalf("late %s upload: %+v", upload.path, response)
		}
	}
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late upload recreated expired log: %v", err)
	}
}
