package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

// AcceptedResultMaxBytes bounds the original Job and complete Result plus
// selected-action metadata and record framing beyond their separate wire caps.
const AcceptedResultMaxBytes int64 = httpJobBytes + httpOutcomeBytes + 1<<20

const acceptedResultMetadataBytes = 1<<20 - 4096

// Only a completed result already saved for replay can use this retry path.
var errAcceptedResultSavedForRetry = errors.New("accepted result saved for publication retry")

// AcceptedResultRecord retains the complete supplied input and validated output
// for publication after a process restart. Accepted is the save-time identity.
type AcceptedResultRecord struct {
	Accepted       state.AcceptedAction `json:"accepted"`
	Job            Job                  `json:"job"`
	Result         Result               `json:"result"`
	FixDestination *FixDestination      `json:"fix_destination,omitempty"`
}

// FixDestination pins only the GitHub facts used to approve a fix push.
type FixDestination struct {
	RepositoryID  int64  `json:"repository_id"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Visibility    string `json:"visibility"`
	ForkID        int64  `json:"fork_id"`
	ForkFullName  string `json:"fork_full_name"`
}

func originalFixDestination(repository, fork github.Repository) *FixDestination {
	return &FixDestination{
		RepositoryID: repository.ID, DefaultBranch: repository.DefaultBranch,
		Private: repository.Private, Visibility: repository.Visibility,
		ForkID: fork.ID, ForkFullName: fork.FullName,
	}
}

func (d *FixDestination) valid(accepted state.AcceptedAction) bool {
	return d != nil && d.RepositoryID == accepted.RepositoryID && d.RepositoryID > 0 &&
		d.DefaultBranch != "" && d.Visibility != "" && d.ForkID > 0 && workspace.ValidRepository(d.ForkFullName)
}

func (d *FixDestination) matches(repository, fork github.Repository) bool {
	return d != nil && d.RepositoryID == repository.ID && d.DefaultBranch == repository.DefaultBranch &&
		d.Private == repository.Private && d.Visibility == repository.Visibility &&
		d.ForkID == fork.ID && strings.EqualFold(d.ForkFullName, fork.FullName)
}

// MatchesAccepted compares immutable selected-action facts. Scheduler flags
// may change after the result is saved.
func (r AcceptedResultRecord) MatchesAccepted(current state.AcceptedAction) bool {
	a := r.Accepted
	return a.JobID == current.JobID && a.AttemptID == current.AttemptID && a.ExecutorID == current.ExecutorID &&
		a.Repository == current.Repository && a.RepositoryID == current.RepositoryID && a.OwnerID == current.OwnerID &&
		a.SubjectType == current.SubjectType && a.SubjectNumber == current.SubjectNumber &&
		a.SourceKey == current.SourceKey && a.SourceNodeID == current.SourceNodeID && a.SourceURL == current.SourceURL &&
		a.SourceUpdatedAt.Equal(current.SourceUpdatedAt) && strings.EqualFold(a.Requester, current.Requester) &&
		a.RequesterID == current.RequesterID && a.CommandName == current.CommandName && a.Action == current.Action &&
		a.AcceptedAt.Equal(current.AcceptedAt) && a.Deadline.Equal(current.Deadline)
}

func (r AcceptedResultRecord) Validate() error {
	if err := r.Accepted.Validate(); err != nil {
		return err
	}
	if _, err := httpEncode(r.Accepted, acceptedResultMetadataBytes); err != nil {
		return fmt.Errorf("accepted result metadata exceeds limit: %w", err)
	}
	if !r.Accepted.Assigned || r.Accepted.Terminal || !jobMatchesAccepted(r.Job, r.Accepted) {
		return errors.New("accepted result identity differs from selected action")
	}
	if err := r.Result.Validate(r.Job); err != nil {
		return err
	}
	if r.Job.Action == command.Fix && len(r.Result.Changes) != 0 {
		if !r.FixDestination.valid(r.Accepted) {
			return errors.New("changed fix result has no valid original destination")
		}
	} else if r.FixDestination != nil {
		return errors.New("fix destination is present without changed fix result")
	}
	return httpResultShape(r.Result)
}

// DecodeAcceptedResultRecord validates bounded, typed state sidecar contents.
func DecodeAcceptedResultRecord(data []byte) (AcceptedResultRecord, error) {
	var record AcceptedResultRecord
	if err := httpDecodeBytes(data, int(AcceptedResultMaxBytes), &record); err != nil {
		return AcceptedResultRecord{}, fmt.Errorf("accepted result: %w", err)
	}
	if err := record.Validate(); err != nil {
		return AcceptedResultRecord{}, err
	}
	return record, nil
}

func jobMatchesAccepted(job Job, a state.AcceptedAction) bool {
	i := job.Identity
	return i.JobID == a.JobID && i.AttemptID == a.AttemptID && i.ExecutorID == a.ExecutorID &&
		i.Repository == a.Repository && i.RepositoryID == a.RepositoryID && i.OwnerID == a.OwnerID &&
		i.SubjectType == a.SubjectType && i.Number == a.SubjectNumber &&
		strings.EqualFold(i.Requester, a.Requester) && i.RequesterID == a.RequesterID &&
		job.Command == a.CommandName && string(job.Action) == a.Action
}

// ReceiveCompletedResult is called only for a successful, sequence-complete
// HTTP outcome. Selected actions save their result before the HTTP ACK.
func (c *executionCheckpoints) ReceiveCompletedResult(result Result) error {
	if !c.AssignmentRequired() {
		return nil
	}
	if c.failure != nil {
		return c.failure
	}
	if err := c.finish(result, nil); err != nil {
		return c.fail(err)
	}
	if err := result.Validate(c.job); err != nil {
		return c.fail(fmt.Errorf("%w: invalid complete action result: %w", monitor.ErrTerminalJob, err))
	}
	published, err := projectPublication(c.job, result)
	if err == nil {
		err = rejectConfiguredKeyPublication(published, result.Changes, c.owner.Runner.secretValues()...)
	}
	if err == nil {
		err = rejectPublishedRefutations(c.job, result, published)
	}
	var auditUnavailable error
	if err == nil && c.job.Action == command.SecurityAudit {
		if c.assignmentContext == nil {
			err = errors.New("audit validation context is missing")
		} else {
			err = c.owner.validateAuditForPublication(c.assignmentContext, c.job, result.Artifacts)
			if errors.Is(err, errAuditPublicationUnavailable) {
				auditUnavailable, err = err, nil
			}
		}
	}
	if c.assignmentContext != nil {
		if cause := c.assignmentContext.Err(); cause != nil {
			return c.fail(cause)
		}
	}
	if err != nil {
		return c.fail(fmt.Errorf("%w: invalid publication result: %w", monitor.ErrTerminalJob, err))
	}
	if c.owner.Store == nil {
		return c.fail(fmt.Errorf("%w: selected action state is missing", monitor.ErrAcceptanceState))
	}
	var accepted state.AcceptedAction
	if err := c.owner.Store.WithLock(func() error {
		a := c.owner.Store.Data.Accepted
		if a == nil || !jobMatchesAccepted(c.job, *a) || !monitor.AssignmentMatchesAccepted(c.assignmentRequest, *a) ||
			!a.Assigned || a.Terminal {
			return c.fail(fmt.Errorf("%w: selected action changed before result save", monitor.ErrAcceptanceState))
		}
		if !time.Now().Before(a.Deadline) {
			return c.fail(errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded))
		}
		accepted = *a
		return nil
	}); err != nil {
		return err
	}
	record := AcceptedResultRecord{Accepted: accepted, Job: c.job, Result: result}
	if c.job.Action == command.Fix && len(result.Changes) != 0 {
		record.FixDestination = c.fixDestination
	}
	if err := record.Validate(); err != nil {
		return c.fail(fmt.Errorf("%w: accepted result record: %w", monitor.ErrTerminalJob, err))
	}
	data, err := httpEncode(record, int(AcceptedResultMaxBytes))
	if err != nil {
		return c.fail(fmt.Errorf("%w: encode accepted result: %w", monitor.ErrTerminalJob, err))
	}
	if err := c.owner.Store.WithLock(func() error {
		a := c.owner.Store.Data.Accepted
		if c.assignmentContext != nil {
			if cause := c.assignmentContext.Err(); cause != nil {
				return c.fail(cause)
			}
		}
		if a == nil || !record.MatchesAccepted(*a) || !a.Assigned || a.Terminal {
			return c.fail(fmt.Errorf("%w: selected action changed before result save", monitor.ErrAcceptanceState))
		}
		if !time.Now().Before(a.Deadline) {
			return c.fail(errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded))
		}
		if err := c.owner.Store.WriteAcceptedResult(data); err != nil {
			return c.fail(fmt.Errorf("%w: save accepted result: %w", monitor.ErrAcceptanceState, err))
		}
		return nil
	}); err != nil {
		return err
	}
	if auditUnavailable != nil {
		return c.fail(fmt.Errorf("%w: %w: audit validation unavailable: %w", errAcceptedResultSavedForRetry, monitor.ErrPostModelPublication, auditUnavailable))
	}
	return nil
}

var _ interface{ ReceiveCompletedResult(Result) error } = (*executionCheckpoints)(nil)
