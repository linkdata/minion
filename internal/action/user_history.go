package action

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

// Return at most eight candidates per page so the UI can show four accessible
// actions while bounding current-access checks for revoked repositories.
const userHistoryPageSize = 8
const historyTimeLayout = "2006-01-02T15:04:05.000000000Z"

var ErrInvalidHistoryCursor = errors.New("invalid action history cursor")

// UserHistoryEntry is a bounded index entry derived from existing durable state.
type UserHistoryEntry struct {
	JobID, AttemptID string
	Summary          state.AcceptedSummary
	Deadline         time.Time
	Outcome          string
	Progress         string
	Cursor           string
}

type UserHistoryPage struct {
	Entries []UserHistoryEntry
	Next    string
}

func historyCursor(at time.Time, jobID string) string {
	digest := sha256.Sum256([]byte(jobID))
	return at.UTC().Format(historyTimeLayout) + ":" + hex.EncodeToString(digest[:])
}

func parseHistoryCursor(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if len(raw) > 200 {
		return "", ErrInvalidHistoryCursor
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != len(historyTimeLayout)+1+64 {
		return "", ErrInvalidHistoryCursor
	}
	value := string(decoded)
	i := strings.LastIndexByte(value, ':')
	if i < 0 {
		return "", ErrInvalidHistoryCursor
	}
	at, hash := value[:i], value[i+1:]
	parsed, timeErr := time.Parse(historyTimeLayout, at)
	if timeErr != nil || parsed.UTC().Format(historyTimeLayout) != at || len(hash) != 64 {
		return "", ErrInvalidHistoryCursor
	}
	if _, err := hex.DecodeString(hash); err != nil || strings.ToLower(hash) != hash {
		return "", ErrInvalidHistoryCursor
	}
	return value, nil
}

// UserHistory returns only entries bound to one original numeric GitHub requester.
// Its cursor is stable when newer actions arrive and carries no attempt ID.
func (e *Orchestrator) UserHistory(requesterID int64, cursor string) (UserHistoryPage, error) {
	if e == nil || e.Store == nil || requesterID <= 0 {
		return UserHistoryPage{}, errors.New("user history unavailable")
	}
	before, err := parseHistoryCursor(cursor)
	if err != nil {
		return UserHistoryPage{}, err
	}
	data, err := readDashboardState(e.Store.ReadSavedDurable, runtime.GOOS == "windows")
	if err != nil {
		return UserHistoryPage{}, err
	}
	entries := make(map[string]UserHistoryEntry)
	add := func(jobID, attemptID string, summary state.AcceptedSummary, deadline time.Time, outcome string) {
		if summary.RequesterID != requesterID || summary == (state.AcceptedSummary{}) ||
			!workspace.ValidIdentitySegment(jobID) || !workspace.ValidIdentitySegment(attemptID) {
			return
		}
		entries[jobID] = UserHistoryEntry{JobID: jobID, AttemptID: attemptID, Summary: summary,
			Deadline: deadline, Outcome: outcome, Cursor: historyCursor(summary.AcceptedAt, jobID)}
	}
	if accepted := data.Accepted; accepted != nil {
		outcome := "In progress"
		if accepted.Terminal {
			outcome = "Terminal cleanup; decision unavailable"
		}
		add(accepted.JobID, accepted.AttemptID, accepted.HistorySummary(), accepted.Deadline, outcome)
	}
	for jobID, failure := range data.JobFailures {
		if retry := failure.RetryAction; retry != (state.RetryAction{}) {
			add(jobID, retry.AttemptID, retry.Summary, failure.Deadline, "Retry pending")
		}
	}
	for jobID, decision := range data.TerminalDecisions {
		add(jobID, decision.AttemptID, decision.Summary, decision.Deadline, decision.Outcome)
	}
	ordered := make([]UserHistoryEntry, 0, len(entries))
	for _, entry := range entries {
		if before == "" || entry.Cursor < before {
			ordered = append(ordered, entry)
		}
	}
	slices.SortFunc(ordered, func(a, b UserHistoryEntry) int { return strings.Compare(b.Cursor, a.Cursor) })
	page := UserHistoryPage{}
	if len(ordered) > userHistoryPageSize {
		page.Next = base64.RawURLEncoding.EncodeToString([]byte(ordered[userHistoryPageSize-1].Cursor))
		ordered = ordered[:userHistoryPageSize]
	}
	page.Entries = ordered
	for i := range page.Entries {
		if page.Entries[i].Outcome != "In progress" {
			continue
		}
		snapshot, err := e.DashboardSnapshot(time.Now().UTC())
		if err == nil && snapshot.Action != nil && snapshot.Action.JobID == page.Entries[i].JobID &&
			snapshot.Action.AttemptID == page.Entries[i].AttemptID {
			page.Entries[i].Progress = snapshot.Action.Phase
		}
		break
	}
	return page, nil
}

// UserActionIdentity reads only the durable index, before any private attempt file.
func (e *Orchestrator) UserActionIdentity(attemptID string, requesterID int64) (state.AcceptedSummary, bool, error) {
	if e == nil || e.Store == nil || requesterID <= 0 || !workspace.ValidIdentitySegment(attemptID) {
		return state.AcceptedSummary{}, false, errors.New("action identity unavailable")
	}
	data, err := readDashboardState(e.Store.ReadSavedDurable, runtime.GOOS == "windows")
	if err != nil {
		return state.AcceptedSummary{}, false, err
	}
	var summary state.AcceptedSummary
	jobID := ""
	add := func(id, candidateAttempt string, candidate state.AcceptedSummary) error {
		if candidateAttempt != attemptID || candidate == (state.AcceptedSummary{}) {
			return nil
		}
		if jobID != "" && (jobID != id || summary != candidate) {
			return errors.New("ambiguous saved attempt")
		}
		jobID, summary = id, candidate
		return nil
	}
	if accepted := data.Accepted; accepted != nil {
		if err := add(accepted.JobID, accepted.AttemptID, accepted.HistorySummary()); err != nil {
			return state.AcceptedSummary{}, false, err
		}
	}
	for id, failure := range data.JobFailures {
		if err := add(id, failure.RetryAction.AttemptID, failure.RetryAction.Summary); err != nil {
			return state.AcceptedSummary{}, false, err
		}
	}
	for id, decision := range data.TerminalDecisions {
		if err := add(id, decision.AttemptID, decision.Summary); err != nil {
			return state.AcceptedSummary{}, false, err
		}
	}
	if jobID == "" || summary.RequesterID != requesterID {
		return state.AcceptedSummary{}, false, nil
	}
	return summary, true, nil
}
