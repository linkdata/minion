package action

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

func taskPublicationMatches(write state.PublicationWrite, job monitor.Job, kind string) bool {
	return (job.AttemptID == "" || write.AttemptID == job.AttemptID) && write.Kind == kind &&
		write.Repository == job.Repository.Name && write.Number == job.Number &&
		write.Marker == job.Marker() && write.Since.Equal(job.Source.UpdatedAt)
}

func existingTaskPublicationMatches(write state.PublicationWrite, job monitor.Job) bool {
	return (write.Kind == "comment" || write.Kind == "review") &&
		(job.AttemptID == "" || write.AttemptID == job.AttemptID) &&
		write.Repository == job.Repository.Name && write.Number == job.Number && write.Marker == job.Marker()
}

// resumeTaskPublication reconciles an issued write before any model work or new write.
func (e *Orchestrator) resumeTaskPublication(ctx context.Context, job monitor.Job) (bool, error) {
	if e.Store == nil {
		return false, nil
	}
	var write state.PublicationWrite
	var exists bool
	if err := e.Store.WithLock(func() error {
		write, exists = e.Store.Data.PublicationWrites[job.ID]
		if exists && !existingTaskPublicationMatches(write, job) {
			return fmt.Errorf("%w: issued publication belongs to another action or target", monitor.ErrAcceptanceState)
		}
		return nil
	}); err != nil {
		return true, err
	}
	if !exists {
		return false, nil
	}
	if write.Status == state.PublicationWritePending {
		if !time.Now().Before(write.Deadline) {
			return true, fmt.Errorf("%w: issued publication grace elapsed", monitor.ErrPostModelPublication)
		}
		if err := ctx.Err(); err != nil {
			return true, errors.Join(monitor.ErrPostModelPublication, err)
		}
		if e.GitHub == nil {
			return true, fmt.Errorf("%w: GitHub client is missing for issued publication", monitor.ErrAcceptanceState)
		}
		graceCtx, cancelGrace := context.WithDeadline(ctx, write.Deadline)
		defer cancelGrace()
		lookupCtx, cancelLookup := context.WithTimeout(graceCtx, 10*time.Second)
		defer cancelLookup()
		found, err := e.GitHub.HasVerifiedBotPublication(lookupCtx, write.Repository, write.Number, write.Since,
			github.User{ID: write.BotID, Login: write.BotLogin}, write.Marker, write.Kind)
		if err != nil {
			return true, fmt.Errorf("%w: check issued publication: %w", monitor.ErrPostModelPublication, err)
		}
		if !found {
			return true, fmt.Errorf("%w: issued publication is not yet observed", monitor.ErrPostModelPublication)
		}
		if err := e.observeTaskPublication(job); err != nil {
			return true, err
		}
	} else if write.Status != state.PublicationWriteObserved {
		return true, fmt.Errorf("%w: issued publication remains uncertain", monitor.ErrPostModelPublication)
	}
	if err := ctx.Err(); err != nil {
		return true, errors.Join(monitor.ErrPostModelPublication, err)
	}
	if !job.Deadline.IsZero() && !time.Now().Before(job.Deadline) {
		return true, errors.Join(monitor.ErrPostModelPublication, context.DeadlineExceeded)
	}
	return true, e.recordPublishedOutcome(job)
}

// beginTaskPublication saves the may-have-issued record before a GitHub POST.
func (e *Orchestrator) beginTaskPublication(ctx context.Context, job monitor.Job, kind string) error {
	return e.beginTaskPublicationAt(ctx, job, kind, time.Now)
}

func (e *Orchestrator) beginTaskPublicationAt(ctx context.Context, job monitor.Job, kind string, now func() time.Time) error {
	if e.Store == nil {
		return fmt.Errorf("%w: publication state is missing", monitor.ErrAcceptanceState)
	}
	if !state.ValidPublicationBotLogin(e.BotLogin) || e.BotID <= 0 {
		return fmt.Errorf("%w: bot identity is invalid before publication", monitor.ErrAcceptanceState)
	}
	return e.Store.WithLock(func() error {
		if previous, exists := e.Store.Data.PublicationWrites[job.ID]; exists {
			if !taskPublicationMatches(previous, job, kind) {
				return fmt.Errorf("%w: issued publication belongs to another action or target", monitor.ErrAcceptanceState)
			}
			return fmt.Errorf("%w: publication was already issued", monitor.ErrPostModelPublication)
		}
		started := now().UTC()
		if err := ctx.Err(); err != nil {
			return err
		}
		if !job.Deadline.IsZero() && !started.Before(job.Deadline) {
			return errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded)
		}
		if job.AttemptID != "" {
			accepted := e.Store.Data.Accepted
			if accepted == nil || !monitor.AssignmentMatchesAccepted(job, *accepted) || !accepted.Assigned {
				return fmt.Errorf("%w: selected assignment changed before publication", monitor.ErrAcceptanceState)
			}
			if accepted.Terminal {
				return fmt.Errorf("%w: selected action is terminal", monitor.ErrTerminalJob)
			}
			if _, decided := e.Store.Data.TerminalDecisions[job.ID]; decided {
				return fmt.Errorf("%w: selected action is already decided", monitor.ErrTerminalJob)
			}
			if !started.Before(accepted.Deadline) {
				return errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded)
			}
		} else if e.assignmentRequired {
			return fmt.Errorf("%w: selected attempt is missing before publication", monitor.ErrAcceptanceState)
		}
		grace := e.PublicationGrace
		if grace <= 0 {
			grace = time.Duration(config.DefaultPublicationGraceSeconds) * time.Second
		}
		attemptID := job.AttemptID
		if attemptID == "" {
			attemptID = e.attemptID
		}
		if attemptID == "" {
			var err error
			attemptID, err = monitor.NewAttemptID()
			if err != nil {
				return err
			}
		}
		if e.Store.Data.PublicationWrites == nil {
			e.Store.Data.PublicationWrites = make(map[string]state.PublicationWrite)
		}
		write := state.PublicationWrite{
			AttemptID: attemptID, Kind: kind, BotLogin: e.BotLogin, BotID: e.BotID, Repository: job.Repository.Name,
			Number: job.Number, Marker: job.Marker(), Since: job.Source.UpdatedAt,
			StartedAt: started, Deadline: started.Add(grace), Status: state.PublicationWritePending,
		}
		e.Store.Data.PublicationWrites[job.ID] = write
		abortBeforePOST := func(reason error) error {
			delete(e.Store.Data.PublicationWrites, job.ID)
			if err := e.Store.Save(); err != nil {
				e.Store.Data.PublicationWrites[job.ID] = write // A failed save may have left the record on disk.
				return errors.Join(reason, fmt.Errorf("%w: clear unissued publication: %w", monitor.ErrAcceptanceState, err))
			}
			return reason
		}
		if err := e.Store.Save(); err != nil {
			return abortBeforePOST(fmt.Errorf("%w: save issued publication: %w", monitor.ErrAcceptanceState, err))
		}
		if err := ctx.Err(); err != nil {
			return abortBeforePOST(err)
		}
		if !job.Deadline.IsZero() && !now().Before(job.Deadline) {
			return abortBeforePOST(errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded))
		}
		return nil
	})
}

// observeTaskPublication retains the remote effect even if the action later expires.
func (e *Orchestrator) observeTaskPublication(job monitor.Job) error {
	if e.Store == nil {
		return fmt.Errorf("%w: publication state is missing", monitor.ErrAcceptanceState)
	}
	return e.Store.WithLock(func() error {
		write, exists := e.Store.Data.PublicationWrites[job.ID]
		if !exists || !existingTaskPublicationMatches(write, job) {
			return fmt.Errorf("%w: issued publication changed before observation", monitor.ErrAcceptanceState)
		}
		if write.Status != state.PublicationWriteObserved {
			write.ResolvedAt = time.Now().UTC()
		}
		write.Status = state.PublicationWriteObserved
		e.Store.Data.PublicationWrites[job.ID] = write
		if err := e.Store.Save(); err != nil {
			return fmt.Errorf("%w: save observed publication: %w", monitor.ErrAcceptanceState, err)
		}
		return nil
	})
}

func (e *Orchestrator) publishTaskComment(ctx context.Context, job monitor.Job, message string) error {
	if job.SubjectType != "Issue" && job.SubjectType != "PullRequest" {
		return fmt.Errorf("cannot reply to %s", job.SubjectType)
	}
	if _, err := e.verifiedRepository(ctx, job.Repository); err != nil {
		return err
	}
	if err := e.beginTaskPublication(ctx, job, "comment"); err != nil {
		return err
	}
	if _, err := e.GitHub.CreateIssueComment(ctx, job.Repository.Name, job.Number, message+"\n\n"+job.Marker()); err != nil {
		return err
	}
	return e.observeTaskPublication(job)
}
