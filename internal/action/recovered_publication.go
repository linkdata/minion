package action

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

// PublishRecovered resumes publication of a saved complete attempt. The monitor
// owns live-source validation and clears the record after durable completion.
func (e *Orchestrator) PublishRecovered(ctx context.Context, request monitor.Job) (resultErr error) {
	if e == nil || e.Store == nil {
		return fmt.Errorf("%w: accepted result store is missing", monitor.ErrAcceptanceState)
	}
	defer func() {
		if terminalGitworkFailure(resultErr) {
			resultErr = fmt.Errorf("%w: %w", monitor.ErrTerminalJob, resultErr)
		}
		if resultErr == nil {
			e.completeRecoveredUsage(request)
		}
		resultErr = redactActionError(resultErr, e.Runner.secretValues()...)
	}()
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(e.MaxRuntime, config.DefaultMaxRuntime))
	defer cancel()
	if !request.Deadline.IsZero() {
		if !time.Now().Before(request.Deadline) {
			return errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded)
		}
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, request.Deadline)
		defer stop()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.GitHub == nil {
		return fmt.Errorf("%w: GitHub client is missing", monitor.ErrAcceptanceState)
	}
	var accepted state.AcceptedAction
	var data []byte
	var completed bool
	if err := e.Store.WithLock(func() error {
		if e.Store.Data.Accepted == nil {
			return errors.New("accepted action is missing")
		}
		accepted = *e.Store.Data.Accepted
		decision, decided := e.Store.Data.TerminalDecisions[request.ID]
		if accepted.Terminal || decided {
			if decided && decision.AttemptID == request.AttemptID && decision.Outcome == "succeeded" &&
				monitor.AssignmentMatchesAccepted(request, accepted) {
				completed = true
				return nil
			}
			return fmt.Errorf("%w: accepted action is already terminal", monitor.ErrTerminalJob)
		}
		if !accepted.ResultReady {
			return errors.New("accepted result was not committed")
		}
		var err error
		data, err = e.Store.ReadAcceptedResult(AcceptedResultMaxBytes)
		return err
	}); err != nil {
		if errors.Is(err, monitor.ErrTerminalJob) {
			return err
		}
		return fmt.Errorf("%w: read accepted result: %w", monitor.ErrAcceptanceState, err)
	}
	if completed {
		return nil
	}
	record, err := DecodeAcceptedResultRecord(data)
	if err != nil {
		return fmt.Errorf("%w: decode accepted result: %w", monitor.ErrAcceptanceState, err)
	}
	if !record.MatchesAccepted(accepted) || !monitor.AssignmentMatchesAccepted(request, accepted) {
		return fmt.Errorf("%w: accepted result differs from current action", monitor.ErrAcceptanceState)
	}
	acknowledged, err := e.alreadyAcknowledged(ctx, request)
	if err != nil {
		return err
	}
	if acknowledged {
		return e.recordPublishedOutcome(request)
	}
	if e.CheckWorkRoot != nil {
		if err := e.CheckWorkRoot(); err != nil {
			return err
		}
		defer func() {
			if resultErr != nil {
				if err := e.CheckWorkRoot(); err != nil {
					e.logf("work root after recovered publication failure: %q", err)
				}
			}
		}()
	}
	p := &preparedAction{request: request, job: record.Job}
	if p.job.Action != command.Fix {
		err = e.publish(ctx, p, record.Result)
	} else {
		err = e.publishRecoveredFix(ctx, p, record.Result, record.FixDestination)
	}
	if err != nil {
		return err
	}
	return e.recordPublishedOutcome(request)
}

func (e *Orchestrator) publishRecoveredFix(ctx context.Context, p *preparedAction, result Result, original *FixDestination) error {
	request := p.request
	if len(request.ID) < 12 {
		return fmt.Errorf("%w: fix job has an invalid ID", monitor.ErrAcceptanceState)
	}
	issue, err := e.GitHub.IssueDetails(ctx, request.Repository.Name, request.Number)
	if err != nil {
		return err
	}
	p.issueNodeID = issue.NodeID
	if len(result.Changes) == 0 {
		return e.publish(ctx, p, result)
	}
	p.repository, p.fork, err = e.fixFork(ctx, request.Repository)
	if err != nil {
		return err
	}
	if !original.matches(p.repository, p.fork) {
		return fmt.Errorf("%w: fix destination changed since accepted result; refusing to publish", monitor.ErrTerminalJob)
	}
	p.branch = fmt.Sprintf("minion/fix-%d-%s", request.Number, request.ID[:12])
	p.existing, err = e.GitHub.FindPullRequestByHead(ctx, p.repository.FullName, p.repository.DefaultBranch, e.BotLogin+":"+p.branch)
	if err != nil {
		return err
	}
	var expected state.PendingFix
	_ = e.Store.WithLock(func() error {
		expected = e.Store.Data.PendingFixes[request.ID]
		return nil
	})
	remote, err := e.GitHub.Branch(ctx, p.fork.FullName, p.branch)
	if err == nil {
		if remote.Name != p.branch || expected.SHA == "" || remote.Commit.SHA != expected.SHA {
			return fmt.Errorf("%w: branch %s exists without a matching recorded bot commit; refusing to reuse it", monitor.ErrTerminalJob, p.branch)
		}
		published, err := e.preparePublication(ctx, p, result)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(expected.Cap, published.cap) {
			return fmt.Errorf("%w: saved fix review decision differs from completed result", monitor.ErrAcceptanceState)
		}
		return e.publishFixReply(ctx, p)
	}
	if !isNotFound(err) {
		return err
	}
	if p.existing != nil {
		return fmt.Errorf("branch %s has a pull request but no remote branch", p.branch)
	}
	if expected.SHA != "" {
		p.recoverFix = expected
	}
	if err := gitwork.Check(ctx, e.WorkRoot); err != nil {
		return fmt.Errorf("Git readiness before recovered fix: %w", err)
	}
	p.checkout, err = gitwork.RestoreCheckout(ctx, e.WorkRoot, p.job.Source)
	if err != nil {
		return err
	}
	defer func() {
		if err := p.checkout.Close(); err != nil {
			e.logf("close restored action: %v", err)
		}
	}()
	if err := p.checkout.NewBranch(ctx, p.branch); err != nil {
		return fmt.Errorf("create restored fix branch: %w", err)
	}
	return e.publish(ctx, p, result)
}
