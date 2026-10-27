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
