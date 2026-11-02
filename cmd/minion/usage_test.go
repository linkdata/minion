package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/usage"
)

func saveReportFixture(t *testing.T, dir string) {
	t.Helper()
	starts := []string{"2026-10-08T23:00:00Z", "2026-10-09T00:00:00Z", "2026-10-09T12:00:00Z", "2026-10-10T00:00:00Z"}
	actions := []string{"audit", "fix", "review", "security-audit"}
	statuses := []string{"succeeded", "failed", "running", "succeeded"}
	for i := range starts {
		start, err := time.Parse(time.RFC3339, starts[i])
		if err != nil {
			t.Fatal(err)
		}
		finish := start.Add(time.Minute)
		record := usage.Record{ID: fmt.Sprintf("%032x", i+1), JobID: fmt.Sprintf("job-%d", i), Repo: "example-owner/example-repo", User: "alice", Action: actions[i], Number: 1, Provider: "codex", Model: "test-model", Effort: "high", StartedAt: start, FinishedAt: &finish, Status: statuses[i], CodexCalls: 1, UsageComplete: true,
			ExecutorID: "local", SourceRef: strings.Repeat("a", 40), UsageSource: "executor_reported"}
		switch i {
		case 0:
			record.Tokens = usage.Tokens{Input: 100, CacheCreationInput: 20, CachedInput: 40, Output: 20}
		case 1:
			record.Tokens = usage.Tokens{Input: 50, CacheCreationInput: 5, CachedInput: 10, Output: 5}
			record.CodexCalls, record.UsageComplete = 2, false
			record.UsageCoverage = usage.Coverage{Scope: "attempt", MissingFinalReport: true, PartialReport: true}
			record.Provider, record.CriticProvider, record.CriticModel = "claude", "codex", "critic-model"
			record.ClaudeCalls = 3
		case 2:
			record.Repo, record.User = "example-owner/second-repo", "bob"
			record.FinishedAt = nil // A crash after a call can leave running usage marked complete.
		case 3:
			record.CodexCalls = 0
			record.UsageSource = ""
		}
		if err := (usage.Store{Dir: dir}).Save(record); err != nil {
			t.Fatal(err)
		}
	}
}

func reportJSON(t *testing.T, args ...string) usageOutput {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := runUsageWithOutput(append(args, "-json"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, &stderr)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", &stderr)
	}
	var output usageOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestUsageJSONFiltersAndTotals(t *testing.T) {
	dir := t.TempDir()
	saveReportFixture(t, dir)
	t.Setenv("PATH", "") // Reports must work without GitHub, Codex, or other commands.
	output := reportJSON(t, "-dir", dir)
	if strings.Join(output.GroupBy, ",") != "repo,user,action" || len(output.Groups) != 4 {
		t.Fatalf("unexpected groups: %+v", output)
	}
	want := usage.Summary{Attempts: 4, UsageRecordedActions: 4, Succeeded: 2, Failed: 1, Running: 1, CodexCalls: 4, ClaudeCalls: 3, IncompleteUsage: 2, Tokens: usage.Tokens{Input: 150, CacheCreationInput: 25, CachedInput: 50, Output: 25}, UnallocatedTokens: usage.Tokens{Input: 150, CacheCreationInput: 25, CachedInput: 50, Output: 25}, TotalTokens: 175}
	if output.Totals != want {
		t.Fatalf("totals = %+v, want %+v", output.Totals, want)
	}
	unknown := reportJSON(t, "-dir", dir, "-group-by", "requester_id,service")
	if len(unknown.Groups) != 1 || unknown.Groups[0].RequesterID != 0 || unknown.Groups[0].Service != "unknown" || unknown.Groups[0].UnallocatedTokens != want.Tokens {
		t.Fatalf("legacy usage lost unknown attribution: %+v", unknown)
	}
	output = reportJSON(t, "-dir", dir, "-repo", "EXAMPLE-OWNER/EXAMPLE-REPO", "-user", "ALICE", "-from", "2026-10-09T02:00:00+02:00", "-to", "2026-10-10", "-group-by", "day,user")
	if len(output.Groups) != 1 || output.Groups[0].Day != "2026-10-09" || output.Groups[0].User != "alice" || output.Totals.Attempts != 1 || output.Totals.TotalTokens != 55 || output.Totals.Tokens.CacheCreationInput != 5 || output.Totals.IncompleteUsage != 1 {
		t.Fatalf("wrong filtered report: %+v", output)
	}
	if output.Filters.From != "2026-10-09T00:00:00Z" || output.Filters.To != "2026-10-10T00:00:00Z" {
		t.Fatalf("wrong UTC boundaries: %+v", output.Filters)
	}
}

func TestUsageServiceAndRequesterFiltersKeepOriginalDetail(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	record := usage.Record{
		ID: "0123456789abcdef0123456789abcdef", JobID: "job-one", Repo: "linkdata/minion", RepositoryID: 11,
		User: "old-login", RequesterID: 22, Action: "review", Number: 1, Provider: "codex", Model: "main-model",
		CriticProvider: "claude", CriticModel: "critic-model", StartedAt: start, Status: "running",
		ExecutorID: "local", SourceRef: strings.Repeat("a", 40), UsageCoverage: usage.Coverage{Scope: "attempt"}, UsageComplete: true,
	}
	for i, call := range []usage.Call{
		{Ordinal: 1, StartedAt: start.Add(time.Second), Provider: "codex", Model: "main-model", Role: "main", Round: 1},
		{Ordinal: 2, StartedAt: start.Add(2 * time.Second), Provider: "claude", Model: "critic-model", Role: "critic", Round: 1, ReviewPhase: "full"},
	} {
		if _, err := record.BeginCall(call); err != nil {
			t.Fatal(err)
		}
		finish := call.StartedAt.Add(time.Second)
		call.FinishedAt = &finish
		call.Tokens = usage.Tokens{Input: int64(10 * (i + 1)), Output: int64(i + 2)}
		call.UsageCoverage = usage.Coverage{Scope: "invocation"}
		call.Complete = true
		if _, err := record.FinishCall(call); err != nil {
			t.Fatal(err)
		}
	}
	end := start.Add(time.Minute)
	record.Status, record.FinishedAt = "succeeded", &end
	if err := (usage.Store{Dir: dir}).Save(record); err != nil {
		t.Fatal(err)
	}
	output := reportJSON(t, "-dir", dir, "-requester-id", "22", "-service", "codex", "-group-by", "service,requester_id")
	if output.Totals.Attempts != 1 || output.Totals.UsageRecordedActions != 1 || output.Totals.Tokens != (usage.Tokens{Input: 10, Output: 2}) ||
		len(output.Groups) != 1 || output.Groups[0].Service != "codex" || output.Groups[0].RequesterID != 22 || output.Groups[0].Tokens != output.Totals.Tokens {
		t.Fatalf("wrong selected service totals: %+v", output)
	}
	details := reportJSON(t, "-dir", dir, "-requester-id", "22", "-service", "claude", "-group-by", "none")
	if len(details.Attempts) != 1 || details.Attempts[0].SelectedUsage == nil ||
		details.Attempts[0].SelectedUsage.Tokens != (usage.Tokens{Input: 20, Output: 3}) ||
		details.Attempts[0].Tokens != (usage.Tokens{Input: 30, Output: 5}) || details.Totals.TotalTokens != 23 {
		t.Fatalf("filtered detail lost full attempt or selected portion: %+v", details)
	}
	var table, stderr bytes.Buffer
	if code := runUsageWithOutput([]string{"-dir", dir, "-service", "claude", "-group-by", "none"}, &table, &stderr); code != 0 {
		t.Fatalf("filtered table exit %d: %s", code, &stderr)
	}
	lines := strings.SplitN(table.String(), "\n", 3)
	if len(lines) < 2 {
		t.Fatalf("filtered table lacks a row: %s", &table)
	}
	totalAt, usageAt := strings.Index(lines[0], "TOTAL"), strings.Index(lines[0], "USAGE")
	if totalAt < 0 || usageAt <= totalAt || strings.TrimSpace(lines[1][totalAt:usageAt]) != "23" || !strings.Contains(lines[1], "22") {
		t.Fatalf("filtered table shows whole-attempt tokens: %s", &table)
	}
	if got := reportJSON(t, "-dir", dir, "-requester-id", "23"); got.Totals.Attempts != 0 {
		t.Fatalf("wrong requester selected: %+v", got)
	}
}

func TestUsageDetailsAndTables(t *testing.T) {
	dir := t.TempDir()
	saveReportFixture(t, dir)
	output := reportJSON(t, "-dir", dir, "-group-by", "none")
	if len(output.Groups) != 0 || len(output.GroupBy) != 0 || len(output.Attempts) != 4 || output.Attempts[0].TotalTokens != 120 || output.Attempts[1].UsageComplete || output.Attempts[2].UsageComplete {
		t.Fatalf("wrong detail report: %+v", output)
	}
	if attempt := output.Attempts[0]; attempt.ExecutorID != "local" || attempt.SourceRef != strings.Repeat("a", 40) || attempt.UsageSource != "executor_reported" || !strings.Contains(output.TokenNote, "executor-reported claims") {
		t.Fatalf("details lost claim provenance: %+v", attempt)
	}
	if attempt := output.Attempts[1]; attempt.Provider != "claude" || attempt.CriticProvider != "codex" || attempt.CriticModel != "critic-model" || attempt.ClaudeCalls != 3 || !attempt.UsageCoverage.MissingFinalReport {
		t.Fatalf("details lost model roles: %+v", attempt)
	}
	for _, grouping := range []string{"repo,month", "none"} {
		var stdout, stderr bytes.Buffer
		if code := runUsageWithOutput([]string{"-dir", dir, "-group-by", grouping}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d: %s", code, &stderr)
		}
		for _, text := range []string{"example-owner/example-repo", "175 total", "2 attempts with incomplete usage", "included in input", "may omit unknown token counts", "CODEX CALLS", "CLAUDE CALLS", "4 Codex calls, 3 Claude calls"} {
			if !strings.Contains(stdout.String(), text) {
				t.Errorf("%q output lacks %q: %s", grouping, text, &stdout)
			}
		}
		lines := strings.SplitN(stdout.String(), "\n", 3)
		if len(lines) < 2 {
			t.Fatalf("%q output lacks table rows: %s", grouping, &stdout)
		}
		header, first := lines[0], lines[1]
		inputAt, createAt, readAt, outputAt := strings.Index(header, "INPUT"), strings.Index(header, "CACHE CREATE"), strings.Index(header, "CACHE READ"), strings.Index(header, "OUTPUT")
		if inputAt < 0 || createAt <= inputAt || readAt <= createAt || outputAt <= readAt || len(first) < outputAt {
			t.Fatalf("%q cache headings are missing or out of order: %s", grouping, &stdout)
		}
		wantCreate, wantRead := "25", "50"
		if grouping == "none" {
			wantCreate, wantRead = "20", "40"
		}
		if gotCreate, gotRead := strings.TrimSpace(first[createAt:readAt]), strings.TrimSpace(first[readAt:outputAt]); gotCreate != wantCreate || gotRead != wantRead {
			t.Fatalf("%q cache counts are in the wrong columns: creation=%q read=%q", grouping, gotCreate, gotRead)
		}
		if grouping == "none" {
			for _, text := range []string{"succeeded", "failed", "running", "partial", "unknown", "complete", "stale after an interruption", "MAIN", "CRITIC", "claude", "codex", "missing_final_report"} {
				if !strings.Contains(stdout.String(), text) {
					t.Errorf("details lack %q: %s", text, &stdout)
				}
			}
		} else if !strings.Contains(stdout.String(), "MONTH") || !strings.Contains(stdout.String(), "INCOMPLETE") {
			t.Fatalf("missing summary columns: %s", &stdout)
		}
	}
}

func TestUsageConfigAndEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"bot_login":"test","state_path":"custom/state.json","repositories":[{"name":"example-owner/example-repo","id":1,"owner_id":2,"actions":["review"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	output := reportJSON(t, "-config", path)
	if output.Totals.Attempts != 0 || output.Groups == nil || output.Attempts == nil {
		t.Fatalf("wrong empty report: %+v", output)
	}
	saveReportFixture(t, filepath.Join(dir, "custom", "usage"))
	if output := reportJSON(t, "-config", path); output.Totals.Attempts != 4 {
		t.Fatalf("wrong directory derived from state_path: %+v", output)
	}
	if err := os.WriteFile(path, []byte("broken config"), 0600); err != nil {
		t.Fatal(err)
	}
	if output := reportJSON(t, "-dir", filepath.Join(dir, "custom", "usage")); output.Totals.Attempts != 4 {
		t.Fatalf("direct report depends on broken config: %+v", output)
	}
}

func TestUsageDefaultsToLocalConfig(t *testing.T) {
	dir := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile("config.json", []byte("sample cannot be used as live config"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("config.local.json", []byte(`{"bot_login":"example-bot","repositories":[{"name":"example-owner/example-repo","id":1,"owner_id":2,"actions":["review"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if output := reportJSON(t); output.Totals.Attempts != 0 {
		t.Fatalf("unexpected usage from local config: %+v", output)
	}
}

func TestUsageRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"-from", "yesterday"}, {"-to", "2026-02-30"},
		{"-from", "2026-10-09", "-to", "2026-10-09"},
		{"-from", "2026-10-10", "-to", "2026-10-09"},
		{"-group-by", "repo,repo"}, {"-group-by", "week"},
		{"-group-by", "none,repo"}, {"-group-by", ""},
		{"-service", "other"}, {"-requester-id", "-1"}, {"-requester-id", "0"}, {"-requester-id", "bad"},
		{"-dir", ""}, {"-dir", "records", "-config", "config.json"},
		{"-loop"}, {"-check"}, {"extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runUsageWithOutput(args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit = %d, want 2; stderr=%s", code, &stderr)
			}
			if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("unexpected output: stdout=%s, stderr=%s", &stdout, &stderr)
			}
		})
	}
}

func TestUsageReadAndWriteErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, strings.Repeat("0", 32)+".json"), []byte("broken record"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runUsageWithOutput([]string{"-dir", dir}, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
		t.Fatalf("read error: exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	for _, extra := range [][]string{nil, {"-json"}} {
		stderr.Reset()
		if code := runUsageWithOutput(append([]string{"-dir", filepath.Join(dir, "missing")}, extra...), usageFailWriter{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "write report") {
			t.Fatalf("write error: exit=%d stderr=%s", code, &stderr)
		}
	}
}

type usageFailWriter struct{}

func (usageFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
