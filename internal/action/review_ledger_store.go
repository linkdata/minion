package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

// retainedReview contains review evidence only, never a publication outcome.
type retainedReview struct {
	Version                 int           `json:"version"`
	Identity                Identity      `json:"identity"`
	SourceRef               string        `json:"source_ref"`
	RecordedAt              time.Time     `json:"recorded_at"`
	CriticMaxRounds         int           `json:"critic_max_rounds"`
	CriticNonblockingRounds int           `json:"critic_nonblocking_rounds"`
	Calls                   []CallUsage   `json:"calls"`
	ReviewLedger            []ReviewEntry `json:"review_ledger"`
}

// retainReviewLedger validates private history independently of public output.
// A failed call may leave a valid prefix; answer/change validation must not erase it.
func (e *Orchestrator) retainReviewLedger(job Job, result Result) error {
	if err := e.saveReviewLedger(job, result); err != nil {
		return fmt.Errorf("%w: retain private review ledger: %w", monitor.ErrTerminalJob, err)
	}
	return nil
}

func (e *Orchestrator) reviewLedgerRoot(job Job) (string, error) {
	if e.WorkRoot == "" {
		return "", errors.New("review evidence work root is required")
	}
	workRoot, err := workspace.PrepareRoot(e.WorkRoot)
	if err != nil {
		return "", err
	}
	providers := []string{job.Main.Provider}
	if job.Critic != nil {
		providers = append(providers, job.Critic.Provider)
	}
	if err := e.Runner.CheckWorkRootIsolation(workRoot, providers...); err != nil {
		return "", err
	}
	root := filepath.Join(workRoot, "reviews")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("review evidence root must be a private directory")
	}
	if err := workspace.CheckRoot(root); err != nil {
		return "", err
	}
	return root, nil
}

func (e *Orchestrator) saveReviewLedger(job Job, result Result) (resultErr error) {
	if job.Critic == nil && len(result.ReviewLedger) == 0 {
		return nil
	}
	if !result.matchesJob(job) {
		return errors.New("review evidence does not match action identity or source")
	}
	if err := job.Validate(); err != nil {
		return err
	}
	if err := result.validateCalls(job, false); err != nil {
		return err
	}
	if err := validateReviewLedger(job, result.Calls, result.ReviewLedger, false); err != nil {
		return err
	}
	record := retainedReview{
		Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, RecordedAt: time.Now().UTC(),
		CriticMaxRounds: job.CriticMaxRounds, CriticNonblockingRounds: job.CriticNonblockingRounds,
		Calls: result.Calls, ReviewLedger: result.ReviewLedger,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) > workspace.MaxBytes {
		return errors.New("review evidence exceeds transfer byte limit")
	}
	root, err := e.reviewLedgerRoot(job)
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
			len(saved.Calls) > len(record.Calls) || len(saved.ReviewLedger) > len(record.ReviewLedger) ||
			!slices.Equal(saved.Calls, record.Calls[:len(saved.Calls)]) || !slices.Equal(saved.ReviewLedger, record.ReviewLedger[:len(saved.ReviewLedger)]) {
			return errors.New("review evidence changed or discarded a saved prefix")
		}
		if len(saved.Calls) == len(record.Calls) && len(saved.ReviewLedger) == len(record.ReviewLedger) {
			return nil
		}
	}
	file, err := os.CreateTemp(root, ".review-*")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(file.Name()); !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	for _, directory := range []string{root, filepath.Dir(root)} {
		file, err := os.Open(directory)
		if err != nil {
			return err
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	e.logf("review evidence job=%s attempt=%s executor=%s entries=%d path=%q", job.Identity.JobID, job.Identity.AttemptID, job.Identity.ExecutorID, len(result.ReviewLedger), path)
	return nil
}
