package action

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/linkdata/minion/internal/monitor"
)

// Executor computes one accepted Job; supplied input is borrowed read-only.
type Executor interface {
	Execute(context.Context, Job, Checkpoints) (Result, error)
}

// Checkpoints acknowledges required facts before execution may continue.
// Methods are called sequentially; nil acknowledges applicable persistence.
// Review slices are borrowed read-only until the synchronous method returns.
type Checkpoints interface {
	BeforeCall(CallCheckpoint) error
	AfterCall(CallCheckpoint) error
	Review(ReviewCheckpoint) error
}

// CallCheckpoint carries pinned dispatch metadata, then returned usage and parsed verdict attestations.
type CallCheckpoint struct {
	Version   int
	Identity  Identity
	SourceRef string
	Call      CallUsage
}

// ReviewCheckpoint carries private evidence; a nonblank decision acknowledges an already saved prefix.
type ReviewCheckpoint struct {
	Version          int
	Identity         Identity
	SourceRef        string
	Calls            []CallUsage
	Refutations      []Refutation
	CriticStopReason string
}

// executionCheckpoints binds local persistence and acknowledgements to one attempt.
type executionCheckpoints struct {
	job      Job
	owner    *Orchestrator
	pending  *CallUsage
	returned *CallUsage
	calls    []CallUsage
	review   ReviewCheckpoint
	reviewed bool
	failure  error
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

func (c *executionCheckpoints) BeforeCall(fact CallCheckpoint) error {
	if c.failure != nil {
		return c.failure
	}
	if !c.bound(fact.Version, fact.Identity, fact.SourceRef) || c.pending != nil || fact.Call != callMetadata(fact.Call) {
		return c.fail(checkpointContractError())
	}
	if c.job.Critic != nil && (!c.reviewed || !slices.Equal(c.review.Calls, c.calls)) {
		return c.fail(checkpointContractError())
	}
	calls := append(slices.Clone(c.calls), fact.Call)
	if err := (Result{Calls: calls}).validateCalls(c.job, false); err != nil {
		return c.fail(fmt.Errorf("%w: %w", checkpointContractError(), err))
	}
	if c.owner.usageCheckpoint != nil {
		if err := c.owner.usageCheckpoint.beforeCall(fact.Call); err != nil {
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
	if !c.bound(fact.Version, fact.Identity, fact.SourceRef) || c.pending == nil || callMetadata(fact.Call) != *c.pending {
		return c.fail(checkpointContractError())
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
	result := Result{Version: fact.Version, Identity: fact.Identity, SourceRef: fact.SourceRef, Calls: fact.Calls, Refutations: fact.Refutations, CriticStopReason: fact.CriticStopReason}
	if err := saveReviewEvidence(c.owner.WorkRoot, c.job, result, c.owner.Logger); err != nil {
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
	if !claimedSuccess {
		// A failed AfterCall may return exactly its captured receipt after the acknowledged prefix.
		calls := c.calls
		if c.returned != nil {
			calls = append(slices.Clone(calls), *c.returned)
		}
		if !result.matchesJob(c.job) || (c.pending != nil && c.returned == nil) || !slices.Equal(result.Calls, calls) {
			return errors.Join(runErr, checkpointContractError())
		}
		return runErr
	}
	if c.failure != nil || c.pending != nil || !result.matchesJob(c.job) || !slices.Equal(result.Calls, c.calls) {
		return errors.Join(runErr, checkpointContractError())
	}
	if c.job.Critic != nil && (!c.reviewed || c.review.CriticStopReason == "" || result.CriticStopReason != c.review.CriticStopReason || !slices.Equal(result.Calls, c.review.Calls) || !slices.Equal(result.Refutations, c.review.Refutations)) {
		return checkpointContractError()
	}
	return nil
}
