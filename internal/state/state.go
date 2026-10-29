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
	"sync"
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
	RepositoryCursors     map[string]time.Time           `json:"repository_cursors,omitempty"`
	NextPollAt            time.Time                      `json:"next_poll_at,omitempty"`
	NextInviteCheckAt     time.Time                      `json:"next_invite_check_at,omitempty"`
	Processed             map[string]time.Time           `json:"processed"`
	JobFailures           map[string]JobFailure          `json:"job_failures,omitempty"`
	PendingNotifications  map[string]PendingNotification `json:"pending_notifications,omitempty"`
	PendingFixes          map[string]PendingFix          `json:"pending_fixes,omitempty"`
	PublicationWrites     map[string]PublicationWrite    `json:"publication_writes,omitempty"`
	PublicationScanCursor string                         `json:"publication_scan_cursor,omitempty"`
	TerminalDecisions     map[string]TerminalDecision    `json:"terminal_decisions,omitempty"`
	Accepted              *AcceptedAction                `json:"accepted,omitempty"`
}

const (
	PublicationWritePending   = "pending"
	PublicationWriteObserved  = "observed"
	PublicationWriteUncertain = "uncertain"
)

// PublicationWrite tracks one GitHub write independently of the action's terminal decision.
type PublicationWrite struct {
	AttemptID  string    `json:"attempt_id"`
	Kind       string    `json:"kind"`
	BotLogin   string    `json:"bot_login"`
	BotID      int64     `json:"bot_id"`
	Repository string    `json:"repository"`
	Number     int       `json:"number"`
	Marker     string    `json:"marker"`
	Branch     string    `json:"branch,omitempty"`
	SHA        string    `json:"sha,omitempty"`
	Since      time.Time `json:"since"`
	StartedAt  time.Time `json:"started_at"`
	Deadline   time.Time `json:"deadline"`
	Status     string    `json:"status"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
}

// ValidPublicationBotLogin accepts the bounded account identity stored with a write.
func ValidPublicationBotLogin(login string) bool {
	return workspace.ValidIdentitySegment(strings.TrimSuffix(login, "[bot]"))
}

func validPublicationWrite(jobID string, write PublicationWrite) bool {
	if !workspace.ValidIdentitySegment(jobID) || !workspace.ValidIdentitySegment(write.AttemptID) ||
		!ValidPublicationBotLogin(write.BotLogin) || write.BotID <= 0 ||
		!workspace.ValidRepository(write.Repository) || write.Number <= 0 ||
		write.StartedAt.IsZero() || !write.Deadline.After(write.StartedAt) {
		return false
	}
	markerJobID := jobID
	switch write.Kind {
	case "comment", "review":
		if write.Branch != "" || write.SHA != "" {
			return false
		}
	case "push":
		if !strings.HasSuffix(jobID, "-push") {
			return false
		}
		markerJobID = strings.TrimSuffix(jobID, "-push")
		owner, _, _ := strings.Cut(write.Repository, "/")
		if len(markerJobID) < 12 || !workspace.ValidIdentitySegment(markerJobID) ||
			!strings.EqualFold(owner, strings.TrimSuffix(write.BotLogin, "[bot]")) ||
			write.Branch != fmt.Sprintf("minion/fix-%d-%s", write.Number, markerJobID[:12]) ||
			!workspace.ValidSHA(write.SHA) {
			return false
		}
	default:
		return false
	}
	if write.Marker != "<!-- linkdataminion:"+markerJobID+" -->" {
		return false
	}
	switch write.Status {
	case PublicationWritePending:
		return write.ResolvedAt.IsZero()
	case PublicationWriteObserved, PublicationWriteUncertain:
		return !write.ResolvedAt.IsZero()
	default:
		return false
	}
}

// TerminalDecision records the first outcome for one accepted attempt.
type TerminalDecision struct {
	AttemptID string    `json:"attempt_id"`
	Outcome   string    `json:"outcome"`
	DecidedAt time.Time `json:"decided_at"`
}

func validTerminalDecision(jobID string, decision TerminalDecision) bool {
	switch decision.Outcome {
	case "succeeded", "expired", "canceled", "lost", "failed":
	default:
		return false
	}
	return workspace.ValidIdentitySegment(jobID) && workspace.ValidIdentitySegment(decision.AttemptID) &&
		!decision.DecidedAt.IsZero()
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
	ResultReady     bool      `json:"result_ready,omitempty"`
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
	NotificationKey  string    `json:"notification_key,omitempty"`
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
	mu      sync.Mutex
	syncDir string // Test-only extra directory for a post-rename sync failure.
	Data    Data
}

// WithLock serializes in-process access to Data and saves while an action runs beside controls.
func (s *Store) WithLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

// DecideTerminalLocked persists the first terminal outcome while the caller holds WithLock.
func (s *Store) DecideTerminalLocked(jobID, attemptID, outcome string, at time.Time) (TerminalDecision, error) {
	if decision, exists := s.Data.TerminalDecisions[jobID]; exists {
		if decision.AttemptID != attemptID {
			return TerminalDecision{}, fmt.Errorf("terminal decision for %q belongs to another attempt", jobID)
		}
		// A previous save may have failed before or after replacement. Persist the
		// winning decision again before any caller acts on it.
		if err := s.Save(); err != nil {
			return TerminalDecision{}, fmt.Errorf("save terminal decision: %w", err)
		}
		return decision, nil
	}
	decision := TerminalDecision{AttemptID: attemptID, Outcome: outcome, DecidedAt: at.UTC()}
	if !validTerminalDecision(jobID, decision) {
		return TerminalDecision{}, errors.New("invalid terminal decision")
	}
	accepted := s.Data.Accepted
	if accepted == nil || accepted.JobID != jobID || accepted.AttemptID != attemptID {
		return TerminalDecision{}, fmt.Errorf("terminal decision for %q has no matching accepted attempt", jobID)
	}
	if s.Data.TerminalDecisions == nil {
		s.Data.TerminalDecisions = map[string]TerminalDecision{}
	}
	accepted.Terminal = true
	s.Data.TerminalDecisions[jobID] = decision
	if err := s.Save(); err != nil {
		// A directory-sync error can follow replacement of the state file.
		// Keep the decision in memory so this process cannot overwrite it.
		return TerminalDecision{}, fmt.Errorf("save terminal decision: %w", err)
	}
	return decision, nil
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
		store.Data = Data{Cursor: initialCursor.UTC(), RepositoryCursors: map[string]time.Time{}, Processed: map[string]time.Time{}, JobFailures: map[string]JobFailure{}, PendingNotifications: map[string]PendingNotification{}, PendingFixes: map[string]PendingFix{}, PublicationWrites: map[string]PublicationWrite{}, TerminalDecisions: map[string]TerminalDecision{}}
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
	if store.Data.PublicationScanCursor != "" && !workspace.ValidIdentitySegment(store.Data.PublicationScanCursor) {
		return nil, errors.Join(fmt.Errorf("invalid publication scan cursor"), store.Close())
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
	for jobID, decision := range store.Data.TerminalDecisions {
		if !validTerminalDecision(jobID, decision) {
			return nil, errors.Join(fmt.Errorf("invalid terminal decision %q", jobID), store.Close())
		}
	}
	for jobID, write := range store.Data.PublicationWrites {
		if !validPublicationWrite(jobID, write) {
			return nil, errors.Join(fmt.Errorf("invalid publication write %q", jobID), store.Close())
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
	if store.Data.PublicationWrites == nil {
		store.Data.PublicationWrites = map[string]PublicationWrite{}
	}
	if store.Data.TerminalDecisions == nil {
		store.Data.TerminalDecisions = map[string]TerminalDecision{}
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
