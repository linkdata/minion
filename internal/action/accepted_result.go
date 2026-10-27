package action

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

// AcceptedResultMaxBytes bounds the original Job and complete Result plus
// selected-action metadata and record framing beyond their separate wire caps.
const AcceptedResultMaxBytes int64 = httpJobBytes + httpOutcomeBytes + 1<<20

const acceptedResultMetadataBytes = 1<<20 - 4096

// AcceptedResultRecord retains the complete supplied input and validated output
// for publication after a process restart. Accepted is the save-time identity.
type AcceptedResultRecord struct {
	Accepted state.AcceptedAction `json:"accepted"`
	Job      Job                  `json:"job"`
	Result   Result               `json:"result"`
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
	return httpResultShape(r.Result)
}

// DecodeAcceptedResultRecord validates bounded, typed state sidecar contents.
func DecodeAcceptedResultRecord(data []byte) (AcceptedResultRecord, error) {
	var record AcceptedResultRecord
	if int64(len(data)) > AcceptedResultMaxBytes {
		return record, errHTTPBodyLimit
	}
	if err := httpDecode(bytes.NewReader(data), int(AcceptedResultMaxBytes), &record); err != nil {
		return AcceptedResultRecord{}, err
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

func requestMatchesAccepted(job monitor.Job, a state.AcceptedAction) bool {
	return job.ID == a.JobID && job.AttemptID == a.AttemptID &&
		job.Repository.Name == a.Repository && job.Repository.ID == a.RepositoryID && job.Repository.OwnerID == a.OwnerID &&
		job.SubjectType == a.SubjectType && job.Number == a.SubjectNumber &&
		job.Source.Key == a.SourceKey && job.Source.NodeID == a.SourceNodeID && job.Source.HTMLURL == a.SourceURL &&
		job.Source.UpdatedAt.Equal(a.SourceUpdatedAt) &&
		strings.EqualFold(job.Source.Author.Login, a.Requester) && job.Source.Author.ID == a.RequesterID &&
		job.Command.Name == a.CommandName && string(job.Command.Action) == a.Action &&
		job.AcceptedAt.Equal(a.AcceptedAt) && job.Deadline.Equal(a.Deadline)
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
	if err == nil && c.job.Action == command.SecurityAudit {
		if c.assignmentContext == nil {
			err = errors.New("audit validation context is missing")
		} else {
			err = c.owner.validateAuditForPublication(c.assignmentContext, c.job, result.Artifacts)
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
		if a == nil || !jobMatchesAccepted(c.job, *a) || !requestMatchesAccepted(c.assignmentRequest, *a) ||
			!a.Assigned || a.Terminal || !time.Now().Before(a.Deadline) {
			return c.fail(fmt.Errorf("%w: selected action changed before result save", monitor.ErrAcceptanceState))
		}
		accepted = *a
		return nil
	}); err != nil {
		return err
	}
	record := AcceptedResultRecord{Accepted: accepted, Job: c.job, Result: result}
	if err := record.Validate(); err != nil {
		return c.fail(fmt.Errorf("%w: accepted result record: %w", monitor.ErrTerminalJob, err))
	}
	data, err := httpEncode(record, int(AcceptedResultMaxBytes))
	if err != nil {
		return c.fail(fmt.Errorf("%w: encode accepted result: %w", monitor.ErrTerminalJob, err))
	}
	return c.owner.Store.WithLock(func() error {
		a := c.owner.Store.Data.Accepted
		if a == nil || !record.MatchesAccepted(*a) || !jobMatchesAccepted(c.job, *a) ||
			!requestMatchesAccepted(c.assignmentRequest, *a) || !a.Assigned || a.Terminal || !time.Now().Before(a.Deadline) {
			return c.fail(fmt.Errorf("%w: selected action changed before result save", monitor.ErrAcceptanceState))
		}
		if err := c.owner.Store.WriteAcceptedResult(data); err != nil {
			return c.fail(fmt.Errorf("%w: save accepted result: %w", monitor.ErrAcceptanceState, err))
		}
		return nil
	})
}

var _ interface{ ReceiveCompletedResult(Result) error } = (*executionCheckpoints)(nil)
