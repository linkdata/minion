package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	store.Data.JobFailures["failed"] = JobFailure{Attempts: 2, ReplyAttempts: 1}
	store.Data.NextInviteCheckAt = initial.Add(time.Hour)
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
	if _, ok := reopened.Data.Processed["job"]; !ok || reopened.Data.JobFailures["failed"] != (JobFailure{Attempts: 2, ReplyAttempts: 1}) || !reopened.Data.Cursor.Equal(initial) || !reopened.Data.NextInviteCheckAt.Equal(initial.Add(time.Hour)) {
		t.Fatalf("lost persisted state: %+v", reopened.Data)
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
		"malformed":      "{",
		"trailing data":  `{"cursor":"2026-10-09T08:00:00Z"} {}`,
		"missing cursor": `{}`,
		"unknown field":  `{"cursor":"2026-10-09T08:00:00Z","unsupported":true}`,
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

func TestOpenRetiresPendingReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	initial := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	saved := map[string]any{
		"cursor":                initial,
		"repository_cursors":    map[string]time.Time{"owner/repo": initial.Add(time.Minute)},
		"next_poll_at":          initial.Add(2 * time.Minute),
		"next_invite_check_at":  initial.Add(time.Hour),
		"processed":             map[string]time.Time{"completed": initial},
		"job_failures":          map[string]JobFailure{"release-job": {Attempts: 1}, "failed": {Attempts: 2}},
		"pending_notifications": map[string]PendingNotification{"thread": {Repository: "owner/repo", Since: initial}},
		"pending_fixes":         map[string]string{"fix-job": "verified-sha"},
		"pending_releases": map[string]any{
			"release-job": map[string]string{"tag": "v1", "target": "old-sha", "notes": "Do not publish these notes."},
			"completed":   map[string]string{"tag": "v2"},
		},
	}
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		store, err := Open(path, initial.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		got := store.Data
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if !got.Cursor.Equal(initial) || !got.RepositoryCursors["owner/repo"].Equal(initial.Add(time.Minute)) ||
			!got.NextPollAt.Equal(initial.Add(2*time.Minute)) || !got.NextInviteCheckAt.Equal(initial.Add(time.Hour)) ||
			!got.Processed["completed"].Equal(initial) || got.Processed["release-job"].IsZero() || len(got.Processed) != 2 ||
			len(got.JobFailures) != 1 || got.JobFailures["failed"].Attempts != 2 ||
			len(got.PendingNotifications) != 1 || !got.PendingNotifications["thread"].Since.Equal(initial) ||
			len(got.PendingFixes) != 1 || got.PendingFixes["fix-job"] != "verified-sha" {
			t.Fatalf("retirement changed unrelated state: %+v", got)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("pending_releases")) || bytes.Contains(data, []byte("Do not publish these notes.")) {
			t.Fatalf("retired checkpoint retained: %s", data)
		}
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
