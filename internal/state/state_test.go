package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStoreLocksAndPersistsCompletedCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	initial := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	store, err := Open(path, initial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, initial); !errors.Is(err, ErrLocked) {
		t.Fatalf("second writer got %v, want ErrLocked", err)
	}
	store.Data.Processed["job"] = initial
	failure := JobFailure{Attempts: 2, ReplyAttempts: 1, ModelCallStarted: true, AcceptedAt: initial, Deadline: initial.Add(time.Hour)}
	store.Data.JobFailures["failed"] = failure
	store.Data.NextInviteCheckAt = initial.Add(time.Hour)
	store.Data.RepositoryCursors["owner/repo"] = initial.Add(time.Minute)
	store.Data.NextPollAt = initial.Add(2 * time.Minute)
	store.Data.PendingNotifications["thread"] = PendingNotification{Repository: "owner/repo", Since: initial}
	pending := PendingFix{SHA: "verified-sha", Cap: &CriticCap{MaxRounds: 5, HasBlockers: true, FullApproved: true}}
	store.Data.PendingFixes["fix-job"] = pending
	store.Data.PendingFixes["normal-fix"] = PendingFix{SHA: "normal-sha"}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, initial.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	got := reopened.Data
	if _, ok := got.Processed["job"]; !ok || got.JobFailures["failed"] != failure || !got.Cursor.Equal(initial) ||
		!got.RepositoryCursors["owner/repo"].Equal(initial.Add(time.Minute)) || !got.NextPollAt.Equal(initial.Add(2*time.Minute)) || !got.NextInviteCheckAt.Equal(initial.Add(time.Hour)) ||
		got.PendingNotifications["thread"] != (PendingNotification{Repository: "owner/repo", Since: initial}) || !reflect.DeepEqual(got.PendingFixes["fix-job"], pending) ||
		got.PendingFixes["normal-fix"] != (PendingFix{SHA: "normal-sha"}) {
		t.Fatalf("lost persisted state: %+v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || runtime.GOOS == "windows" && info.Mode().Perm()&0200 == 0 || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("state mode %v, want writable regular file (0600 on Unix)", info.Mode())
	}
}

func TestTerminalDecisionsPersistAcrossOpen(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path, at)
	if err != nil {
		t.Fatal(err)
	}
	if store.Data.TerminalDecisions == nil {
		t.Fatal("new state has no terminal decision map")
	}
	want := map[string]TerminalDecision{
		"job-success": {AttemptID: "attempt-1", Outcome: "succeeded", DecidedAt: at},
		"job-expiry":  {AttemptID: "attempt-2", Outcome: "expired", DecidedAt: at},
		"job-stop":    {AttemptID: "attempt-3", Outcome: "canceled", DecidedAt: at},
		"job-loss":    {AttemptID: "attempt-4", Outcome: "lost", DecidedAt: at},
		"job-failure": {AttemptID: "attempt-5", Outcome: "failed", DecidedAt: at},
	}
	store.Data.TerminalDecisions = want
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if !reflect.DeepEqual(reopened.Data.TerminalDecisions, want) {
		t.Fatalf("terminal decisions changed after reopen: got=%+v want=%+v", reopened.Data.TerminalDecisions, want)
	}
}

func TestOpenInitializesMissingTerminalDecisions(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	data, err := json.Marshal(Data{Cursor: at})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, at)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.Data.TerminalDecisions == nil {
		t.Fatal("legacy state has no terminal decision map")
	}
}

func TestPublicationWritesPersistWithoutAcceptedAction(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path, at)
	if err != nil {
		t.Fatal(err)
	}
	write := PublicationWrite{
		AttemptID: "attempt-1", Kind: "review", BotLogin: "bot[bot]", BotID: 8, Repository: "owner/repo", Number: 7,
		Marker: "<!-- linkdataminion:job-1 -->", Since: at, StartedAt: at,
		Deadline: at.Add(2 * time.Minute), Status: PublicationWritePending,
	}
	store.Data.PublicationWrites["job-1"] = write
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, at)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := reopened.Data.PublicationWrites["job-1"]; !reflect.DeepEqual(got, write) || reopened.Data.Accepted != nil {
		t.Fatalf("publication write changed after restart: got=%+v accepted=%+v", got, reopened.Data.Accepted)
	}
}

func TestOpenInitializesMissingPublicationWrites(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	data, err := json.Marshal(Data{Cursor: at})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, at)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.Data.PublicationWrites == nil {
		t.Fatal("missing publication writes map")
	}
}

func TestOpenRejectsInvalidPublicationWrite(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	valid := PublicationWrite{AttemptID: "attempt-1", Kind: "comment", BotLogin: "bot", Repository: "owner/repo", Number: 7,
		Marker: "<!-- linkdataminion:job-1 -->", Since: at, StartedAt: at,
		Deadline: at.Add(2 * time.Minute), Status: PublicationWritePending}
	for _, test := range []struct {
		name, jobID string
		change      func(*PublicationWrite)
	}{
		{name: "bad job", jobID: "../job"},
		{name: "missing attempt", jobID: "job-1", change: func(w *PublicationWrite) { w.AttemptID = "" }},
		{name: "bad attempt", jobID: "job-1", change: func(w *PublicationWrite) { w.AttemptID = "../attempt" }},
		{name: "bad kind", jobID: "job-1", change: func(w *PublicationWrite) { w.Kind = "push" }},
		{name: "missing bot login", jobID: "job-1", change: func(w *PublicationWrite) { w.BotLogin = "" }},
		{name: "empty bot account", jobID: "job-1", change: func(w *PublicationWrite) { w.BotLogin = "[bot]" }},
		{name: "bad bot login", jobID: "job-1", change: func(w *PublicationWrite) { w.BotLogin = "owner/bot" }},
		{name: "bad bot ID", jobID: "job-1", change: func(w *PublicationWrite) { w.BotID = -1 }},
		{name: "bad target", jobID: "job-1", change: func(w *PublicationWrite) { w.Repository = "../repo" }},
		{name: "wrong marker", jobID: "job-1", change: func(w *PublicationWrite) { w.Marker = "<!-- linkdataminion:other -->" }},
		{name: "renewed backwards", jobID: "job-1", change: func(w *PublicationWrite) { w.Deadline = at }},
		{name: "invalid status", jobID: "job-1", change: func(w *PublicationWrite) { w.Status = "complete" }},
		{name: "pending with resolution", jobID: "job-1", change: func(w *PublicationWrite) { w.ResolvedAt = at }},
		{name: "observed without resolution", jobID: "job-1", change: func(w *PublicationWrite) { w.Status = PublicationWriteObserved }},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			write := valid
			if test.change != nil {
				test.change(&write)
			}
			data, err := json.Marshal(Data{Cursor: at, PublicationWrites: map[string]PublicationWrite{test.jobID: write}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if store, err := Open(path, at); store != nil || err == nil || !strings.Contains(err.Error(), "invalid publication write") {
				t.Fatalf("Open() = %v, %v; want invalid publication write", store, err)
			}
		})
	}
}

func TestOpenRejectsInvalidTerminalDecision(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	valid := TerminalDecision{AttemptID: "attempt-1", Outcome: "succeeded", DecidedAt: at}
	for _, test := range []struct {
		name, jobID string
		decision    TerminalDecision
	}{
		{name: "missing job ID", decision: valid},
		{name: "invalid job ID", jobID: "../job", decision: valid},
		{name: "missing attempt ID", jobID: "job-1", decision: TerminalDecision{Outcome: "succeeded", DecidedAt: at}},
		{name: "invalid attempt ID", jobID: "job-1", decision: TerminalDecision{AttemptID: "../attempt", Outcome: "succeeded", DecidedAt: at}},
		{name: "unknown outcome", jobID: "job-1", decision: TerminalDecision{AttemptID: "attempt-1", Outcome: "unknown", DecidedAt: at}},
		{name: "missing decision time", jobID: "job-1", decision: TerminalDecision{AttemptID: "attempt-1", Outcome: "succeeded"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			data, err := json.Marshal(Data{Cursor: at, TerminalDecisions: map[string]TerminalDecision{test.jobID: test.decision}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if store, err := Open(path, at); store != nil || err == nil || !strings.Contains(err.Error(), "invalid terminal decision") {
				t.Fatalf("Open() = %v, %v; want invalid terminal decision", store, err)
			}
		})
	}
}

func TestDecideTerminalLockedKeepsFirstOutcomeAcrossRestart(t *testing.T) {
	accepted := acceptedActionFixture()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path, accepted.AcceptedAt)
	if err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted = &accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	var first, second TerminalDecision
	if err := store.WithLock(func() error {
		var err error
		first, err = store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "expired", accepted.Deadline)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithLock(func() error {
		var err error
		second, err = store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "succeeded", accepted.Deadline.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if first != second || first.Outcome != "expired" || !store.Data.Accepted.Terminal {
		t.Fatalf("later outcome changed first terminal decision: first=%+v second=%+v accepted=%+v", first, second, store.Data.Accepted)
	}
	if err := store.WithLock(func() error {
		_, err := store.DecideTerminalLocked(accepted.JobID, "other-attempt", "failed", accepted.Deadline)
		return err
	}); err == nil || store.Data.TerminalDecisions[accepted.JobID] != first {
		t.Fatalf("another attempt reused first decision: err=%v decision=%+v", err, store.Data.TerminalDecisions[accepted.JobID])
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, accepted.AcceptedAt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Data.TerminalDecisions[accepted.JobID] != first || reopened.Data.Accepted == nil || !reopened.Data.Accepted.Terminal {
		t.Fatalf("first terminal decision lost after reopen: decision=%+v accepted=%+v", reopened.Data.TerminalDecisions[accepted.JobID], reopened.Data.Accepted)
	}
}

func TestDecideTerminalLockedRejectsInvalidNewDecision(t *testing.T) {
	accepted := acceptedActionFixture()
	for _, test := range []struct {
		name, jobID, attemptID, outcome string
		at                              time.Time
	}{
		{name: "invalid job", jobID: "../job", attemptID: accepted.AttemptID, outcome: "failed", at: accepted.Deadline},
		{name: "invalid attempt", jobID: accepted.JobID, attemptID: "../attempt", outcome: "failed", at: accepted.Deadline},
		{name: "unknown outcome", jobID: accepted.JobID, attemptID: accepted.AttemptID, outcome: "unknown", at: accepted.Deadline},
		{name: "missing time", jobID: accepted.JobID, attemptID: accepted.AttemptID, outcome: "failed"},
		{name: "different accepted job", jobID: "other-job", attemptID: accepted.AttemptID, outcome: "failed", at: accepted.Deadline},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "state.json"), accepted.AcceptedAt)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			copy := accepted
			store.Data.Accepted = &copy
			if err := store.WithLock(func() error {
				_, err := store.DecideTerminalLocked(test.jobID, test.attemptID, test.outcome, test.at)
				return err
			}); err == nil || copy.Terminal || len(store.Data.TerminalDecisions) != 0 {
				t.Fatalf("invalid terminal decision changed state: err=%v accepted=%+v decisions=%+v", err, copy, store.Data.TerminalDecisions)
			}
		})
	}
}

func TestDecideTerminalLockedKeepsFirstOutcomeAfterFailedSave(t *testing.T) {
	accepted := acceptedActionFixture()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path, accepted.AcceptedAt)
	if err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted = &accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	backup := path + ".saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.WithLock(func() error {
		_, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "expired", accepted.Deadline)
		return err
	}); err == nil || !accepted.Terminal || store.Data.TerminalDecisions[accepted.JobID].Outcome != "expired" {
		t.Fatalf("failed save forgot terminal decision: err=%v accepted=%+v decisions=%+v", err, accepted, store.Data.TerminalDecisions)
	}
	if err := store.WithLock(func() error {
		decision, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "succeeded", accepted.Deadline.Add(time.Minute))
		if err == nil && decision.Outcome != "expired" {
			t.Fatalf("failed save allowed a different in-memory outcome: %+v", decision)
		}
		return err
	}); err == nil {
		t.Fatal("failed save allowed a caller to act on an unconfirmed decision")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if err := store.WithLock(func() error {
		decision, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "succeeded", accepted.Deadline.Add(time.Minute))
		if err == nil && decision.Outcome != "expired" {
			t.Fatalf("retry changed the first decision: %+v", decision)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, accepted.AcceptedAt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Data.Accepted == nil || !reopened.Data.Accepted.Terminal || reopened.Data.TerminalDecisions[accepted.JobID].Outcome != "expired" {
		t.Fatalf("retry did not persist the first decision: accepted=%+v decisions=%+v", reopened.Data.Accepted, reopened.Data.TerminalDecisions)
	}
}

func TestDecideTerminalLockedKeepsFirstOutcomeAfterPostRenameSyncFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows skips directory sync")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	accepted := acceptedActionFixture()
	store, err := Open(path, accepted.AcceptedAt)
	if err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted = &accepted
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	store.syncDir = filepath.Join(dir, "missing-sync")
	if err := store.WithLock(func() error {
		_, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "expired", accepted.Deadline)
		return err
	}); !errors.Is(err, os.ErrNotExist) || !accepted.Terminal || store.Data.TerminalDecisions[accepted.JobID].Outcome != "expired" {
		t.Fatalf("post-rename save error forgot first decision: err=%v state=%+v", err, store.Data)
	}
	store.syncDir = ""
	if err := store.WithLock(func() error {
		decision, err := store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "succeeded", accepted.Deadline.Add(time.Minute))
		if err == nil && decision.Outcome != "expired" {
			t.Fatalf("post-rename error allowed a different outcome: %+v", decision)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, accepted.AcceptedAt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Data.Accepted == nil || !reopened.Data.Accepted.Terminal || reopened.Data.TerminalDecisions[accepted.JobID].Outcome != "expired" {
		t.Fatalf("post-rename decision changed on restart: %+v", reopened.Data)
	}
}

func TestStorePersistsAcrossProcessRestart(t *testing.T) {
	initial := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	const childMarker = "minion-state-restart-child"
	if len(os.Args) >= 3 && os.Args[len(os.Args)-2] == childMarker {
		path := os.Args[len(os.Args)-1]
		store, err := Open(path, initial)
		if err != nil {
			t.Fatal(err)
		}
		store.Data.Processed["job"] = initial
		accepted := acceptedActionFixture()
		accepted.SourceUpdatedAt = time.Time{} // Unknown source time must stay unknown after restart.
		store.Data.Accepted = &accepted
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		if err := store.WriteAcceptedResult([]byte("accepted result after process exit")); err != nil {
			t.Fatal(err)
		}
		return // Process exit releases the lock without Store.Close.
	}
	unrelated := filepath.Join(t.TempDir(), "unrelated-state.json")
	if err := os.WriteFile(unrelated, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINION_STATE_RESTART_TEST", unrelated)
	path := filepath.Join(t.TempDir(), "state.json")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStorePersistsAcrossProcessRestart$", "--", childMarker, path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("state writer process: %v\n%s", err, output)
	}
	if got, err := os.ReadFile(unrelated); err != nil || string(got) != "untouched" {
		t.Fatalf("inherited environment path changed: %q, %v", got, err)
	}
	store, err := Open(path, initial.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	want := acceptedActionFixture()
	want.SourceUpdatedAt = time.Time{}
	if !store.Data.Cursor.Equal(initial) || !store.Data.Processed["job"].Equal(initial) || !reflect.DeepEqual(store.Data.Accepted, &want) {
		t.Fatalf("state lost after process exit: %+v", store.Data)
	}
	if result, err := store.ReadAcceptedResult(1024); err != nil || string(result) != "accepted result after process exit" {
		t.Fatalf("accepted result lost after process exit: %q, %v", result, err)
	}
}

func acceptedActionFixture() AcceptedAction {
	acceptedAt := time.Date(2026, 10, 9, 8, 1, 0, 0, time.UTC)
	return AcceptedAction{
		JobID: "job-1", AttemptID: "attempt-1", ExecutorID: "local",
		Repository: "owner/repo", RepositoryID: 1, OwnerID: 2,
		SubjectType: "PullRequest", SubjectNumber: 7,
		SourceKey: "owner/repo#issue-comment:123", SourceNodeID: "node:123",
		SourceURL: "https://github.com/owner/repo/pull/7#issuecomment-123", SourceUpdatedAt: acceptedAt.Add(-time.Minute),
		Requester: "writer", RequesterID: 3, CommandName: "review", Action: "review",
		AcceptedAt: acceptedAt, Deadline: acceptedAt.Add(time.Hour), Assigned: true,
	}
}

func TestOpenRejectsInvalidAcceptedAction(t *testing.T) {
	tests := []struct {
		name   string
		change func(*AcceptedAction)
	}{
		{"missing job", func(a *AcceptedAction) { a.JobID = "" }},
		{"invalid attempt", func(a *AcceptedAction) { a.AttemptID = "../attempt" }},
		{"missing executor", func(a *AcceptedAction) { a.ExecutorID = "" }},
		{"invalid repository", func(a *AcceptedAction) { a.Repository = "../repo" }},
		{"missing repository ID", func(a *AcceptedAction) { a.RepositoryID = 0 }},
		{"missing owner ID", func(a *AcceptedAction) { a.OwnerID = 0 }},
		{"invalid subject", func(a *AcceptedAction) { a.SubjectType = "Discussion" }},
		{"missing subject number", func(a *AcceptedAction) { a.SubjectNumber = 0 }},
		{"missing source key", func(a *AcceptedAction) { a.SourceKey = "" }},
		{"missing source node ID", func(a *AcceptedAction) { a.SourceNodeID = "" }},
		{"missing requester", func(a *AcceptedAction) { a.Requester = "" }},
		{"missing requester ID", func(a *AcceptedAction) { a.RequesterID = 0 }},
		{"missing command", func(a *AcceptedAction) { a.CommandName = "" }},
		{"unsupported action", func(a *AcceptedAction) { a.Action = "unknown" }},
		{"missing accepted time", func(a *AcceptedAction) { a.AcceptedAt = time.Time{} }},
		{"nonpositive deadline", func(a *AcceptedAction) { a.Deadline = a.AcceptedAt }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accepted := acceptedActionFixture()
			test.change(&accepted)
			path := filepath.Join(t.TempDir(), "state.json")
			data, err := json.Marshal(Data{Cursor: acceptedActionFixture().AcceptedAt, Accepted: &accepted})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if store, err := Open(path, time.Now()); store != nil || err == nil || !strings.Contains(err.Error(), "invalid accepted action") {
				t.Fatalf("Open() = %v, %v; want invalid accepted action", store, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			store, err := Open(path, time.Now())
			if err != nil {
				t.Fatalf("invalid state retained lock: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
	accepted := acceptedActionFixture()
	accepted.SourceURL = "" // GitHub may omit a display URL.
	if err := accepted.Validate(); err != nil {
		t.Fatalf("optional source URL rejected: %v", err)
	}
}

func TestOpenAcceptsFixedReplyActionSubjectPairs(t *testing.T) {
	for _, test := range []struct {
		name, action, subject string
	}{
		{"review on issue", "review", "Issue"},
		{"fix on pull request", "fix", "PullRequest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			accepted := acceptedActionFixture()
			accepted.Action, accepted.CommandName, accepted.SubjectType = test.action, test.action, test.subject
			accepted.SourceURL = ""
			accepted.Assigned = false
			path := filepath.Join(t.TempDir(), "state.json")
			store, err := Open(path, accepted.AcceptedAt)
			if err != nil {
				t.Fatal(err)
			}
			store.Data.Accepted = &accepted
			if err := store.Save(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
			}()
			if !reflect.DeepEqual(reopened.Data.Accepted, &accepted) {
				t.Fatalf("fixed reply acceptance lost: %+v", reopened.Data.Accepted)
			}
		})
	}
}

func TestJobFailureOmitsUnsetOriginalDeadline(t *testing.T) {
	data, err := json.Marshal(JobFailure{Attempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"attempts":1}` {
		t.Fatalf("unset deadline encoded as %s", data)
	}
}

func TestSaveReportsPostRenameSyncFailureWithAcceptedRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows skips directory sync")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	accepted := acceptedActionFixture()
	store, err := Open(path, accepted.AcceptedAt)
	if err != nil {
		t.Fatal(err)
	}
	store.Data.Accepted = &accepted
	store.syncDir = filepath.Join(dir, "missing")
	if err := store.Save(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Save() = %v; want post-rename sync-directory error", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	if !reflect.DeepEqual(reopened.Data.Accepted, &accepted) || !reopened.Data.Accepted.Assigned {
		t.Fatalf("post-rename error lost accepted assignment: %+v", reopened.Data.Accepted)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
		t.Fatalf("temporary state file retained after sync error: %v, %v", entries, err)
	}
}

func TestStateDirectorySyncFailureRequiresRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows supports process-restart recovery only")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "new", "nested")
	wantFirst := []string{filepath.Join(root, "new"), root}
	syncFailure := errors.New("parent sync failed")
	var first []string
	err := prepareStateDirectory(dir, func(parents ...string) error {
		first = append(first, parents...)
		return syncFailure
	})
	if !errors.Is(err, syncFailure) || len(first) < len(wantFirst) || !reflect.DeepEqual(first[:len(wantFirst)], wantFirst) {
		t.Fatalf("first directory preparation = %v, parents %v; want sync failure after %v", err, first, wantFirst)
	}
	var retried []string
	if err := prepareStateDirectory(dir, func(parents ...string) error {
		retried = append(retried, parents...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(retried, first) {
		t.Fatalf("retry skipped existing directory parents: got %v, want %v", retried, first)
	}
	store, err := Open(filepath.Join(dir, "state.json"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStateDirectorySyncsSymlinkTargetParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows supports process-restart recovery only")
	}
	root := t.TempDir()
	aliasParent := filepath.Join(root, "alias")
	realParent := filepath.Join(root, "real")
	for _, dir := range []string{aliasParent, realParent} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(aliasParent, "link")
	if err := os.Symlink(realParent, link); err != nil {
		t.Fatal(err)
	}
	resolvedRealParent, err := filepath.EvalSymlinks(realParent)
	if err != nil {
		t.Fatal(err)
	}
	var synced []string
	if err := prepareStateDirectory(filepath.Join(link, "new"), func(parents ...string) error {
		synced = append(synced, parents...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(synced, aliasParent) || !slices.Contains(synced, resolvedRealParent) {
		t.Fatalf("state path parents not both synced: %v", synced)
	}
}

func TestOpenFailureReleasesLock(t *testing.T) {
	for name, content := range map[string]string{
		"malformed":         "{",
		"trailing data":     `{"cursor":"2026-10-09T08:00:00Z"} {}`,
		"missing cursor":    `{}`,
		"unknown field":     `{"cursor":"2026-10-09T08:00:00Z","unsupported":true}`,
		"old pending fix":   `{"cursor":"2026-10-09T08:00:00Z","pending_fixes":{"job":"old-sha"}}`,
		"unknown cap field": `{"cursor":"2026-10-09T08:00:00Z","pending_fixes":{"job":{"sha":"sha","cap":{"max_rounds":3,"unsupported":true}}}}`,
		"missing deadline":  `{"cursor":"2026-10-09T08:00:00Z","job_failures":{"job":{"attempts":1,"accepted_at":"2026-10-09T08:00:00Z"}}}`,
		"missing accepted":  `{"cursor":"2026-10-09T08:00:00Z","job_failures":{"job":{"attempts":1,"deadline":"2026-10-09T09:00:00Z"}}}`,
		"reversed deadline": `{"cursor":"2026-10-09T08:00:00Z","job_failures":{"job":{"attempts":1,"accepted_at":"2026-10-09T08:00:00Z","deadline":"2026-10-09T07:00:00Z"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if store, err := Open(path, time.Now()); err == nil || store != nil {
				t.Fatalf("Open() = %v, %v; want failure", store, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			store, err := Open(path, time.Now())
			if err != nil {
				t.Fatalf("state lock retained after failure: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSaveEncodingFailurePreservesStateAndCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, err := Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Data.Cursor = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	var marshalErr *json.MarshalerError
	if err := store.Save(); !errors.As(err, &marshalErr) {
		t.Fatalf("Save() = %v; want JSON encoding error", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed save changed state: %s, %v", after, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("temporary state file retained: %v, %v", files, err)
	}
}

func TestSaveReportsRenameFailureAndCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "occupied")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	store := Store{path: path, Data: Data{Cursor: time.Now().UTC()}}
	if err := store.Save(); err == nil {
		t.Fatal("Save() replaced a non-empty directory")
	}
	if data, err := os.ReadFile(filepath.Join(path, "child")); err != nil || string(data) != "keep" {
		t.Fatalf("failed save changed target: %q, %v", data, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 || entries[0].Name() != "occupied" {
		t.Fatalf("temporary state file retained: %v, %v", entries, err)
	}
}
