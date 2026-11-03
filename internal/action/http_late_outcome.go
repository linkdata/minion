package action

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/linkdata/minion/internal/workspace"
)

var errLateOutcomeRejected = errors.New("late HTTP outcome is not bound to a retained terminal attempt")

func (e *HTTPExecutor) serveLateOutcome(w http.ResponseWriter, outcome httpOutcome) {
	binding := httpBinding{outcome.Result.Version, outcome.Result.Identity, outcome.Result.SourceRef}
	err := e.lateOutcome(outcome)
	if err != nil {
		status := http.StatusInternalServerError
		message := errors.New("late HTTP outcome retention failed")
		if errors.Is(err, errLateOutcomeRejected) || errors.Is(err, errPrivateRecordConflict) {
			status = http.StatusConflict
			message = errLateOutcomeRejected
		}
		httpServerReject(w, "outcome", status, binding, outcome.Sequence, message)
		return
	}
	_ = httpServerWrite(w, http.StatusOK, httpAcknowledgement{httpBinding: binding, Sequence: outcome.Sequence, Kind: "outcome"})
}

// receiveLateOutcome retains private facts only after the first terminal decision.
func (e *Orchestrator) receiveLateOutcome(executorID string, outcome httpOutcome) (resultErr error) {
	result := outcome.Result
	defer func() {
		if resultErr != nil && !errors.Is(resultErr, errLateOutcomeRejected) {
			e.logf("late remote outcome retention failed job=%s attempt=%s: %v", result.Identity.JobID, result.Identity.AttemptID, resultErr)
		}
	}()
	binding := httpBinding{result.Version, result.Identity, result.SourceRef}
	if e.Store == nil || result.Version != ProtocolVersion || outcome.Sequence == 0 ||
		result.Identity.ExecutorID != executorID || result.Identity.validate() != nil ||
		validDiagnostics(result.Diagnostics) != nil {
		return errLateOutcomeRejected
	}
	saved, err := e.Store.ReadSaved()
	if err != nil {
		return fmt.Errorf("read saved terminal decision: %w", err)
	}
	decision, admitted := saved.TerminalDecisions[result.Identity.JobID]
	digest := httpBindingDigest(binding)
	if !admitted {
		if accepted := saved.Accepted; accepted != nil && accepted.JobID == result.Identity.JobID &&
			accepted.AttemptID == result.Identity.AttemptID && accepted.OutcomeBinding == digest {
			return errors.New("matching terminal decision is not yet durable")
		}
		return errLateOutcomeRejected
	}
	if decision.AttemptID != result.Identity.AttemptID || decision.OutcomeBinding == "" ||
		decision.OutcomeBinding != digest || !time.Now().Before(decision.DecidedAt.Add(privateRecordRetention)) {
		return errLateOutcomeRejected
	}
	decidedAt := decision.DecidedAt
	return e.Store.WithPrivateRecords(func() error {
		if !time.Now().Before(decidedAt.Add(privateRecordRetention)) {
			return errLateOutcomeRejected
		}
		if len(result.Calls) != 0 && e.Usage != nil {
			record, found, err := e.Usage.Load(result.Identity.AttemptID)
			if err != nil {
				return fmt.Errorf("load late action usage: %w", err)
			}
			trailingPending := len(record.Calls) == len(result.Calls)+1 && record.Calls[len(record.Calls)-1].FinishedAt == nil
			if !found || record.JobID != result.Identity.JobID || record.RepositoryID != result.Identity.RepositoryID ||
				record.RequesterID != result.Identity.RequesterID || record.Number != result.Identity.Number ||
				record.Repo != result.Identity.Repository || record.ExecutorID != result.Identity.ExecutorID ||
				record.SourceRef != result.SourceRef || len(record.Calls) != len(result.Calls) && !trailingPending {
				return errLateOutcomeRejected
			}
			for i, call := range result.Calls {
				entry := usageCall(i+1, call)
				finished := time.Now().UTC()
				entry.FinishedAt = &finished
				_, err := record.FinishCall(entry)
				if err != nil {
					return fmt.Errorf("%w: %v", errLateOutcomeRejected, err)
				}
			}
			var saveErr error
			if e.lateUsageSave != nil {
				saveErr = e.lateUsageSave(record)
			} else {
				saveErr = e.Usage.Save(record)
			}
			if saveErr != nil {
				return fmt.Errorf("save late action usage: %w", saveErr)
			}
		}
		job := Job{Version: ProtocolVersion, Identity: result.Identity, Source: workspace.Snapshot{Head: result.SourceRef}}
		failure := outcome.Failure.workerErr()
		err := e.retainWorkerDiagnosticsLocked(job, result, failure)
		if errors.Is(err, errPrivateRecordConflict) {
			// The live parent may have added this fixed marker before saving diagnostics.
			capped := result
			capped.Diagnostics = slices.Clone(result.Diagnostics)
			capped = withCaptureLimitDiagnostic(capped)
			if !slices.Equal(capped.Diagnostics, result.Diagnostics) {
				return e.retainWorkerDiagnosticsLocked(job, capped, failure)
			}
		}
		return err
	})
}
