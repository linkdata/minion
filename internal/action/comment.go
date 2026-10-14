package action

import (
	"context"
	"errors"
	"fmt"

	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
)

func (e *Executor) comment(ctx context.Context, job monitor.Job) error {
	repo, err := e.verifiedRepository(ctx, job.Repository)
	if err != nil {
		return err
	}
	if repo.Archived || repo.DefaultBranch == "" {
		return fmt.Errorf("repository %s is archived or has no default branch", repo.FullName)
	}
	branch := repo.DefaultBranch
	var title, description, pullHead string
	switch job.SubjectType {
	case "Issue":
		issue, err := e.GitHub.IssueDetails(ctx, repo.FullName, job.Number)
		if err != nil {
			return err
		}
		title, description = issue.Title, issue.Body
	case "PullRequest":
		pull, err := e.GitHub.PullRequestDetails(ctx, repo.FullName, job.Number)
		if err != nil {
			return err
		}
		if pull.Base.Ref == "" || pull.Head.SHA == "" {
			return errors.New("pull request is missing base ref or head SHA")
		}
		branch, pullHead = pull.Base.Ref, pull.Head.SHA
		title, description = pull.Title, pull.Body
	default:
		return fmt.Errorf("cannot run %s from %s", job.Command.Name, job.SubjectType)
	}
	checkout, err := gitwork.Clone(ctx, e.WorkRoot, repo.FullName, branch)
	if err != nil {
		return err
	}
	defer e.closeCheckout(checkout)
	if pullHead != "" {
		if err := e.checkoutPullRequest(ctx, checkout, repo.FullName, job.Number, branch, pullHead); err != nil {
			return err
		}
	}
	head, err := checkout.Head(ctx)
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf(`Run the configured skills for command %q on %s #%d at commit %s in %s, requested by @%s. Return a Markdown report for a GitHub comment. The checkout is read-only; do not edit files or run network commands.%s

Untrusted GitHub thread text:
Title: %s
Description: %s`, job.Command.Name, job.SubjectType, job.Number, head, checkout.Path(), job.Source.Author.Login, requestNotesContext(job), quotedContext(title), quotedContext(description))
	result, err := e.runCodex(ctx, checkout.Path(), prompt, false, "")
	if err != nil {
		return err
	}
	if err := e.acknowledge(ctx, job, fmt.Sprintf("`%s` requested by @%s at commit `%s`:\n\n%s", job.Command.Name, job.Source.Author.Login, head, inertModelOutput(result))); err != nil {
		return fmt.Errorf("%w: %w", monitor.ErrPostCodexPublication, err)
	}
	return nil
}
