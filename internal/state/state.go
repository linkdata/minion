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

type Data struct {
	Cursor            time.Time            `json:"cursor"`
	NextPollAt        time.Time            `json:"next_poll_at,omitempty"`
	NextInviteCheckAt time.Time            `json:"next_invite_check_at,omitempty"`
	Processed         map[string]time.Time `json:"processed"`
	PendingFixes      map[string]string    `json:"pending_fixes,omitempty"`
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
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
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
		store.Data = Data{Cursor: initialCursor.UTC(), Processed: map[string]time.Time{}, PendingFixes: map[string]string{}}
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
	err = f.Chmod(0600)
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
