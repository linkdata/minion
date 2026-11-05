package action

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/state"
)

func historyAccepted(number int, requesterID int64, at time.Time) *state.AcceptedAction {
	return &state.AcceptedAction{JobID: fmt.Sprintf("job-%d", number), AttemptID: fmt.Sprintf("attempt-%d", number),
		ExecutorID: "local", Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
		SubjectType: "Issue", SubjectNumber: number, SourceKey: fmt.Sprintf("source-%d", number),
		SourceNodeID: fmt.Sprintf("node-%d", number), SourceURL: fmt.Sprintf("https://github.com/owner/repo/issues/%d", number),
		Requester: "writer", RequesterID: requesterID, CommandName: "comment", Action: "comment",
		MainProvider: "codex", MainSettings: config.RoleModelConfig{Model: "o3", Effort: "high"},
		AcceptedAt: at, Deadline: at.Add(time.Hour)}
}

func TestUserHistoryRetainsRetryAndPreExecutionFailureAfterCleanup(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	store := dashboardStore(t, at)
	owner := &Orchestrator{Store: store}
	preExecution := historyAccepted(1, 3, at)
	store.Data.Accepted = preExecution
	if _, err := store.DecideTerminalLocked(preExecution.JobID, preExecution.AttemptID, "failed", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted = nil
	retry := historyAccepted(2, 3, at.Add(time.Minute))
	store.Data.JobFailures[retry.JobID] = state.JobFailure{Attempts: 0, AcceptedAt: retry.AcceptedAt,
		Deadline: retry.Deadline, MainProvider: retry.MainProvider, MainSettings: retry.MainSettings,
		RetryAction: state.RetryAction{AttemptID: retry.AttemptID, Summary: retry.HistorySummary()}}
	other := historyAccepted(3, 4, at.Add(2*time.Minute))
	store.Data.Accepted = other
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	page, err := owner.UserHistory(3, "")
	if err != nil || len(page.Entries) != 2 || page.Entries[0].Outcome != "Retry pending" ||
		page.Entries[1].Outcome != "failed" || page.Next != "" ||
		page.Entries[1].Summary.MainSettings.Model != "o3" || page.Entries[1].Summary.SourceKey != preExecution.SourceKey {
		t.Fatalf("own durable history: page=%+v err=%v", page, err)
	}
	if _, found, err := owner.UserActionIdentity(other.AttemptID, 3); err != nil || found {
		t.Fatalf("other requester identity: found=%t err=%v", found, err)
	}
	for _, attempt := range []string{preExecution.AttemptID, retry.AttemptID} {
		summary, found, err := owner.UserActionIdentity(attempt, 3)
		if err != nil || !found || summary.RequesterID != 3 {
			t.Fatalf("own identity %s: summary=%+v found=%t err=%v", attempt, summary, found, err)
		}
		detail, found, err := owner.UserActionDetail(attempt, at.Add(3*time.Minute), summary)
		if err != nil || !found || detail.Summary.RequesterID != 3 || detail.Summary.SourceKey == "" {
			t.Fatalf("own detail %s: detail=%+v found=%t err=%v", attempt, detail, found, err)
		}
	}
	if _, found, err := owner.UserActionDetail(preExecution.AttemptID, at, other.HistorySummary()); err == nil || found {
		t.Fatalf("changed authorization read private detail: found=%t err=%v", found, err)
	}
	if _, err := owner.UserHistory(3, "bogus"); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

func TestUserHistoryOmitsLegacyDecisionWithoutRequesterProof(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := fmt.Sprintf(`{"cursor":%q,"processed":{},"terminal_decisions":{"job-legacy":{"attempt_id":"attempt-legacy","outcome":"failed","decided_at":%q}}}`,
		at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(path, at)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if store != nil {
			_ = store.Close()
		}
	})
	store.Data.Accepted = historyAccepted(1, 3, at.Add(time.Minute))
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = state.Open(path, at)
	if err != nil {
		t.Fatal(err)
	}
	owner := &Orchestrator{Store: store}
	detail, found, err := owner.DashboardActionDetail("attempt-legacy", at.Add(2*time.Minute))
	if err != nil || !found || detail.JobID != "job-legacy" || detail.Outcome != "failed" {
		t.Fatalf("legacy admin detail: detail=%+v found=%t err=%v", detail, found, err)
	}
	page, err := owner.UserHistory(3, "")
	if err != nil || len(page.Entries) != 1 || page.Entries[0].AttemptID != "attempt-1" {
		t.Fatalf("legacy decision leaked or blocked history: page=%+v err=%v", page, err)
	}
	if _, found, err := owner.UserActionIdentity("attempt-legacy", 3); err != nil || found {
		t.Fatalf("legacy decision became requester proof: found=%t err=%v", found, err)
	}
}

func TestUserHistoryStableBoundedPages(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	store := dashboardStore(t, at)
	owner := &Orchestrator{Store: store}
	for i := 1; i <= 43; i++ {
		accepted := historyAccepted(i, 3, at.Add(time.Duration(i%5)*time.Nanosecond))
		store.Data.Accepted = accepted
		if _, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "succeeded", at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	store.Data.Accepted = nil
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	cursor := ""
	for pageNumber := 0; ; pageNumber++ {
		page, err := owner.UserHistory(3, cursor)
		if err != nil || len(page.Entries) > userHistoryPageSize || pageNumber > 6 {
			t.Fatalf("page %d: count=%d err=%v", pageNumber, len(page.Entries), err)
		}
		for _, entry := range page.Entries {
			if seen[entry.AttemptID] {
				t.Fatalf("duplicate attempt %s", entry.AttemptID)
			}
			seen[entry.AttemptID] = true
		}
		if page.Next == "" {
			break
		}
		if strings.Contains(page.Next, "attempt-") {
			t.Fatalf("cursor revealed attempt identity: %q", page.Next)
		}
		cursor = page.Next
	}
	if len(seen) != 43 {
		t.Fatalf("history lost entries: %d", len(seen))
	}
}

func TestUserHistoryCursorSurvivesAttemptReplacement(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	store := dashboardStore(t, at)
	owner := &Orchestrator{Store: store}
	for i := 1; i <= userHistoryPageSize+1; i++ {
		accepted := historyAccepted(i, 3, at)
		store.Data.Accepted = accepted
		if _, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "failed", at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	store.Data.Accepted = nil
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	first, err := owner.UserHistory(3, "")
	if err != nil || first.Next == "" {
		t.Fatalf("first page: page=%+v err=%v", first, err)
	}
	second, err := owner.UserHistory(3, first.Next)
	if err != nil || len(second.Entries) != 1 {
		t.Fatalf("second page: page=%+v err=%v", second, err)
	}
	jobID := second.Entries[0].JobID
	decision := store.Data.TerminalDecisions[jobID]
	decision.AttemptID = "retry-1"
	store.Data.TerminalDecisions[jobID] = decision
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	second, err = owner.UserHistory(3, first.Next)
	if err != nil || len(second.Entries) != 1 || second.Entries[0].JobID != jobID || second.Entries[0].AttemptID != "retry-1" {
		t.Fatalf("attempt replacement moved past cursor: page=%+v err=%v", second, err)
	}
}
