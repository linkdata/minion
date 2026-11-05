package action

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

const (
	detailReadLimit  = workspace.MaxBytes
	detailTextLimit  = 64 << 10
	detailEntryLimit = 64
	detailLineLimit  = 8192
	detailFieldLimit = 4 << 10
)

type DashboardDetailEntry struct {
	Kind, Meta, Text string
	Truncated        bool
}

// DashboardDetail projects only existing saved state and private attempt records.
type DashboardDetail struct {
	JobID, AttemptID, Repository, Requester, Action    string
	Summary                                            state.AcceptedSummary
	Outcome, Publication, PushPublication              string
	Progress                                           string
	Deadline                                           time.Time
	Remaining                                          time.Duration
	Records                                            []DashboardDetailEntry
	RecordsNotice                                      string
	CriticVerdict, CriticStop, ReviewNotice            string
	Refutations                                        []DashboardDetailEntry
	Diagnostics                                        []DashboardDetailEntry
	WorkerFailure, WorkerObservedFailure, WorkerNotice string
}

// DashboardActionDetail resolves the selected attempt or a retained terminal decision.
// The HTTP caller enforces administrator authorization before calling it.
func (e *Orchestrator) DashboardActionDetail(attemptID string, now time.Time) (DashboardDetail, bool, error) {
	return e.dashboardActionDetail(attemptID, now, nil)
}

// UserActionDetail binds the private read to the identity authorized by its HTTP caller.
func (e *Orchestrator) UserActionDetail(attemptID string, now time.Time, authorized state.AcceptedSummary) (DashboardDetail, bool, error) {
	if authorized == (state.AcceptedSummary{}) {
		return DashboardDetail{}, false, errors.New("missing authorized action identity")
	}
	return e.dashboardActionDetail(attemptID, now, &authorized)
}

func (e *Orchestrator) dashboardActionDetail(attemptID string, now time.Time, authorized *state.AcceptedSummary) (DashboardDetail, bool, error) {
	if e == nil || e.Store == nil {
		return DashboardDetail{}, false, errors.New("dashboard state is unavailable")
	}
	if !workspace.ValidIdentitySegment(attemptID) {
		return DashboardDetail{}, false, errors.New("invalid attempt ID")
	}
	data, err := readDashboardState(e.Store.ReadSavedDurable, runtime.GOOS == "windows")
	if err != nil {
		return DashboardDetail{}, false, err
	}
	var accepted *state.AcceptedAction
	if data.Accepted != nil && data.Accepted.AttemptID == attemptID {
		accepted = data.Accepted
	}
	jobID := ""
	if accepted != nil {
		jobID = accepted.JobID
	}
	var decision state.TerminalDecision
	for id, saved := range data.TerminalDecisions {
		if saved.AttemptID != attemptID {
			continue
		}
		if jobID != "" && jobID != id {
			return DashboardDetail{}, false, errors.New("ambiguous saved attempt")
		}
		jobID, decision = id, saved
	}
	var retry state.RetryAction
	var retryDeadline time.Time
	for id, failure := range data.JobFailures {
		if failure.RetryAction.AttemptID != attemptID {
			continue
		}
		if jobID != "" && jobID != id {
			return DashboardDetail{}, false, errors.New("ambiguous saved attempt")
		}
		jobID, retry, retryDeadline = id, failure.RetryAction, failure.Deadline
	}
	if jobID == "" {
		return DashboardDetail{}, false, nil
	}
	detail := DashboardDetail{JobID: jobID, AttemptID: attemptID, Outcome: "In progress",
		Publication: "No confirmed publication checkpoint"}
	if accepted != nil {
		detail.Repository, detail.Requester, detail.Action = accepted.Repository, accepted.Requester, accepted.Action
		detail.Summary = accepted.HistorySummary()
		detail.Deadline = accepted.Deadline
		if !accepted.Terminal {
			detail.Remaining = max(0, accepted.Deadline.Sub(now))
		} else {
			detail.Outcome = "Terminal cleanup; decision unavailable"
		}
	}
	if decision.AttemptID != "" {
		detail.Outcome = decision.Outcome
		if accepted == nil {
			detail.Deadline = decision.Deadline
			if decision.Summary != (state.AcceptedSummary{}) {
				detail.Repository, detail.Requester, detail.Action = decision.Summary.Repository, decision.Summary.Requester, decision.Summary.Action
				detail.Summary = decision.Summary
			}
		}
	}
	if retry != (state.RetryAction{}) && decision.AttemptID == "" && accepted == nil {
		detail.Outcome, detail.Deadline, detail.Summary = "Retry pending", retryDeadline, retry.Summary
		detail.Remaining = max(0, retryDeadline.Sub(now))
		detail.Repository, detail.Requester, detail.Action = retry.Summary.Repository, retry.Summary.Requester, retry.Summary.Action
	}
	if authorized != nil && detail.Summary != *authorized {
		return DashboardDetail{}, false, errors.New("authorized action identity changed")
	}
	if accepted != nil && !accepted.Terminal && decision.AttemptID == "" {
		if snapshot, err := e.DashboardSnapshot(now); err == nil && snapshot.Action != nil &&
			snapshot.Action.JobID == jobID && snapshot.Action.AttemptID == attemptID {
			detail.Progress = snapshot.Action.Phase
		}
	}
	if write := data.PublicationWrites[jobID]; write.AttemptID == attemptID {
		detail.Publication = detailPublicationStatus(write.Status)
	}
	if write := data.PublicationWrites[jobID+"-push"]; write.AttemptID == attemptID {
		detail.PushPublication = detailPublicationStatus(write.Status)
	}
	workRoot, err := workspace.CanonicalPath(e.WorkRoot)
	if err != nil {
		workRoot = ""
	}
	sourceRef := ""
	var summary *state.AcceptedSummary
	if detail.Summary != (state.AcceptedSummary{}) {
		summary = &detail.Summary
	}
	detail.readRecords(workRoot, summary, &sourceRef)
	detail.readReview(workRoot, summary, &sourceRef)
	detail.readWorker(workRoot, summary, &sourceRef)
	return detail, true, nil
}

func detailPublicationStatus(status string) string {
	switch status {
	case state.PublicationWriteObserved:
		return "Observed on GitHub"
	case state.PublicationWritePending:
		return "Pending confirmation"
	case state.PublicationWriteUncertain:
		return "Uncertain; publication not confirmed"
	default:
		return "No confirmed publication checkpoint"
	}
}

func readDetailFile(workRoot, directory, name string) ([]byte, string) {
	if workRoot == "" {
		return nil, "Private record root unavailable"
	}
	path := filepath.Join(workRoot, directory, name)
	data, err := workspace.ReadRegular(path, detailReadLimit)
	if err == nil {
		return data, ""
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, "No saved record available"
	}
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() && info.Size() > detailReadLimit {
		return nil, fmt.Sprintf("Saved record exceeds the %d MiB read limit", detailReadLimit>>20)
	}
	return nil, "Saved record unavailable or invalid"
}

func (d *DashboardDetail) matches(id Identity, source string, summary *state.AcceptedSummary, sourceRef *string) bool {
	if id.validate() != nil || id.JobID != d.JobID || id.AttemptID != d.AttemptID || !workspace.ValidSHA(source) ||
		summary != nil && (id.ExecutorID != summary.ExecutorID || id.Repository != summary.Repository ||
			id.RepositoryID != summary.RepositoryID || id.OwnerID != summary.OwnerID ||
			id.RequesterID != summary.RequesterID || !strings.EqualFold(id.Requester, summary.Requester) ||
			id.Number != summary.SubjectNumber || id.SubjectType != summary.SubjectType) ||
		*sourceRef != "" && *sourceRef != source {
		return false
	}
	*sourceRef = source
	if d.Repository == "" {
		d.Repository, d.Requester = id.Repository, id.Requester
	}
	return true
}

func detailText(value string, remaining *int) (string, bool) {
	limit := min(detailFieldLimit, *remaining)
	if len(value) <= limit {
		*remaining -= len(value)
		return value, false
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	*remaining -= limit
	return value[:limit], true
}

func detailEntry(entries *[]DashboardDetailEntry, kind, meta, value string, remaining *int) bool {
	if len(*entries) >= detailEntryLimit || *remaining == 0 {
		return false
	}
	text, clipped := detailText(value, remaining)
	*entries = append(*entries, DashboardDetailEntry{Kind: kind, Meta: meta, Text: text, Truncated: clipped})
	return true
}

func (d *DashboardDetail) readRecords(workRoot string, summary *state.AcceptedSummary, sourceRef *string) {
	data, notice := readDetailFile(workRoot, "attempt-records", d.AttemptID+".jsonl")
	if notice != "" {
		d.RecordsNotice = notice
		return
	}
	remaining, count, complete, stopped, limited, invalid, incomplete, captureLimited := detailTextLimit, 0, false, false, false, false, false, false
	for len(data) > 0 {
		if complete {
			invalid = true
			break
		}
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			incomplete = true
			break
		}
		if count >= detailLineLimit {
			limited = true
			break
		}
		line := data[:end]
		data = data[end+1:]
		count++
		var record attemptRecord
		if len(line) > maxAttemptRecordLineBytes || httpDecodeBytes(line, maxAttemptRecordLineBytes, &record) != nil ||
			record.Sequence != uint64(count) || record.validateShape() != nil ||
			!d.matches(record.Identity, record.SourceRef, summary, sourceRef) {
			invalid = true
			break
		}
		if stopped && record.Kind != attemptRecordCoverage {
			invalid = true
			break
		}
		if record.Kind == attemptRecordCoverage {
			complete = true
			continue
		}
		if record.Kind == attemptRecordStop && record.Text == "capture_limit" {
			captureLimited = true
		}
		if record.Kind == attemptRecordStop {
			stopped = true
		}
		if record.Kind == attemptRecordReviewReference || record.Kind == attemptRecordWorkerReference {
			continue
		}
		value := record.Text
		if value == "" {
			switch record.EventSource {
			case record.Provider + ".content_missing":
				value = "Content missing from received event"
			case record.Provider + ".diagnostics_truncated":
				value = "Provider diagnostics truncated before receipt"
			case record.Provider + ".call.diagnostic_unavailable":
				value = "Provider call diagnostic unavailable"
			case record.Provider + ".stderr_unavailable":
				value = "Provider stderr unavailable"
			default:
				continue
			}
		}
		meta := strings.Join([]string{record.Role, record.Provider, record.Model, record.Agent}, " ")
		if record.Round > 0 {
			meta += fmt.Sprintf(" round %d", record.Round)
		}
		if !detailEntry(&d.Records, record.Kind, strings.TrimSpace(meta), value, &remaining) {
			limited = true
		} else if d.Records[len(d.Records)-1].Truncated {
			limited = true
		}
	}
	switch {
	case incomplete:
		d.RecordsNotice = "Saved record ends with an incomplete line; only an earlier received prefix is shown"
	case invalid:
		d.RecordsNotice = "Saved record is invalid; only an earlier received prefix is shown"
	case captureLimited && limited:
		d.RecordsNotice = "Capture limit reached; received record is truncated; display is also truncated"
	case captureLimited:
		d.RecordsNotice = "Capture limit reached; received record is truncated"
	case limited && complete:
		d.RecordsNotice = "Received record coverage marker is present; display is truncated"
	case limited:
		d.RecordsNotice = "Received record display is truncated; coverage is unconfirmed"
	case !complete:
		d.RecordsNotice = "Received record coverage is unconfirmed; this may be a partial transcript"
	default:
		d.RecordsNotice = "Received record coverage marker is present"
	}
}

func (d *DashboardDetail) readReview(workRoot string, summary *state.AcceptedSummary, sourceRef *string) {
	data, notice := readDetailFile(workRoot, "reviews", d.AttemptID+".json")
	if notice != "" {
		d.ReviewNotice = notice
		return
	}
	var review retainedReview
	if httpDecodeBytes(data, detailReadLimit, &review) != nil ||
		httpReviewShape(ReviewCheckpoint{Version: ProtocolVersion, Identity: review.Identity,
			SourceRef: review.SourceRef, Calls: review.Calls, Refutations: review.Refutations,
			CriticStopReason: review.CriticStopReason}) != nil ||
		(review.ReviewClaimOrigin != "" && review.ReviewClaimOrigin != executorClaimOrigin) ||
		(len(review.Calls) > 0 && review.ReviewClaimOrigin != executorClaimOrigin) ||
		!d.matches(review.Identity, review.SourceRef, summary, sourceRef) {
		d.ReviewNotice = "Saved critic evidence unavailable or invalid"
		return
	}
	d.CriticVerdict = "No completed full critic verdict saved"
	lastFull := -1
	for i, call := range review.Calls {
		if call.Role == roleCritic && call.ReviewPhase == reviewFull && call.ReviewComplete {
			lastFull = i
			d.CriticVerdict = fmt.Sprintf("Round %d full review: %s", call.Round, detailCriticVerdict(call))
		}
	}
	if lastFull < 0 {
		for _, call := range review.Calls {
			if call.Role == roleCritic && call.ReviewPhase == reviewFull {
				d.CriticVerdict = "Last full critic review incomplete; no completed verdict saved"
			}
		}
	} else if lastFull < len(review.Calls)-1 {
		laterFullIncomplete := false
		var verification *CallUsage
		for i := lastFull + 1; i < len(review.Calls); i++ {
			call := &review.Calls[i]
			if call.Role == roleCritic && call.ReviewPhase == reviewVerification {
				verification = call
			}
			if call.Role == roleCritic && call.ReviewPhase == reviewFull && !call.ReviewComplete {
				laterFullIncomplete = true
			}
		}
		if verification != nil {
			d.CriticVerdict += fmt.Sprintf("; round %d verification: %s", verification.Round, detailCriticVerdict(*verification))
		}
		if laterFullIncomplete {
			d.CriticVerdict += "; later full review incomplete"
		} else {
			d.CriticVerdict += "; later work has no completed full review"
		}
	}
	switch review.CriticStopReason {
	case "approved":
		d.CriticStop = "Stopped after approval"
	case "nonblocking":
		d.CriticStop = "Stopped after configured no-blocker rounds; approval not implied"
	case "round_cap":
		cap := cappedReviewCalls(review.CriticMaxRounds, review.Calls)
		var err error
		d.CriticStop, err = criticCapNotice(cap)
		if err != nil {
			d.ReviewNotice = "Saved critic cap evidence unavailable or invalid"
			return
		}
	case "":
		d.CriticStop = "No final stopping decision saved"
	default:
		d.ReviewNotice = "Saved critic stopping reason is invalid"
		return
	}
	remaining, limited := detailTextLimit, false
	for _, refutation := range review.Refutations {
		if !detailEntry(&d.Refutations, "Main-model refutation", fmt.Sprintf("round %d", refutation.Round), refutation.Text, &remaining) {
			limited = true
		} else if d.Refutations[len(d.Refutations)-1].Truncated {
			limited = true
		}
	}
	if len(review.Calls) == 0 {
		d.ReviewNotice = "No critic calls saved yet"
	} else {
		d.ReviewNotice = "Saved critic calls are executor-reported and validated at receipt"
	}
	if limited {
		d.ReviewNotice += "; main-model refutations are truncated in this view"
	}
}

func detailCriticVerdict(call CallUsage) string {
	if !call.ReviewComplete {
		return "Critic review incomplete"
	}
	switch {
	case call.Approved && call.NoBlockers && !call.HasBlockers:
		return "Critic approved; no blockers reported"
	case call.NoBlockers && !call.Approved && !call.HasBlockers:
		return "Critic reported no blockers without approval"
	case call.HasBlockers && !call.Approved && !call.NoBlockers:
		return "Critic reported blockers"
	case !call.Approved && !call.NoBlockers && !call.HasBlockers:
		return "Completed review has no parsed verdict"
	default:
		return "Critic verdict fields are inconsistent"
	}
}

func (d *DashboardDetail) readWorker(workRoot string, summary *state.AcceptedSummary, sourceRef *string) {
	data, notice := readDetailFile(workRoot, "worker-diagnostics", d.AttemptID+".json")
	if notice != "" {
		d.WorkerNotice = notice
		return
	}
	var worker retainedWorkerDiagnostics
	if httpDecodeBytes(data, detailReadLimit, &worker) != nil ||
		worker.WorkerReportedFailure.validate() != nil || validDiagnostics(worker.WorkerReportedDiagnostics) != nil ||
		!d.matches(worker.Identity, worker.SourceRef, summary, sourceRef) {
		d.WorkerNotice = "Saved worker diagnostics unavailable or invalid"
		return
	}
	remaining, limited := detailTextLimit, false
	if worker.WorkerReportedFailure != nil {
		d.WorkerFailure, limited = detailText(worker.WorkerReportedFailure.Message, &remaining)
		if worker.WorkerReportedFailure.ObservedMessage != "" {
			var clipped bool
			d.WorkerObservedFailure, clipped = detailText(worker.WorkerReportedFailure.ObservedMessage, &remaining)
			limited = limited || clipped
		}
	}
	for _, diagnostic := range worker.WorkerReportedDiagnostics {
		if !detailEntry(&d.Diagnostics, diagnostic.Code, "worker-reported", diagnostic.Message, &remaining) {
			limited = true
		} else if d.Diagnostics[len(d.Diagnostics)-1].Truncated {
			limited = true
		}
	}
	d.WorkerNotice = "Worker diagnostics are executor-reported"
	if limited {
		d.WorkerNotice += "; display is truncated"
	}
	if worker.CleanupUnverifiedWorkRoot != "" {
		d.WorkerNotice += "; worker cleanup is unverified"
	}
}
