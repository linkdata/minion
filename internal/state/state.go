package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
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
	PendingFixes         map[string]string              `json:"pending_fixes,omitempty"`
}

// PendingNotification retains a failed thread after its repository cursor advances.
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
	Attempts      int `json:"attempts"`
	ReplyAttempts int `json:"reply_attempts,omitempty"`
}

type Store struct {
	path string
	lock *os.File
	Data Data
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
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = ErrLocked
		}
		return nil, errors.Join(err, lock.Close())
	}
	store := &Store{path: path, lock: lock}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		store.Data = Data{Cursor: initialCursor.UTC(), RepositoryCursors: map[string]time.Time{}, Processed: map[string]time.Time{}, JobFailures: map[string]JobFailure{}, PendingNotifications: map[string]PendingNotification{}, PendingFixes: map[string]string{}}
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
		store.Data.PendingFixes = map[string]string{}
	}
	return store, nil
}

// Save atomically replaces state.
func (s *Store) Save() (err error) {
	dir := filepath.Dir(s.path)
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if removeErr := os.Remove(tmp); !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	err = f.Chmod(0o600)
	if err == nil {
		err = encoder.Encode(s.Data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if d, openErr := os.Open(dir); openErr == nil {
		d.Sync()  //nolint:errcheck // -- Directory sync is best effort after replacement; some filesystems do not support it.
		d.Close() //nolint:errcheck // -- The read-only directory has no pending writes; its sync is best effort.
	}
	return
}

func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	closeErr := s.lock.Close()
	s.lock = nil
	if err != nil {
		return err
	}
	return closeErr
}
