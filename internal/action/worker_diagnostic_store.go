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

type retainedWorkerDiagnostics struct {
	Identity                  Identity     `json:"identity"`
	SourceRef                 string       `json:"source_ref"`
	WorkerReportedFailure     *httpFailure `json:"worker_reported_failure,omitempty"`
	WorkerReportedDiagnostics []Diagnostic `json:"worker_reported_diagnostics,omitempty"`
	CleanupUnverifiedWorkRoot string       `json:"cleanup_unverified_work_root,omitempty"`
}

// retainWorkerDiagnostics saves bounded worker text before its parent attempt can publish.
func (e *Orchestrator) retainWorkerDiagnostics(job Job, result Result, runErr error) error {
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
		return fmt.Errorf("%w: prepare private worker diagnostics: %w", monitor.ErrTerminalJob, err)
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
		return fmt.Errorf("%w: read private worker diagnostics: %w", monitor.ErrTerminalJob, err)
	}
	if err == nil {
		if !bytes.Equal(previous, data) {
			return fmt.Errorf("%w: private worker diagnostics changed within an attempt", monitor.ErrTerminalJob)
		}
	} else if err := workspace.WriteAtomic(path, data, filepath.Dir(root)); err != nil {
		return fmt.Errorf("%w: save private worker diagnostics: %w", monitor.ErrTerminalJob, err)
	}
	id := job.Identity
	e.logf("action worker diagnostics job=%s attempt=%s executor=%s count=%d worker_reported_failure=%t cleanup_unverified=%t private_path=%q", id.JobID, id.AttemptID, id.ExecutorID, len(diagnostics), failure != nil, cleanupUnverified, path)
	return nil
}
