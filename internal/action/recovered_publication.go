package action

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/linkdata/minion/internal/command"
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
	defer func() { resultErr = redactActionError(resultErr, e.Runner.secretValues()...) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.GitHub == nil {
		return errors.New("GitHub client is missing")
	}
	var accepted state.AcceptedAction
	var data []byte
	if err := e.Store.WithLock(func() error {
		if e.Store.Data.Accepted == nil {
			return errors.New("accepted action is missing")
		}
		accepted = *e.Store.Data.Accepted
		var err error
		data, err = e.Store.ReadAcceptedResult(AcceptedResultMaxBytes)
		return err
	}); err != nil {
		return fmt.Errorf("%w: read accepted result: %w", monitor.ErrAcceptanceState, err)
	}
	record, err := DecodeAcceptedResultRecord(data)
	if err != nil {
		return fmt.Errorf("%w: decode accepted result: %w", monitor.ErrAcceptanceState, err)
	}
	if !record.MatchesAccepted(accepted) || !requestMatchesAccepted(request, accepted) {
		return fmt.Errorf("%w: accepted result differs from current action", monitor.ErrAcceptanceState)
	}
	acknowledged, err := e.alreadyAcknowledged(ctx, request)
	if err != nil || acknowledged {
		return err
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
		return e.publish(ctx, p, record.Result)
	}
	return e.publishRecoveredFix(ctx, p, record.Result, record.FixDestination)
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
		return errors.New("fix destination changed since accepted result; refusing to publish")
	}
	p.branch = fmt.Sprintf("minion/fix-%d-%s", request.Number, request.ID[:12])
	p.existing, err = e.GitHub.FindPullRequestByHead(ctx, p.repository.FullName, p.repository.DefaultBranch, e.BotLogin+":"+p.branch)
	if err != nil {
		return err
	}
	var expected state.PendingFix
	if err := e.Store.WithLock(func() error {
		expected = e.Store.Data.PendingFixes[request.ID]
		return nil
	}); err != nil {
		return err
	}
	remote, err := e.GitHub.Branch(ctx, p.fork.FullName, p.branch)
	if err == nil {
		if remote.Name != p.branch || expected.SHA == "" || remote.Commit.SHA != expected.SHA {
			return fmt.Errorf("branch %s exists without a matching recorded bot commit; refusing to reuse it", p.branch)
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
		return fmt.Errorf("branch %s is unavailable with recorded bot commit %s; refusing to replace the checkpoint", p.branch, expected.SHA)
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
		return err
	}
	return e.publish(ctx, p, result)
}
