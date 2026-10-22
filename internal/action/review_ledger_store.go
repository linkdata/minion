package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

// retainedReview contains review evidence only, never a publication outcome.
type retainedReview struct {
	CriticStopReason                 string       `json:"critic_stop_reason,omitempty"`
	Identity                         Identity     `json:"identity"`
	SourceRef                        string       `json:"source_ref"`
	CriticMaxRounds                  int          `json:"critic_max_rounds"`
	CriticNonblockingRounds          int          `json:"critic_nonblocking_rounds"`
	CriticConsecutiveNoBlockerRounds int          `json:"critic_consecutive_no_blocker_rounds"`
	Calls                            []CallUsage  `json:"calls"`
	Refutations                      []Refutation `json:"refutations"`
}

// retainReviewEvidence validates and saves private review history for an accepted Job.
// A failed call may leave a valid prefix; answer/change validation must not erase it.
func (e *Orchestrator) retainReviewEvidence(job Job, result Result) error {
	if job.Critic == nil {
		return nil
	}
	if err := saveReviewEvidence(e.WorkRoot, job, result, e.Logger); err != nil {
		return fmt.Errorf("%w: retain private review evidence: %w", monitor.ErrTerminalJob, err)
	}
	return nil
}

func privateRecordsDir(workRoot, name string) (string, error) {
	if workRoot == "" {
		return "", errors.New("private records work root is required")
	}
	workRoot, err := workspace.CanonicalPath(workRoot)
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

// saveReviewEvidence accepts a validated job on an admitted host work root.
func saveReviewEvidence(workRoot string, job Job, result Result, logger *log.Logger) error {
	if !result.matchesJob(job) {
		return errors.New("review evidence does not match action identity or source")
	}
	if err := result.validateCalls(job, false); err != nil {
		return err
	}
	if err := validateRefutations(job, result.Calls, result.Refutations); err != nil {
		return err
	}
	record := retainedReview{
		CriticStopReason: result.CriticStopReason,
		Identity:         job.Identity, SourceRef: job.Source.Head,
		CriticMaxRounds: job.CriticMaxRounds, CriticNonblockingRounds: job.CriticNonblockingRounds,
		CriticConsecutiveNoBlockerRounds: job.CriticConsecutiveNoBlockerRounds,
		Calls:                            result.Calls, Refutations: result.Refutations,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
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
		if (saved.CriticStopReason != "" && saved.CriticStopReason != record.CriticStopReason) ||
			saved.Identity != record.Identity || saved.SourceRef != record.SourceRef ||
			saved.CriticMaxRounds != record.CriticMaxRounds || saved.CriticNonblockingRounds != record.CriticNonblockingRounds ||
			saved.CriticConsecutiveNoBlockerRounds != record.CriticConsecutiveNoBlockerRounds ||
			len(saved.Calls) > len(record.Calls) || len(saved.Refutations) > len(record.Refutations) ||
			!slices.Equal(saved.Calls, record.Calls[:len(saved.Calls)]) || !slices.Equal(saved.Refutations, record.Refutations[:len(saved.Refutations)]) {
			return errors.New("review evidence changed or discarded a saved prefix")
		}
		if saved.CriticStopReason == record.CriticStopReason && len(saved.Calls) == len(record.Calls) && len(saved.Refutations) == len(record.Refutations) {
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
