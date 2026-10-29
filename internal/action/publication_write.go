package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workspace"
)

func fixPushWriteKey(jobID string) string { return jobID + "-push" }

func fixPushMatches(write state.PublicationWrite, pending state.PendingFix, job monitor.Job) bool {
	return write.Kind == "push" && write.AttemptID == job.AttemptID &&
		write.Number == job.Number && write.Marker == job.Marker() &&
		write.Since.Equal(job.Source.UpdatedAt) && write.SHA == pending.SHA
}

// reconcileFixPush checks an issued push before recovered fix publication.
func (e *Orchestrator) reconcileFixPush(ctx context.Context, job monitor.Job) error {
	if job.Command.Action != command.Fix || e.Store == nil {
		return nil
	}
	var write state.PublicationWrite
	var pending state.PendingFix
	var exists, hasPending bool
	_ = e.Store.WithLock(func() error {
		write, exists = e.Store.Data.PublicationWrites[fixPushWriteKey(job.ID)]
		pending, hasPending = e.Store.Data.PendingFixes[job.ID]
		return nil
	})
	if !exists {
		return nil // Older SHA-only checkpoints can still match a remote branch.
	}
	if !hasPending || !fixPushMatches(write, pending, job) {
		return fmt.Errorf("%w: issued fix push differs from its commit checkpoint", monitor.ErrAcceptanceState)
	}
	if write.Status == state.PublicationWriteUncertain {
		return fmt.Errorf("%w: issued fix push remains uncertain", monitor.ErrPostModelPublication)
	}
	if write.Status == state.PublicationWriteObserved {
		return nil
	}
	if !time.Now().Before(write.Deadline) {
		return fmt.Errorf("%w: issued fix push grace elapsed", monitor.ErrPostModelPublication)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(monitor.ErrPostModelPublication, err)
	}
	if e.GitHub == nil {
		return fmt.Errorf("%w: GitHub client is missing for issued fix push", monitor.ErrAcceptanceState)
	}
	lookupCtx, cancel := context.WithDeadline(ctx, write.Deadline)
	defer cancel()
	lookupCtx, cancelLookup := context.WithTimeout(lookupCtx, 10*time.Second)
	defer cancelLookup()
	branch, err := e.GitHub.Branch(lookupCtx, write.Repository, write.Branch)
	if err != nil {
		return fmt.Errorf("%w: check issued fix push: %w", monitor.ErrPostModelPublication, err)
	}
	if branch.Name != write.Branch || branch.Commit.SHA != write.SHA {
		return fmt.Errorf("%w: issued fix branch differs from the recorded commit", monitor.ErrPostModelPublication)
	}
	return e.observeFixPush(job)
}

// beginFixPush saves the exact branch and PendingFix SHA before Git can push.
func (e *Orchestrator) beginFixPush(ctx context.Context, job monitor.Job, repository, branch, sha string, cap *state.CriticCap) error {
	return e.beginFixPushAt(ctx, job, repository, branch, sha, cap, time.Now)
}

func (e *Orchestrator) beginFixPushAt(ctx context.Context, job monitor.Job, repository, branch, sha string, cap *state.CriticCap, now func() time.Time) error {
	if e.Store == nil || job.Command.Action != command.Fix || !workspace.ValidIdentitySegment(job.AttemptID) ||
		!state.ValidPublicationBotLogin(e.BotLogin) || e.BotID <= 0 ||
		len(job.ID) < 12 || !workspace.ValidIdentitySegment(fixPushWriteKey(job.ID)) ||
		!workspace.ValidRepository(repository) || !workspace.ValidSHA(sha) ||
		branch != fmt.Sprintf("minion/fix-%d-%s", job.Number, job.ID[:12]) {
		return fmt.Errorf("%w: fix push identity or target is invalid", monitor.ErrAcceptanceState)
	}
	owner, _, _ := strings.Cut(repository, "/")
	if !strings.EqualFold(owner, strings.TrimSuffix(e.BotLogin, "[bot]")) {
		return fmt.Errorf("%w: fix push fork does not belong to bot", monitor.ErrAcceptanceState)
	}
	return e.Store.WithLock(func() error {
		key := fixPushWriteKey(job.ID)
		if _, exists := e.Store.Data.PublicationWrites[key]; exists {
			return fmt.Errorf("%w: fix push was already issued", monitor.ErrPostModelPublication)
		}
		if _, exists := e.Store.Data.PendingFixes[job.ID]; exists {
			return fmt.Errorf("%w: fix commit was already reserved", monitor.ErrPostModelPublication)
		}
		started := now().UTC()
		if err := ctx.Err(); err != nil {
			return err
		}
		accepted := e.Store.Data.Accepted
		if accepted == nil || !monitor.AssignmentMatchesAccepted(job, *accepted) || !accepted.Assigned || !accepted.ResultReady {
			return fmt.Errorf("%w: selected result changed before fix push", monitor.ErrAcceptanceState)
		}
		if accepted.Terminal || !started.Before(accepted.Deadline) {
			return errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded)
		}
		if _, decided := e.Store.Data.TerminalDecisions[job.ID]; decided {
			return fmt.Errorf("%w: selected action is already decided", monitor.ErrTerminalJob)
		}
		grace := e.PublicationGrace
		if grace <= 0 {
			grace = time.Duration(config.DefaultPublicationGraceSeconds) * time.Second
		}
		write := state.PublicationWrite{
			AttemptID: job.AttemptID, Kind: "push", BotLogin: e.BotLogin, BotID: e.BotID,
			Repository: repository, Number: job.Number, Marker: job.Marker(), Since: job.Source.UpdatedAt,
			Branch: branch, SHA: sha, StartedAt: started, Deadline: started.Add(grace), Status: state.PublicationWritePending,
		}
		e.Store.Data.PublicationWrites[key] = write
		e.Store.Data.PendingFixes[job.ID] = state.PendingFix{SHA: sha, Cap: cap}
		abortBeforePush := func(reason error) error {
			delete(e.Store.Data.PublicationWrites, key)
			delete(e.Store.Data.PendingFixes, job.ID)
			if err := e.Store.Save(); err != nil {
				return errors.Join(reason, fmt.Errorf("%w: clear unissued fix push: %w", monitor.ErrAcceptanceState, err))
			}
			return reason
		}
		if err := e.Store.Save(); err != nil {
			return abortBeforePush(fmt.Errorf("%w: save issued fix push: %w", monitor.ErrAcceptanceState, err))
		}
		if err := ctx.Err(); err != nil {
			return abortBeforePush(err)
		}
		if !now().Before(accepted.Deadline) {
			return abortBeforePush(errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded))
		}
		return nil
	})
}

func (e *Orchestrator) observeFixPush(job monitor.Job) error {
	return e.Store.WithLock(func() error {
		key := fixPushWriteKey(job.ID)
		write, exists := e.Store.Data.PublicationWrites[key]
		pending, hasPending := e.Store.Data.PendingFixes[job.ID]
		if !exists || !hasPending || !fixPushMatches(write, pending, job) {
			return fmt.Errorf("%w: issued fix push changed before observation", monitor.ErrAcceptanceState)
		}
		if write.Status == state.PublicationWriteUncertain {
			return fmt.Errorf("%w: issued fix push remains uncertain", monitor.ErrPostModelPublication)
		}
		if write.Status == state.PublicationWriteObserved {
			return nil
		}
		write.Status = state.PublicationWriteObserved
		write.ResolvedAt = time.Now().UTC()
		e.Store.Data.PublicationWrites[key] = write
		if err := e.Store.Save(); err != nil {
			if accepted := e.Store.Data.Accepted; accepted != nil && accepted.ResultReady &&
				monitor.AssignmentMatchesAccepted(job, *accepted) {
				return fmt.Errorf("%w: save observed fix push: %w", monitor.ErrPostModelPublication, err)
			}
			return fmt.Errorf("%w: save observed fix push: %w", monitor.ErrAcceptanceState, err)
		}
		return nil
	})
}

func taskPublicationMatches(write state.PublicationWrite, job monitor.Job, kind string) bool {
	return job.AttemptID != "" && write.AttemptID == job.AttemptID && write.Kind == kind &&
		write.Repository == job.Repository.Name && write.Number == job.Number &&
		write.Marker == job.Marker() && write.Since.Equal(job.Source.UpdatedAt)
}

func existingTaskPublicationMatches(write state.PublicationWrite, job monitor.Job) bool {
	return (write.Kind == "comment" || write.Kind == "review") &&
		job.AttemptID != "" && write.AttemptID == job.AttemptID &&
		write.Repository == job.Repository.Name && write.Number == job.Number && write.Marker == job.Marker()
}

// resumeTaskPublication reconciles an issued write before any model work or new write.
func (e *Orchestrator) resumeTaskPublication(ctx context.Context, job monitor.Job) (bool, error) {
	if job.AttemptID == "" {
		return true, fmt.Errorf("%w: selected attempt is missing before publication recovery", monitor.ErrAcceptanceState)
	}
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
	if job.AttemptID == "" {
		return fmt.Errorf("%w: selected attempt is missing before publication", monitor.ErrAcceptanceState)
	}
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
		grace := e.PublicationGrace
		if grace <= 0 {
			grace = time.Duration(config.DefaultPublicationGraceSeconds) * time.Second
		}
		if e.Store.Data.PublicationWrites == nil {
			e.Store.Data.PublicationWrites = make(map[string]state.PublicationWrite)
		}
		write := state.PublicationWrite{
			AttemptID: job.AttemptID, Kind: kind, BotLogin: e.BotLogin, BotID: e.BotID, Repository: job.Repository.Name,
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
		accepted := e.Store.Data.Accepted
		savedResult := accepted != nil && accepted.ResultReady &&
			accepted.JobID == job.ID && accepted.AttemptID == job.AttemptID
		if write.Status != state.PublicationWriteObserved {
			write.ResolvedAt = time.Now().UTC()
			if savedResult {
				accepted.RetryAt = time.Time{}
			}
		}
		write.Status = state.PublicationWriteObserved
		e.Store.Data.PublicationWrites[job.ID] = write
		if err := e.Store.Save(); err != nil {
			if savedResult {
				return fmt.Errorf("%w: save observed publication: %w", monitor.ErrPostModelPublication, err)
			}
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
