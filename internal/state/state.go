package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/workspace"
)

var ErrLocked = errors.New("another minion process holds the state lock")

// Data holds durable monitor state.
type Data struct {
	// Cursor seeds repositories without a saved cursor.
	Cursor time.Time `json:"cursor"`
	// RepositoryCursors tracks each repository's durable notification cursor.
	// Missing entries start at Cursor when that repository is first polled.
	RepositoryCursors    map[string]time.Time           `json:"repository_cursors,omitempty"`
	NextPollAt           time.Time                      `json:"next_poll_at,omitempty"`
	NextInviteCheckAt    time.Time                      `json:"next_invite_check_at,omitempty"`
	Processed            map[string]time.Time           `json:"processed"`
	JobFailures          map[string]JobFailure          `json:"job_failures,omitempty"`
	PendingNotifications map[string]PendingNotification `json:"pending_notifications,omitempty"`
	PendingFixes         map[string]PendingFix          `json:"pending_fixes,omitempty"`
	Accepted             *AcceptedAction                `json:"accepted,omitempty"`
}

// AcceptedAction retains the one selected action across process restarts.
type AcceptedAction struct {
	JobID           string    `json:"job_id"`
	AttemptID       string    `json:"attempt_id"`
	ExecutorID      string    `json:"executor_id"`
	Repository      string    `json:"repository"`
	RepositoryID    int64     `json:"repository_id"`
	OwnerID         int64     `json:"owner_id"`
	SubjectType     string    `json:"subject_type"`
	SubjectNumber   int       `json:"subject_number"`
	SourceKey       string    `json:"source_key"`
	SourceNodeID    string    `json:"source_node_id"`
	SourceURL       string    `json:"source_url"`
	SourceUpdatedAt time.Time `json:"source_updated_at"`
	Requester       string    `json:"requester"`
	RequesterID     int64     `json:"requester_id"`
	CommandName     string    `json:"command_name"`
	Action          string    `json:"action"`
	AcceptedAt      time.Time `json:"accepted_at"`
	Deadline        time.Time `json:"deadline"`
	RetryAt         time.Time `json:"retry_at,omitzero"`
	Assigned        bool      `json:"assigned"`
	Terminal        bool      `json:"terminal,omitempty"`
}

// Validate checks the persisted action identity and absolute runtime window.
func (a AcceptedAction) Validate() error {
	for _, value := range []string{a.JobID, a.AttemptID, a.ExecutorID, strings.TrimSuffix(a.Requester, "[bot]"), a.CommandName} {
		if !workspace.ValidIdentitySegment(value) {
			return errors.New("accepted action identity is missing or invalid")
		}
	}
	if !workspace.ValidRepository(a.Repository) || a.RepositoryID <= 0 || a.OwnerID <= 0 || a.RequesterID <= 0 || a.SubjectNumber <= 0 ||
		(a.SubjectType != "Issue" && a.SubjectType != "PullRequest") {
		return errors.New("accepted action repository or requester identity is invalid")
	}
	if a.SourceKey == "" || a.SourceNodeID == "" {
		return errors.New("accepted action source is missing")
	}
	switch a.Action {
	case "review", "fix", "comment", "security-audit":
	default:
		return errors.New("accepted action is unsupported")
	}
	if a.AcceptedAt.IsZero() || !a.Deadline.After(a.AcceptedAt) {
		return errors.New("accepted action deadline is invalid")
	}
	return nil
}

// PendingFix retains the commit and its publication caveat before a push.
type PendingFix struct {
	SHA string     `json:"sha"`
	Cap *CriticCap `json:"cap,omitempty"`
}

// CriticCap preserves the original final-candidate facts for publication retries.
type CriticCap struct {
	MaxRounds    int  `json:"max_rounds"`
	HasBlockers  bool `json:"has_blockers"`
	FullApproved bool `json:"full_approved"`
}

// PendingNotification retains a failed or capacity-deferred thread for replay.
type PendingNotification struct {
	Repository   string    `json:"repository"`
	RepositoryID int64     `json:"repository_id"`
	OwnerID      int64     `json:"owner_id"`
	SubjectType  string    `json:"subject_type"`
	SubjectURL   string    `json:"subject_url"`
	Since        time.Time `json:"since"`
	UpdatedAt    time.Time `json:"updated_at"`
	RetryAt      time.Time `json:"retry_at"`
	Failures     int       `json:"failures"`
}

type JobFailure struct {
	Attempts         int       `json:"attempts"`
	ReplyAttempts    int       `json:"reply_attempts,omitempty"`
	ModelCallStarted bool      `json:"model_call_started,omitempty"`
	AcceptedAt       time.Time `json:"accepted_at,omitzero"`
	Deadline         time.Time `json:"deadline,omitzero"`
}

// Validate checks the optional original action deadline.
func (f JobFailure) Validate() error {
	if f.AcceptedAt.IsZero() != f.Deadline.IsZero() || !f.Deadline.IsZero() && !f.Deadline.After(f.AcceptedAt) {
		return errors.New("job failure deadline is invalid")
	}
	return nil
}

type Store struct {
	path    string
	lock    *os.File
	syncDir string // Test-only extra directory for a post-rename sync failure.
	Data    Data
}

// Open loads state while holding an exclusive lock until [Store.Close].
//
// The lock prevents overlapping cron invocations from executing the same command.
func Open(path string, initialCursor time.Time) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(lock); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	store := &Store{path: path, lock: lock}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		store.Data = Data{Cursor: initialCursor.UTC(), RepositoryCursors: map[string]time.Time{}, Processed: map[string]time.Time{}, JobFailures: map[string]JobFailure{}, PendingNotifications: map[string]PendingNotification{}, PendingFixes: map[string]PendingFix{}}
		if err := store.Save(); err != nil {
			return nil, errors.Join(err, store.Close())
		}
		return store, nil
	}
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&store.Data); err != nil {
		err = fmt.Errorf("decode state: %w", err)
	}
	if err == nil {
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			err = fmt.Errorf("decode state: trailing data")
		}
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	if store.Data.Cursor.IsZero() {
		return nil, errors.Join(fmt.Errorf("state cursor is missing"), store.Close())
	}
	if store.Data.Accepted != nil {
		if err := store.Data.Accepted.Validate(); err != nil {
			return nil, errors.Join(fmt.Errorf("invalid accepted action: %w", err), store.Close())
		}
	}
	for jobID, failure := range store.Data.JobFailures {
		if err := failure.Validate(); err != nil {
			return nil, errors.Join(fmt.Errorf("invalid job failure %q: %w", jobID, err), store.Close())
		}
	}
	if store.Data.Processed == nil {
		store.Data.Processed = map[string]time.Time{}
	}
	if store.Data.RepositoryCursors == nil {
		store.Data.RepositoryCursors = map[string]time.Time{}
	}
	if store.Data.JobFailures == nil {
		store.Data.JobFailures = map[string]JobFailure{}
	}
	if store.Data.PendingNotifications == nil {
		store.Data.PendingNotifications = map[string]PendingNotification{}
	}
	if store.Data.PendingFixes == nil {
		store.Data.PendingFixes = map[string]PendingFix{}
	}
	return store, nil
}

// SyncPathAncestors syncs a selected worker's state path entries before it can
// accept work on Unix. Windows provides process-restart recovery only.
func (s *Store) SyncPathAncestors() error {
	return prepareStateDirectory(filepath.Dir(s.path), workspace.SyncDirectories)
}

// prepareStateDirectory persists the path to selected state before acceptance.
// Syncing every ancestor also repairs a previous startup whose sync failed
// after creating a directory. SyncDirectories is a no-op on Windows.
func prepareStateDirectory(dir string, syncDirectories func(...string) error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	paths := []string{dir}
	if runtime.GOOS != "windows" {
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		if resolved != dir {
			paths = append(paths, resolved)
		}
	}
	var parents []string
	for _, path := range paths {
		for current := path; ; {
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			parents = append(parents, parent)
			current = parent
		}
	}
	return syncDirectories(parents...)
}

// Save atomically replaces state and reports failed durability steps.
func (s *Store) Save() error {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(s.Data); err != nil {
		return err
	}
	if s.syncDir != "" {
		return workspace.WriteAtomic(s.path, data.Bytes(), s.syncDir)
	}
	return workspace.WriteAtomic(s.path, data.Bytes())
}

func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	err := unlockFile(s.lock)
	closeErr := s.lock.Close()
	s.lock = nil
	if err != nil {
		return err
	}
	return closeErr
}
