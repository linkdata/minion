package state

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linkdata/minion/internal/config"
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
	LastStopJobID         string                         `json:"last_stop_job_id,omitempty"`
	Accepted              *AcceptedAction                `json:"accepted,omitempty"`
	AcceptedTotal         uint64                         `json:"accepted_total,omitempty"`
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
	SourceURL  string    `json:"source_url,omitempty"`
	SourceKey  string    `json:"source_key,omitempty"`
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
	AttemptID        string            `json:"attempt_id"`
	Outcome          string            `json:"outcome"`
	DecidedAt        time.Time         `json:"decided_at"`
	Deadline         time.Time         `json:"deadline,omitzero"`
	Summary          AcceptedSummary   `json:"summary,omitzero"`
	OutcomeBinding   string            `json:"outcome_binding,omitempty"`
	UnresolvedAccess AccessDisposition `json:"unresolved_access,omitzero"`
	StopRequest      StopRequest       `json:"stop_request,omitzero"`
}

// AcceptedSummary retains the selected identity after Accepted is cleared.
// An empty summary belongs to an older decision and is never user-view proof.
type AcceptedSummary struct {
	AcceptedAt      time.Time              `json:"accepted_at"`
	SourceKey       string                 `json:"source_key"`
	SourceNodeID    string                 `json:"source_node_id"`
	SourceURL       string                 `json:"source_url,omitempty"`
	SourceUpdatedAt time.Time              `json:"source_updated_at,omitzero"`
	CommandName     string                 `json:"command_name"`
	Requester       string                 `json:"requester"`
	RequesterID     int64                  `json:"requester_id"`
	Repository      string                 `json:"repository"`
	RepositoryID    int64                  `json:"repository_id"`
	OwnerID         int64                  `json:"owner_id"`
	ExecutorID      string                 `json:"executor_id"`
	SubjectType     string                 `json:"subject_type"`
	SubjectNumber   int                    `json:"subject_number"`
	Action          string                 `json:"action"`
	MainProvider    string                 `json:"main_provider,omitempty"`
	CriticProvider  string                 `json:"critic_provider,omitempty"`
	MainSettings    config.RoleModelConfig `json:"main_settings,omitzero"`
	CriticSettings  config.RoleModelConfig `json:"critic_settings,omitzero"`
}

// HistorySummary carries the selected identity into a retry or first terminal decision.
func (a AcceptedAction) HistorySummary() AcceptedSummary {
	return AcceptedSummary{AcceptedAt: a.AcceptedAt, SourceKey: a.SourceKey, SourceNodeID: a.SourceNodeID,
		SourceURL: a.SourceURL, SourceUpdatedAt: a.SourceUpdatedAt,
		CommandName: a.CommandName, Requester: a.Requester, RequesterID: a.RequesterID,
		Repository: a.Repository, RepositoryID: a.RepositoryID, OwnerID: a.OwnerID,
		ExecutorID: a.ExecutorID, SubjectType: a.SubjectType, SubjectNumber: a.SubjectNumber,
		Action: a.Action, MainProvider: a.MainProvider, CriticProvider: a.CriticProvider,
		MainSettings: a.MainSettings, CriticSettings: a.CriticSettings}
}

func (s AcceptedSummary) valid() bool {
	if s == (AcceptedSummary{}) { // Older decisions remain readable by the administrator.
		return true
	}
	if s.AcceptedAt.IsZero() || s.SourceKey == "" || s.SourceNodeID == "" ||
		!workspace.ValidIdentitySegment(s.CommandName) ||
		!workspace.ValidIdentitySegment(s.ExecutorID) ||
		!workspace.ValidIdentitySegment(strings.TrimSuffix(s.Requester, "[bot]")) || s.RequesterID <= 0 ||
		!workspace.ValidRepository(s.Repository) || s.RepositoryID <= 0 || s.OwnerID <= 0 ||
		(s.SubjectType != "Issue" && s.SubjectType != "PullRequest") || s.SubjectNumber <= 0 ||
		!s.MainSettings.ValidChoice() ||
		(s.CriticProvider != "" && !s.CriticSettings.ValidChoice()) ||
		(s.MainProvider != "codex" && s.MainProvider != "claude") ||
		(s.CriticProvider != "" && s.CriticProvider != "codex" && s.CriticProvider != "claude") ||
		(s.CriticProvider == "" && s.CriticSettings != (config.RoleModelConfig{})) {
		return false
	}
	switch s.Action {
	case "review", "fix", "comment", "security-audit":
		return true
	default:
		return false
	}
}

// StopRequest records the first stop request and the observed cleanup result.
// A terminal decision may precede a worker's return, so its outcome is not proof
// that termination was confirmed.
type StopRequest struct {
	RequestedBy string    `json:"requested_by"`
	RequestedAt time.Time `json:"requested_at"`
	Status      string    `json:"status"`
}

func (r StopRequest) valid() bool {
	if r == (StopRequest{}) {
		return true
	}
	if !validStopRequester(r.RequestedBy) || r.RequestedAt.IsZero() {
		return false
	}
	switch r.Status {
	case "requested", "confirmed", "unverified":
		return true
	default:
		return false
	}
}

// A GitHub user's numeric ID has a distinct namespace from local admin names.
func validStopRequester(requestedBy string) bool {
	if workspace.ValidIdentitySegment(requestedBy) {
		return true
	}
	const prefix = "github-user:"
	if !strings.HasPrefix(requestedBy, prefix) || len(requestedBy) > len(prefix)+19 {
		return false
	}
	idText := requestedBy[len(prefix):]
	id, err := strconv.ParseInt(idText, 10, 64)
	return err == nil && id > 0 && strconv.FormatInt(id, 10) == idText
}

// AccessDisposition identifies an accepted request that needs operator action.
type AccessDisposition struct {
	Repository     string `json:"repository"`
	SourceURL      string `json:"source_url,omitempty"`
	SourceKey      string `json:"source_key"`
	Reason         string `json:"reason"`
	ReplyState     string `json:"reply_state"`
	OperatorAction string `json:"operator_action"`
}

func validTerminalDecision(jobID string, decision TerminalDecision) bool {
	switch decision.Outcome {
	case "succeeded", "inactivity", "expired", "canceled", "lost", "failed":
	default:
		return false
	}
	if !workspace.ValidIdentitySegment(jobID) || !workspace.ValidIdentitySegment(decision.AttemptID) ||
		decision.DecidedAt.IsZero() || !validOutcomeBinding(decision.OutcomeBinding) || !decision.StopRequest.valid() ||
		!decision.Summary.valid() || decision.Summary != (AcceptedSummary{}) && !decision.Deadline.After(decision.Summary.AcceptedAt) {
		return false
	}
	access := decision.UnresolvedAccess
	if access == (AccessDisposition{}) {
		return true
	}
	return decision.Outcome != "succeeded" && workspace.ValidRepository(access.Repository) &&
		access.SourceKey != "" && access.Reason != "" && access.OperatorAction != "" &&
		(access.ReplyState == "not_issued" || access.ReplyState == "unknown")
}

// AcceptedAction retains the one selected action across process restarts.
type AcceptedAction struct {
	JobID           string                 `json:"job_id"`
	AttemptID       string                 `json:"attempt_id"`
	ExecutorID      string                 `json:"executor_id"`
	Repository      string                 `json:"repository"`
	RepositoryID    int64                  `json:"repository_id"`
	OwnerID         int64                  `json:"owner_id"`
	SubjectType     string                 `json:"subject_type"`
	SubjectNumber   int                    `json:"subject_number"`
	SourceKey       string                 `json:"source_key"`
	SourceNodeID    string                 `json:"source_node_id"`
	SourceURL       string                 `json:"source_url"`
	SourceUpdatedAt time.Time              `json:"source_updated_at"`
	Requester       string                 `json:"requester"`
	RequesterID     int64                  `json:"requester_id"`
	CommandName     string                 `json:"command_name"`
	Action          string                 `json:"action"`
	MainProvider    string                 `json:"main_provider,omitempty"`
	CriticProvider  string                 `json:"critic_provider,omitempty"`
	MainSettings    config.RoleModelConfig `json:"main_settings,omitzero"`
	CriticSettings  config.RoleModelConfig `json:"critic_settings,omitzero"`
	AcceptedAt      time.Time              `json:"accepted_at"`
	Deadline        time.Time              `json:"deadline"`
	RetryAt         time.Time              `json:"retry_at,omitzero"`
	Assigned        bool                   `json:"assigned"`
	OutcomeBinding  string                 `json:"outcome_binding,omitempty"`
	ResultReady     bool                   `json:"result_ready,omitempty"`
	FixedRefusal    string                 `json:"fixed_refusal,omitempty"`
	Terminal        bool                   `json:"terminal,omitempty"`
	StopRequest     StopRequest            `json:"stop_request,omitzero"`
}

// Validate checks the persisted action identity and absolute runtime window.
func (a AcceptedAction) Validate() error {
	if !validOutcomeBinding(a.OutcomeBinding) || a.OutcomeBinding != "" && !a.Assigned {
		return errors.New("accepted action outcome binding is invalid")
	}
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
	if !a.MainSettings.ValidChoice() ||
		a.CriticProvider != "" && !a.CriticSettings.ValidChoice() ||
		a.CriticProvider == "" && a.CriticSettings != (config.RoleModelConfig{}) {
		return errors.New("accepted action model settings are invalid")
	}
	if a.MainProvider != "codex" && a.MainProvider != "claude" ||
		a.CriticProvider != "" && a.CriticProvider != "codex" && a.CriticProvider != "claude" {
		return errors.New("accepted action role providers are invalid")
	}
	if !a.StopRequest.valid() || a.StopRequest != (StopRequest{}) && !a.Terminal {
		return errors.New("accepted action stop request is invalid")
	}
	return nil
}

func validOutcomeBinding(value string) bool {
	if value == "" {
		return true
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
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
	Attempts         int                    `json:"attempts"`
	ModelCallStarted bool                   `json:"model_call_started,omitempty"`
	NotificationKey  string                 `json:"notification_key,omitempty"`
	AcceptedAt       time.Time              `json:"accepted_at,omitzero"`
	Deadline         time.Time              `json:"deadline,omitzero"`
	MainProvider     string                 `json:"main_provider,omitempty"`
	CriticProvider   string                 `json:"critic_provider,omitempty"`
	MainSettings     config.RoleModelConfig `json:"main_settings,omitzero"`
	CriticSettings   config.RoleModelConfig `json:"critic_settings,omitzero"`
	RetryAction      RetryAction            `json:"retry_action,omitzero"`
}

// RetryAction keeps an unused accepted attempt visible until reacceptance or resolution.
type RetryAction struct {
	AttemptID string          `json:"attempt_id"`
	Summary   AcceptedSummary `json:"summary"`
}

// Validate checks retry timing and frozen role settings.
func (f JobFailure) Validate() error {
	if f.AcceptedAt.IsZero() != f.Deadline.IsZero() || !f.Deadline.IsZero() && !f.Deadline.After(f.AcceptedAt) {
		return errors.New("job failure deadline is invalid")
	}
	if f.AcceptedAt.IsZero() {
		if f.MainProvider != "" || f.CriticProvider != "" ||
			f.MainSettings != (config.RoleModelConfig{}) || f.CriticSettings != (config.RoleModelConfig{}) {
			return errors.New("job failure frozen role settings are invalid")
		}
	} else if f.MainProvider != "codex" && f.MainProvider != "claude" || !f.MainSettings.ValidChoice() ||
		f.CriticProvider != "" && f.CriticProvider != "codex" && f.CriticProvider != "claude" ||
		f.CriticProvider != "" && !f.CriticSettings.ValidChoice() ||
		f.CriticProvider == "" && f.CriticSettings != (config.RoleModelConfig{}) {
		return errors.New("job failure frozen role settings are invalid")
	}
	if f.RetryAction != (RetryAction{}) &&
		(!workspace.ValidIdentitySegment(f.RetryAction.AttemptID) ||
			f.RetryAction.Summary == (AcceptedSummary{}) || !f.RetryAction.Summary.valid() ||
			!f.RetryAction.Summary.AcceptedAt.Equal(f.AcceptedAt) ||
			f.RetryAction.Summary.MainProvider != f.MainProvider ||
			f.RetryAction.Summary.CriticProvider != f.CriticProvider ||
			f.RetryAction.Summary.MainSettings != f.MainSettings ||
			f.RetryAction.Summary.CriticSettings != f.CriticSettings) {
		return errors.New("job failure retry action is invalid")
	}
	return nil
}

type Store struct {
	path           string
	lock           *os.File
	mu             sync.Mutex
	privateRecords sync.Mutex
	activeStop     *selectedStopCancel // Guarded by mu.
	savedStop      savedStopRequest    // Guarded by mu; successful persistence proof for repeats.
	syncDir        string              // Test-only extra directory for a post-rename sync failure.
	Data           Data
}

// WithLock serializes in-process access to Data and saves while an action runs beside controls.
func (s *Store) WithLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

// WithPrivateRecords serializes late receipt admission with private retention pruning.
func (s *Store) WithPrivateRecords(fn func() error) error {
	s.privateRecords.Lock()
	defer s.privateRecords.Unlock()
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
	accepted := s.Data.Accepted
	if accepted == nil || accepted.JobID != jobID || accepted.AttemptID != attemptID {
		return TerminalDecision{}, fmt.Errorf("terminal decision for %q has no matching accepted attempt", jobID)
	}
	if accepted.StopRequest != (StopRequest{}) && outcome != "lost" {
		outcome = "canceled"
	}
	decision := TerminalDecision{AttemptID: attemptID, Outcome: outcome, DecidedAt: at.UTC(), Deadline: accepted.Deadline,
		Summary: accepted.HistorySummary(), OutcomeBinding: accepted.OutcomeBinding, StopRequest: accepted.StopRequest}
	if !validTerminalDecision(jobID, decision) {
		return TerminalDecision{}, errors.New("invalid terminal decision")
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

// DecideRetryTerminalLocked resolves a released, unused attempt without recreating Accepted.
// The caller holds WithLock. The original attempt remains the history identity.
func (s *Store) DecideRetryTerminalLocked(jobID, outcome string, at time.Time) (TerminalDecision, error) {
	decision, err := s.retryTerminalDecision(jobID, outcome, at)
	if err != nil {
		return TerminalDecision{}, err
	}
	if s.Data.TerminalDecisions == nil {
		s.Data.TerminalDecisions = make(map[string]TerminalDecision)
	}
	s.Data.TerminalDecisions[jobID] = decision
	if err := s.Save(); err != nil {
		return TerminalDecision{}, err
	}
	return decision, nil
}

// RetryTerminalRequest is one released attempt to resolve before batch retirement.
type RetryTerminalRequest struct {
	JobID, Outcome string
	DecidedAt      time.Time
}

// DecideRetryTerminalsLocked persists all first decisions before callers retire retries.
// The caller holds WithLock; a failed save leaves the decisions in memory for retry.
func (s *Store) DecideRetryTerminalsLocked(requests []RetryTerminalRequest) error {
	if len(requests) == 0 {
		return nil
	}
	decisions := make(map[string]TerminalDecision, len(requests))
	for _, request := range requests {
		if _, duplicate := decisions[request.JobID]; duplicate {
			return errors.New("duplicate retry terminal request")
		}
		decision, err := s.retryTerminalDecision(request.JobID, request.Outcome, request.DecidedAt)
		if err != nil {
			return err
		}
		decisions[request.JobID] = decision
	}
	if s.Data.TerminalDecisions == nil {
		s.Data.TerminalDecisions = make(map[string]TerminalDecision)
	}
	for jobID, decision := range decisions {
		s.Data.TerminalDecisions[jobID] = decision
	}
	return s.Save()
}

func (s *Store) retryTerminalDecision(jobID, outcome string, at time.Time) (TerminalDecision, error) {
	failure, exists := s.Data.JobFailures[jobID]
	if !exists || failure.RetryAction == (RetryAction{}) ||
		s.Data.Accepted != nil && s.Data.Accepted.JobID == jobID {
		return TerminalDecision{}, errors.New("no released retry attempt to resolve")
	}
	if decision, decided := s.Data.TerminalDecisions[jobID]; decided {
		if decision.AttemptID != failure.RetryAction.AttemptID {
			return TerminalDecision{}, errors.New("terminal decision belongs to another attempt")
		}
		return decision, nil
	}
	decision := TerminalDecision{AttemptID: failure.RetryAction.AttemptID, Outcome: outcome,
		DecidedAt: at.UTC(), Deadline: failure.Deadline, Summary: failure.RetryAction.Summary}
	if !validTerminalDecision(jobID, decision) {
		return TerminalDecision{}, errors.New("invalid released retry terminal decision")
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
	store.Data, err = decodeStateData(f)
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, errors.Join(err, store.Close())
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
