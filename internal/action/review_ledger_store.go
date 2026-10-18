package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

// retainedReview contains review evidence only, never a publication outcome.
type retainedReview struct {
	Version                 int          `json:"version"`
	Identity                Identity     `json:"identity"`
	SourceRef               string       `json:"source_ref"`
	RecordedAt              time.Time    `json:"recorded_at"`
	CriticMaxRounds         int          `json:"critic_max_rounds"`
	CriticNonblockingRounds int          `json:"critic_nonblocking_rounds"`
	Calls                   []CallUsage  `json:"calls"`
	Refutations             []Refutation `json:"refutations"`
}

// retainReviewEvidence saves private history after Runner validates the job and admits its root.
// A failed call may leave a valid prefix; answer/change validation must not erase it.
func (e *Orchestrator) retainReviewEvidence(job Job, result Result) (resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("%w: retain private review evidence: %w", monitor.ErrTerminalJob, resultErr)
		}
	}()
	if job.Critic == nil && len(result.Refutations) == 0 {
		return nil
	}
	return saveReviewEvidence(e.WorkRoot, job, result, e.Logger)
}

func privateRecordsDir(workRoot, name string) (string, error) {
	if workRoot == "" {
		return "", errors.New("private records work root is required")
	}
	workRoot, err := workspace.PrepareRoot(workRoot)
	if err != nil {
		return "", err
	}
	root := filepath.Join(workRoot, name)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if err := workspace.CheckPrivateRoot(root); err != nil {
		return "", err
	}
	if err := workspace.CheckRoot(root); err != nil {
		return "", err
	}
	return root, nil
}

func validateReviewEvidence(job Job, result Result) error {
	if !result.matchesJob(job) {
		return errors.New("review evidence does not match action identity or source")
	}
	if err := result.validateCalls(job, false); err != nil {
		return err
	}
	return validateRefutations(job, result.Calls, result.Refutations)
}

// saveReviewEvidence accepts a validated job on an admitted host work root.
func saveReviewEvidence(workRoot string, job Job, result Result, logger *log.Logger) error {
	if err := validateReviewEvidence(job, result); err != nil {
		return err
	}
	record := retainedReview{
		Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, RecordedAt: time.Now().UTC(),
		CriticMaxRounds: job.CriticMaxRounds, CriticNonblockingRounds: job.CriticNonblockingRounds,
		Calls: result.Calls, Refutations: result.Refutations,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) > workspace.MaxBytes {
		return errors.New("review evidence exceeds transfer byte limit")
	}
	root, err := privateRecordsDir(workRoot, "reviews")
	if err != nil {
		return err
	}
	path := filepath.Join(root, job.Identity.AttemptID+".json")
	previous, err := workspace.ReadRegular(path, workspace.MaxBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		var saved retainedReview
		if err := json.Unmarshal(previous, &saved); err != nil {
			return errors.New("saved review evidence is invalid")
		}
		if saved.Version != record.Version || saved.Identity != record.Identity || saved.SourceRef != record.SourceRef ||
			saved.CriticMaxRounds != record.CriticMaxRounds || saved.CriticNonblockingRounds != record.CriticNonblockingRounds ||
			len(saved.Calls) > len(record.Calls) || len(saved.Refutations) > len(record.Refutations) ||
			!slices.Equal(saved.Calls, record.Calls[:len(saved.Calls)]) || !slices.Equal(saved.Refutations, record.Refutations[:len(saved.Refutations)]) {
			return errors.New("review evidence changed or discarded a saved prefix")
		}
		if len(saved.Calls) == len(record.Calls) && len(saved.Refutations) == len(record.Refutations) {
			return nil
		}
	}
	if err := workspace.WriteAtomic(path, data, filepath.Dir(root)); err != nil {
		return err
	}
	if logger != nil {
		logger.Printf("review evidence job=%s attempt=%s executor=%s refutations=%d path=%q", job.Identity.JobID, job.Identity.AttemptID, job.Identity.ExecutorID, len(result.Refutations), path)
	}
	return nil
}
