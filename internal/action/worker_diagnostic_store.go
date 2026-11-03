package action

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

var errLocalWorkerCleanupUnverified = errors.New("direct local worker attempt cleanup unverified")

type retainedWorkerDiagnostics struct {
	Identity                  Identity     `json:"identity"`
	SourceRef                 string       `json:"source_ref"`
	WorkerReportedFailure     *httpFailure `json:"worker_reported_failure,omitempty"`
	WorkerReportedDiagnostics []Diagnostic `json:"worker_reported_diagnostics,omitempty"`
	CleanupUnverifiedWorkRoot string       `json:"cleanup_unverified_work_root,omitempty"`
}

// retainWorkerDiagnostics saves bounded worker text before its parent attempt can publish.
func (e *Orchestrator) retainWorkerDiagnostics(job Job, result Result, runErr error) error {
	if e.Store == nil {
		return e.retainWorkerDiagnosticsLocked(job, result, runErr)
	}
	return e.Store.WithPrivateRecords(func() error {
		return e.retainWorkerDiagnosticsLocked(job, result, runErr)
	})
}

// retainWorkerDiagnosticsLocked runs while the private-record lock is held.
func (e *Orchestrator) retainWorkerDiagnosticsLocked(job Job, result Result, runErr error) error {
	return e.retainWorkerDiagnosticsWithCaptureUpgradeLocked(job, result, runErr, false)
}

// Only the late receiver may augment an exact parent-only capture marker.
func (e *Orchestrator) retainWorkerDiagnosticsWithCaptureUpgradeLocked(job Job, result Result, runErr error, allowCaptureUpgrade bool) error {
	var failure *workerFailureError
	if errors.As(runErr, &failure) && failure != nil {
		if err := failure.failure.validate(); err != nil {
			return fmt.Errorf("%w: invalid worker failure: %w", monitor.ErrTerminalJob, err)
		}
	}
	var diagnostics []Diagnostic
	if result.matchesJob(job) && validDiagnostics(result.Diagnostics) == nil {
		diagnostics = result.Diagnostics
	}
	cleanupUnverified := errors.Is(runErr, errLocalWorkerCleanupUnverified)
	if failure == nil && len(diagnostics) == 0 && !cleanupUnverified {
		return nil
	}
	if err := job.Identity.validate(); err != nil {
		return fmt.Errorf("%w: private worker diagnostic identity: %w", monitor.ErrTerminalJob, err)
	}
	root, err := privateRecordsDir(e.WorkRoot, "worker-diagnostics")
	if err != nil {
		return retainedRecordFailure("prepare private worker diagnostics", err)
	}
	record := retainedWorkerDiagnostics{Identity: job.Identity, SourceRef: job.Source.Head, WorkerReportedDiagnostics: diagnostics}
	if failure != nil {
		record.WorkerReportedFailure = &failure.failure
	}
	if cleanupUnverified {
		record.CleanupUnverifiedWorkRoot = filepath.Dir(root)
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("%w: encode private worker diagnostics: %w", monitor.ErrTerminalJob, err)
	}
	path := filepath.Join(root, job.Identity.AttemptID+".json")
	previous, err := workspace.ReadRegular(path, workspace.MaxBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		if !retryableHostIO(err) {
			err = fmt.Errorf("%w: %w", errPrivateRecordConflict, err)
		}
		return retainedRecordFailure("read private worker diagnostics", err)
	}
	save := workspace.WriteAtomic
	if e.lateDiagnosticSave != nil {
		save = e.lateDiagnosticSave
	}
	if err == nil {
		if !bytes.Equal(previous, data) {
			conflict := fmt.Errorf("%w: %w: private worker diagnostics changed within an attempt", monitor.ErrTerminalJob, errPrivateRecordConflict)
			if !allowCaptureUpgrade || cleanupUnverified {
				return conflict
			}
			marker := Diagnostic{Code: "capture_limit", Message: captureLimitMessage}
			markers := 0
			for _, diagnostic := range diagnostics {
				if diagnostic.Code == marker.Code {
					if diagnostic != marker {
						return conflict
					}
					markers++
				}
			}
			if markers != 1 {
				return conflict
			}
			parentOnly, err := json.Marshal(retainedWorkerDiagnostics{
				Identity: job.Identity, SourceRef: job.Source.Head,
				WorkerReportedDiagnostics: []Diagnostic{marker},
			})
			if err != nil {
				return fmt.Errorf("%w: encode parent capture marker: %w", monitor.ErrTerminalJob, err)
			}
			if !bytes.Equal(previous, parentOnly) {
				return conflict
			}
			if err := save(path, data, filepath.Dir(root)); err != nil {
				return retainedRecordFailure("upgrade private worker diagnostics", err)
			}
		} else if err := syncPrivateRecordDirectories(root, filepath.Dir(root)); err != nil {
			return retainedRecordFailure("sync private worker diagnostics", err)
		}
	} else if err := save(path, data, filepath.Dir(root)); err != nil {
		return retainedRecordFailure("save private worker diagnostics", err)
	}
	id := job.Identity
	e.logf("action worker diagnostics job=%s attempt=%s executor=%s count=%d worker_reported_failure=%t private_path=%q cleanup_unverified=%t", id.JobID, id.AttemptID, id.ExecutorID, len(diagnostics), failure != nil, path, cleanupUnverified)
	return nil
}
