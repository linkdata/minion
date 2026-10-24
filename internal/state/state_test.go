package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	store.Data.JobFailures["failed"] = JobFailure{Attempts: 2, ReplyAttempts: 1, ModelCallStarted: true}
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
	if _, ok := got.Processed["job"]; !ok || got.JobFailures["failed"] != (JobFailure{Attempts: 2, ReplyAttempts: 1, ModelCallStarted: true}) || !got.Cursor.Equal(initial) ||
		!got.RepositoryCursors["owner/repo"].Equal(initial.Add(time.Minute)) || !got.NextPollAt.Equal(initial.Add(2*time.Minute)) || !got.NextInviteCheckAt.Equal(initial.Add(time.Hour)) ||
		got.PendingNotifications["thread"] != (PendingNotification{Repository: "owner/repo", Since: initial}) || !reflect.DeepEqual(got.PendingFixes["fix-job"], pending) ||
		got.PendingFixes["normal-fix"] != (PendingFix{SHA: "normal-sha"}) {
		t.Fatalf("lost persisted state: %+v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state mode %o, want 0600", info.Mode().Perm())
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
