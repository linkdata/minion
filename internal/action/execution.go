package action

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/linkdata/minion/internal/monitor"
)

// Executor computes one accepted Job; supplied input is borrowed read-only.
// The caller owns the returned Result, which the implementation must not mutate afterward.
type Executor interface {
	Execute(context.Context, Job, Checkpoints) (Result, error)
}

// Checkpoints acknowledges required reported data before execution may continue.
// Methods are called sequentially; nil acknowledges applicable persistence.
// Review slices are borrowed read-only until the synchronous method returns.
type Checkpoints interface {
	BeforeCall(CallCheckpoint) error
	AfterCall(CallCheckpoint) error
	Review(ReviewCheckpoint) error
}

// assignmentCheckpoint is local to the selected worker. The acceptance record
// must be saved before the worker receives its assignment.
type assignmentCheckpoint interface {
	AssignmentRequired() bool
	BeforeAssignment(Job) error
	AssignmentNotDelivered() error
}

// CallCheckpoint carries pinned dispatch metadata, then returned usage and parsed verdict claims.
type CallCheckpoint struct {
	Version   int       `json:"version"`
	Identity  Identity  `json:"identity"`
	SourceRef string    `json:"source_ref"`
	Ordinal   int       `json:"ordinal"`
	Call      CallUsage `json:"call"`
}

// ReviewCheckpoint carries private worker-reported review data; a nonblank decision acknowledges an already saved prefix.
type ReviewCheckpoint struct {
	Version          int          `json:"version"`
	Identity         Identity     `json:"identity"`
	SourceRef        string       `json:"source_ref"`
	Calls            []CallUsage  `json:"calls,omitempty"`
	Refutations      []Refutation `json:"refutations,omitempty"`
	CriticStopReason string       `json:"critic_stop_reason,omitempty"`
}

// executionCheckpoints binds local persistence and acknowledgements to one attempt.
type executionCheckpoints struct {
	job                 Job
	owner               *Orchestrator
	pending             *CallUsage
	returned            *CallUsage
	calls               []CallUsage
	review              ReviewCheckpoint
	reviewed            bool
	failure             error
	assigned            bool
	assignmentContext   context.Context
	assignmentRequest   monitor.Job
	fixDestination      *FixDestination
	writeAcceptedResult func([]byte) error // Test seam for a save that crosses the deadline.
	saveAcceptedResult  func() error       // Test seam for the result commit crossing the deadline.
}

func newExecutionCheckpoints(owner *Orchestrator, job Job) *executionCheckpoints {
	if job.Critic != nil {
		critic := *job.Critic
		job.Critic = &critic
	}
	return &executionCheckpoints{owner: owner, job: job}
}

func callMetadata(call CallUsage) CallUsage {
	return CallUsage{Provider: call.Provider, Role: call.Role, Round: call.Round, ReviewPhase: call.ReviewPhase, Model: call.Model, Effort: call.Effort}
}

func (c *executionCheckpoints) bound(version int, identity Identity, source string) bool {
	return (Result{Version: version, Identity: identity, SourceRef: source}).matchesJob(c.job)
}

func (c *executionCheckpoints) fail(err error) error {
	if c.failure == nil {
		c.failure = err
	}
	return c.failure
}

func checkpointContractError() error {
	return fmt.Errorf("%w: execution checkpoint contract violated", monitor.ErrTerminalJob)
}

func (c *executionCheckpoints) AssignmentRequired() bool {
	return c.owner != nil && c.owner.assignmentRequired
}

func (c *executionCheckpoints) acceptedAssignment(job Job) bool {
	if c.owner == nil || c.owner.Store == nil || c.owner.Store.Data.Accepted == nil || job.Identity != c.job.Identity {
		return false
	}
	return jobMatchesAccepted(job, *c.owner.Store.Data.Accepted)
}

func (c *executionCheckpoints) BeforeAssignment(job Job) error {
	if c.failure != nil {
		return c.failure
	}
	accepted := false
	if c.owner != nil && c.owner.Store != nil {
		_ = c.owner.Store.WithLock(func() error {
			accepted = c.acceptedAssignment(job)
			return nil
		})
	}
	if !accepted {
		return c.fail(fmt.Errorf("%w: worker assignment differs from accepted action", monitor.ErrAcceptanceState))
	}
	if c.AssignmentRequired() {
		if c.owner.ValidateAssignment == nil || c.assignmentContext == nil {
			return c.fail(fmt.Errorf("%w: assignment revalidation is unavailable", monitor.ErrAcceptanceState))
		}
		if err := c.owner.ValidateAssignment(c.assignmentContext, c.assignmentRequest); err != nil {
			if errors.Is(err, monitor.ErrAcceptedDiscarded) {
				c.assigned = false // The accepted record and its marker were durably removed.
			}
			if errors.Is(err, monitor.ErrAcceptedRetry) {
				return err // A prior undelivered marker may need its release callback.
			}
			return c.fail(err)
		}
		if err := c.assignmentContext.Err(); err != nil {
			if time.Now().Before(c.assignmentRequest.Deadline) {
				return fmt.Errorf("%w: %w", monitor.ErrAcceptedRetry, err)
			}
			return c.fail(errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded))
		}
		if !time.Now().Before(c.assignmentRequest.Deadline) {
			return c.fail(errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded))
		}
	}
	return c.owner.Store.WithLock(func() error {
		if !c.acceptedAssignment(job) {
			return c.fail(fmt.Errorf("%w: accepted action changed during assignment revalidation", monitor.ErrAcceptanceState))
		}
		accepted := c.owner.Store.Data.Accepted
		_, decided := c.owner.Store.Data.TerminalDecisions[accepted.JobID]
		if accepted.Terminal || decided {
			return c.fail(fmt.Errorf("%w: accepted action is already terminal", monitor.ErrTerminalJob))
		}
		if !time.Now().Before(accepted.Deadline) {
			if _, err := c.owner.Store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "expired", time.Now()); err != nil {
				return c.fail(fmt.Errorf("%w: save expired assignment: %w", monitor.ErrAcceptanceState, err))
			}
			return c.fail(errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded))
		}
		if c.assignmentContext != nil {
			if err := c.assignmentContext.Err(); err != nil {
				return fmt.Errorf("%w: %w", monitor.ErrAcceptedRetry, err)
			}
		}
		if c.assigned {
			if !c.owner.Store.Data.Accepted.Assigned {
				return c.fail(fmt.Errorf("%w: assigned action marker changed before re-pull", monitor.ErrAcceptanceState))
			}
			return nil // A disconnected pull may be retried before any bytes are sent.
		}
		if c.owner.Store.Data.Accepted.Assigned {
			return c.fail(fmt.Errorf("%w: accepted action was already assigned", monitor.ErrAcceptanceState))
		}
		c.owner.Store.Data.Accepted.Assigned = true
		if err := c.owner.Store.Save(); err != nil {
			c.owner.Store.Data.Accepted.Assigned = false
			return c.fail(fmt.Errorf("%w: save worker assignment: %w", monitor.ErrAcceptanceState, err))
		}
		c.assigned = true
		return nil
	})
}

func (c *executionCheckpoints) AssignmentNotDelivered() error {
	if !c.assigned {
		return c.failure
	}
	return c.owner.Store.WithLock(func() error {
		if !c.acceptedAssignment(c.job) || !c.owner.Store.Data.Accepted.Assigned {
			return c.fail(fmt.Errorf("%w: accepted assignment changed before delivery", monitor.ErrAcceptanceState))
		}
		c.owner.Store.Data.Accepted.Assigned = false
		if err := c.owner.Store.Save(); err != nil {
			c.owner.Store.Data.Accepted.Assigned = true
			return c.fail(fmt.Errorf("%w: release undelivered assignment: %w", monitor.ErrAcceptanceState, err))
		}
		c.assigned = false
		return c.failure
	})
}

func (c *executionCheckpoints) BeforeCall(fact CallCheckpoint) error {
	if c.failure != nil {
		return c.failure
	}
	if !c.bound(fact.Version, fact.Identity, fact.SourceRef) || c.pending != nil || fact.Ordinal != len(c.calls)+1 || fact.Call != callMetadata(fact.Call) {
		return c.fail(checkpointContractError())
	}
	if c.job.Critic != nil && (!c.reviewed || !slices.Equal(c.review.Calls, c.calls)) {
		return c.fail(checkpointContractError())
	}
	calls := append(slices.Clone(c.calls), fact.Call)
	if err := (Result{Calls: calls}).validateCalls(c.job, false); err != nil {
		return c.fail(fmt.Errorf("%w: %w", checkpointContractError(), err))
	}
	var selectedCallAllowed func() error
	if c.AssignmentRequired() {
		if !c.assigned {
			return c.fail(fmt.Errorf("%w: selected assignment was not delivered before model call", monitor.ErrAcceptanceState))
		}
		if c.assignmentContext == nil || c.owner.Store == nil {
			return c.fail(fmt.Errorf("%w: selected action context or state is missing before model call", monitor.ErrAcceptanceState))
		}
		selectedCallAllowed = func() error {
			if cause := httpExecutionContextError(c.assignmentContext); cause != nil {
				return cause
			}
			accepted := c.owner.Store.Data.Accepted
			if accepted == nil || !jobMatchesAccepted(c.job, *accepted) || !accepted.Assigned {
				return fmt.Errorf("%w: selected assignment changed before model call", monitor.ErrAcceptanceState)
			}
			_, decided := c.owner.Store.Data.TerminalDecisions[accepted.JobID]
			if accepted.Terminal || decided {
				return fmt.Errorf("%w: selected action is already terminal", monitor.ErrTerminalJob)
			}
			if !time.Now().Before(accepted.Deadline) {
				if _, err := c.owner.Store.DecideTerminalLocked(accepted.JobID, accepted.AttemptID, "expired", time.Now()); err != nil {
					return fmt.Errorf("%w: save expired model call: %w", monitor.ErrAcceptanceState, err)
				}
				return errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded)
			}
			return nil
		}
		if err := c.owner.Store.WithLock(selectedCallAllowed); err != nil {
			return c.fail(err)
		}
	}
	if c.owner.usageCheckpoint != nil {
		c.owner.usageCheckpoint.record.ExecutorID = c.job.Identity.ExecutorID
		c.owner.usageCheckpoint.record.SourceRef = c.job.Source.Head
		if err := c.owner.usageCheckpoint.beforeCall(fact.Call); err != nil {
			return c.fail(err)
		}
	}
	if c.owner.Store != nil {
		if err := c.owner.Store.WithLock(func() error {
			if selectedCallAllowed != nil {
				if err := selectedCallAllowed(); err != nil {
					return err
				}
			}
			failure, exists := c.owner.Store.Data.JobFailures[c.job.Identity.JobID]
			if !failure.ModelCallStarted {
				failure.ModelCallStarted = true
				c.owner.Store.Data.JobFailures[c.job.Identity.JobID] = failure
				if err := c.owner.Store.Save(); err != nil {
					if exists {
						failure.ModelCallStarted = false
						c.owner.Store.Data.JobFailures[c.job.Identity.JobID] = failure
					} else {
						delete(c.owner.Store.Data.JobFailures, c.job.Identity.JobID)
					}
					return fmt.Errorf("save model call start: %w", err)
				}
			}
			if selectedCallAllowed != nil {
				return selectedCallAllowed()
			}
			return nil
		}); err != nil {
			return c.fail(err)
		}
	}
	call := fact.Call
	c.pending = &call
	return nil
}

// failureEvidence never promotes rejected Result fields into checkpoint facts.
func (c *executionCheckpoints) failureEvidence() Result {
	result := Result{Version: c.job.Version, Identity: c.job.Identity, SourceRef: c.job.Source.Head,
		Calls: slices.Clone(c.calls), Refutations: slices.Clone(c.review.Refutations), CriticStopReason: c.review.CriticStopReason}
	if c.returned != nil {
		result.Calls = append(result.Calls, *c.returned)
	}
	return result
}

func (c *executionCheckpoints) AfterCall(fact CallCheckpoint) error {
	if c.failure != nil {
		return c.failure
	}
	if !c.bound(fact.Version, fact.Identity, fact.SourceRef) || c.pending == nil || fact.Ordinal != len(c.calls)+1 || callMetadata(fact.Call) != *c.pending {
		return c.fail(checkpointContractError())
	}
	if err := (Result{Calls: append(slices.Clone(c.calls), fact.Call)}).validateCalls(c.job, false); err != nil {
		return c.fail(fmt.Errorf("%w: %w", checkpointContractError(), err))
	}
	call := fact.Call
	c.returned = &call
	if c.owner.usageCheckpoint != nil {
		if err := c.owner.usageCheckpoint.afterCall(fact.Call); err != nil {
			return c.fail(err)
		}
	}
	c.calls = append(c.calls, fact.Call)
	c.pending, c.returned = nil, nil
	return nil
}

func (c *executionCheckpoints) Review(fact ReviewCheckpoint) error {
	if c.failure != nil {
		return c.failure
	}
	if c.job.Critic == nil || !c.bound(fact.Version, fact.Identity, fact.SourceRef) || c.pending != nil || !slices.Equal(fact.Calls, c.calls) {
		return c.fail(checkpointContractError())
	}
	// A reached decision acknowledges an already saved receipt/refutation prefix.
	if fact.CriticStopReason != "" && (!c.reviewed || !slices.Equal(fact.Calls, c.review.Calls) || !slices.Equal(fact.Refutations, c.review.Refutations)) {
		return c.fail(checkpointContractError())
	}
	if added := len(fact.Refutations) - len(c.review.Refutations); added > 0 {
		if added != 1 || len(c.calls) == 0 || len(c.review.Calls) == len(c.calls) ||
			c.calls[len(c.calls)-1].Role != roleMain || fact.Refutations[len(fact.Refutations)-1].Round != c.calls[len(c.calls)-1].Round {
			return c.fail(checkpointContractError())
		}
	}
	result := Result{Version: fact.Version, Identity: fact.Identity, SourceRef: fact.SourceRef, Calls: fact.Calls, Refutations: fact.Refutations, CriticStopReason: fact.CriticStopReason}
	if err := saveReviewEvidence(c.owner.WorkRoot, c.job, result, c.owner.Logger, c.owner.Runner.secretValues()...); err != nil {
		if len(c.calls) == 0 {
			err = preparationFailure("prepare private review ledger", err)
		} else {
			err = fmt.Errorf("%w: checkpoint private review ledger: %w", monitor.ErrTerminalJob, err)
		}
		return c.fail(err)
	}
	c.review = fact
	c.review.Calls = slices.Clone(fact.Calls)
	c.review.Refutations = slices.Clone(fact.Refutations)
	c.reviewed = true
	return nil
}

func (c *executionCheckpoints) finish(result Result, runErr error) error {
	claimedSuccess := runErr == nil
	if c.failure != nil && !errors.Is(runErr, c.failure) {
		runErr = errors.Join(runErr, c.failure)
	}
	if claimedSuccess && c.AssignmentRequired() && !c.assigned {
		return c.fail(fmt.Errorf("%w: selected assignment was not delivered", monitor.ErrAcceptanceState))
	}
	if !claimedSuccess {
		// An admitted model call may have incurred a charge even without a receipt.
		// The parent owns this checkpoint; worker failure flags do not set retry policy.
		if !errors.Is(runErr, monitor.ErrTerminalJob) &&
			(c.pending != nil || c.returned != nil || len(c.calls) != 0) {
			runErr = errors.Join(runErr, monitor.ErrExecutionOutcomeUnknown)
		}
		// A failed AfterCall may return exactly its captured receipt after the acknowledged prefix.
		calls := c.calls
		if c.returned != nil {
			calls = append(slices.Clone(calls), *c.returned)
		}
		if !result.matchesJob(c.job) || (c.pending != nil && c.returned == nil) || !slices.Equal(result.Calls, calls) ||
			!slices.Equal(result.Refutations, c.review.Refutations) || result.CriticStopReason != c.review.CriticStopReason {
			return errors.Join(runErr, checkpointContractError())
		}
		return runErr
	}
	if c.failure != nil || c.pending != nil || !result.matchesJob(c.job) || !slices.Equal(result.Calls, c.calls) {
		return errors.Join(runErr, checkpointContractError())
	}
	if c.job.Critic != nil {
		if !c.reviewed || c.review.CriticStopReason == "" || result.CriticStopReason != c.review.CriticStopReason || !slices.Equal(result.Calls, c.review.Calls) || !slices.Equal(result.Refutations, c.review.Refutations) {
			return checkpointContractError()
		}
	}
	return nil
}
