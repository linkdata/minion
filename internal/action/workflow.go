package action

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

// Prepared owns orchestration-only publication state. Only Job crosses the worker boundary.
type Prepared struct {
	Job Job

	request    monitor.Job
	checkout   *gitwork.Checkout
	repository github.Repository
	fork       github.Repository
	branch     string
	existing   *github.PullRequest
	reply      string
}

// NeedsExecution distinguishes supplied work from fixed replies and publication recovery.
func (p *Prepared) NeedsExecution() bool { return p != nil && p.checkout != nil }

// Close releases the trusted orchestration checkout after publication or failure.
func (p *Prepared) Close() error {
	if p == nil || p.checkout == nil {
		return nil
	}
	return p.checkout.Close()
}

func (e *Orchestrator) perform(ctx context.Context, request monitor.Job) error {
	prepared, err := e.prepare(ctx, request)
	if err != nil {
		return err
	}
	defer func() {
		if err := prepared.Close(); err != nil {
			e.logf("close prepared action: %v", err)
		}
	}()
	var result Result
	if prepared.NeedsExecution() {
		result, err = e.Runner.Run(ctx, prepared.Job)
		if err != nil {
			if validationErr := result.validate(prepared.Job, false); validationErr != nil {
				return errors.Join(err, fmt.Errorf("%w: invalid incomplete action result: %w", monitor.ErrTerminalJob, validationErr))
			}
			e.logDiagnostics(result)
			if prepared.Job.Action == command.SecurityAudit && len(result.Artifacts) != 0 {
				err = errors.Join(err, e.retainAudit(prepared, result, false))
			}
			return err
		}
	}
	return e.Publish(ctx, prepared, result)
}

func (e *Orchestrator) prepare(ctx context.Context, request monitor.Job) (prepared *Prepared, resultErr error) {
	p := &Prepared{request: request}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, p.Close())
		}
	}()
	if request.Command.Action == command.Fix {
		return e.prepareFix(ctx, p)
	}
	if request.Command.Action == command.Review && request.SubjectType != "PullRequest" {
		p.reply = fmt.Sprintf("`%s` applies to pull requests.", request.Command.Name)
		return p, nil
	}
	repo, err := e.verifiedRepository(ctx, request.Repository)
	if err != nil {
		return nil, err
	}
	if repo.Archived || repo.DefaultBranch == "" {
		return nil, fmt.Errorf("repository %s is archived or has no default branch", repo.FullName)
	}
	p.repository = repo
	branch := repo.DefaultBranch
	var title, description, pullHead string
	switch request.SubjectType {
	case "Issue":
		issue, err := e.GitHub.IssueDetails(ctx, repo.FullName, request.Number)
		if err != nil {
			return nil, err
		}
		title, description = issue.Title, issue.Body
	case "PullRequest":
		pull, err := e.GitHub.PullRequestDetails(ctx, repo.FullName, request.Number)
		if err != nil {
			return nil, err
		}
		if pull.Base.Ref == "" || pull.Head.SHA == "" {
			return nil, errors.New("pull request is missing base ref or head SHA")
		}
		branch, pullHead = pull.Base.Ref, pull.Head.SHA
		title, description = pull.Title, pull.Body
	default:
		return nil, fmt.Errorf("cannot run %s from %s", request.Command.Name, request.SubjectType)
	}
	if err := e.checkGitHubContext(request, title, description); err != nil {
		return nil, err
	}
	p.checkout, err = gitwork.Clone(ctx, e.Runner.WorkRoot, repo.FullName, branch, func(ctx context.Context, sha string) (github.GitTree, error) {
		return e.GitHub.GitTree(ctx, repo.FullName, sha)
	})
	if err != nil {
		return nil, err
	}
	if pullHead != "" {
		if err := p.checkout.CheckoutPullRequest(ctx, request.Number, branch, pullHead); err != nil {
			return nil, err
		}
	}
	if p.Job, err = e.portableJob(ctx, p, title, description); err != nil {
		return nil, err
	}
	return p, nil
}

func (e *Orchestrator) portableJob(ctx context.Context, p *Prepared, title, description string) (Job, error) {
	request := p.request
	source, err := p.checkout.Snapshot(ctx, request.Command.History, request.SubjectType == "PullRequest")
	if err != nil {
		return Job{}, err
	}
	var attempt [16]byte
	if _, err := rand.Read(attempt[:]); err != nil {
		return Job{}, fmt.Errorf("create action attempt: %w", err)
	}
	budget := cmp.Or(e.Timeout, config.DefaultJobTimeout)
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline))
	}
	settings := func(provider string, required []string) RoleSettings {
		role := RoleSettings{Provider: provider, Skills: required, AlwaysOnSkills: e.AlwaysOnSkills}
		role.Model, role.Effort = e.CodexModel, e.CodexEffort
		if provider == "claude" {
			role.Model, role.Effort = e.ClaudeModel, e.ClaudeEffort
		}
		return role
	}
	job := Job{
		Version: ProtocolVersion,
		Identity: Identity{
			JobID: request.ID, AttemptID: hex.EncodeToString(attempt[:]), ExecutorID: "local",
			Repository: request.Repository.Name, RepositoryID: request.Repository.ID, OwnerID: request.Repository.OwnerID,
			Requester: request.Source.Author.Login, RequesterID: request.Source.Author.ID,
			Number: request.Number, SubjectType: request.SubjectType,
		},
		Command: request.Command.Name, Action: request.Command.Action,
		Title: title, Description: description, Notes: requestNotes(request), Source: source,
		Main:            settings(e.mainProvider(), e.Commands[request.Command.Name].Skills),
		CriticMaxRounds: e.CriticMaxRounds, RuntimeBudget: budget,
	}
	if e.Adversarial != "" {
		critic := settings(e.Adversarial, nil)
		job.Critic = &critic
	}
	if err := job.Validate(); err != nil {
		return job, fmt.Errorf("%w: %w", monitor.ErrTerminalJob, err)
	}
	return job, nil
}

// Publish validates a worker result before applying data or issuing GitHub writes.
func (e *Orchestrator) Publish(ctx context.Context, p *Prepared, result Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil {
		return errors.New("action has no prepared source")
	}
	if p.reply != "" {
		return e.acknowledge(ctx, p.request, p.reply)
	}
	if p.checkout == nil {
		if p.request.Command.Action == command.Fix && p.branch != "" {
			return e.publishFixReply(ctx, p)
		}
		return errors.New("action has no prepared source")
	}
	if err := result.Validate(p.Job); err != nil {
		return fmt.Errorf("%w: invalid action result: %w", monitor.ErrTerminalJob, err)
	}
	e.logDiagnostics(result)
	if _, err := p.checkout.Head(ctx); err != nil {
		return err
	}
	if p.Job.Action == command.SecurityAudit {
		if err := e.retainAudit(p, result, true); err != nil {
			return err
		}
	}
	request := p.request
	var err error
	switch request.Command.Action {
	case command.Review:
		if _, err = e.verifiedRepository(ctx, request.Repository); err != nil {
			break
		}
		body := fmt.Sprintf("Automated review requested by @%s:\n\n%s\n\n%s", request.Source.Author.Login, inertModelOutput(result.Answer), request.Marker())
		_, err = e.GitHub.CreatePullRequestReview(ctx, request.Repository.Name, request.Number, body, p.Job.Source.Head)
	case command.Comment:
		err = e.acknowledge(ctx, request, fmt.Sprintf("`%s` requested by @%s at commit `%s`:\n\n%s", request.Command.Name, request.Source.Author.Login, p.Job.Source.Head, inertModelOutput(result.Answer)))
	case command.SecurityAudit:
		err = e.acknowledge(ctx, request, fmt.Sprintf("Security audit requested by @%s at commit `%s`:\n\n%s", request.Source.Author.Login, p.Job.Source.Head, inertModelOutput(result.Answer)))
	case command.Fix:
		return e.publishFix(ctx, p, result)
	default:
		return fmt.Errorf("unsupported action %q", request.Command.Action)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", monitor.ErrPostModelPublication, err)
	}
	return nil
}

func (e *Orchestrator) logDiagnostics(result Result) {
	for _, diagnostic := range result.Diagnostics {
		// Full private messages belong in restricted action records, never journal text.
		e.logf("action diagnostic job=%s attempt=%s executor=%s code=%s", result.Identity.JobID, result.Identity.AttemptID, result.Identity.ExecutorID, diagnostic.Code)
	}
}

func (e *Orchestrator) retainAudit(p *Prepared, result Result, complete bool) error {
	root := filepath.Join(e.Runner.WorkRoot, "audits")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	prefix := ""
	if !complete {
		prefix = "incomplete-"
	}
	dir, err := os.MkdirTemp(root, prefix+fmt.Sprintf("%s-%d-", strings.ReplaceAll(p.Job.Identity.Repository, "/", "-"), p.Job.Identity.Number))
	if err != nil {
		return err
	}
	if err := workspace.Materialize(dir, result.Artifacts); err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	if !complete {
		if err := os.WriteFile(filepath.Join(dir, "INCOMPLETE.txt"), []byte("The audit failed. These partial artifacts are incomplete and must not be published.\n"), 0600); err != nil {
			return err
		}
	}
	e.logf("audit %s #%d at %s; reports: %s", p.Job.Identity.Repository, p.Job.Identity.Number, p.Job.Source.Head, dir)
	return nil
}

func (e *Orchestrator) prepareFix(ctx context.Context, p *Prepared) (*Prepared, error) {
	request := p.request
	if request.SubjectType != "Issue" {
		p.reply = fmt.Sprintf("`%s` currently applies to issue threads. It pushes a branch for the requester to open a pull request.", request.Command.Name)
		return p, nil
	}
	issue, err := e.GitHub.IssueDetails(ctx, request.Repository.Name, request.Number)
	if err != nil {
		return nil, err
	}
	repo, fork, err := e.fixFork(ctx, request.Repository)
	if err != nil {
		return nil, err
	}
	if len(request.ID) < 12 || e.Store == nil {
		return nil, errors.New("fix job has no durable state or valid ID")
	}
	p.repository, p.fork = repo, fork
	p.branch = fmt.Sprintf("minion/fix-%d-%s", request.Number, request.ID[:12])
	p.existing, err = e.GitHub.FindPullRequestByHead(ctx, repo.FullName, repo.DefaultBranch, e.BotLogin+":"+p.branch)
	if err != nil {
		return nil, err
	}
	expected := e.Store.Data.PendingFixes[request.ID]
	remote, err := e.GitHub.Branch(ctx, fork.FullName, p.branch)
	if err == nil {
		if remote.Name != p.branch || expected == "" || !strings.EqualFold(remote.Commit.SHA, expected) {
			return nil, fmt.Errorf("branch %s exists without a matching recorded bot commit; refusing to reuse it", p.branch)
		}
		return p, nil
	}
	if !isNotFound(err) {
		return nil, err
	}
	if p.existing != nil {
		return nil, fmt.Errorf("branch %s has a pull request but no remote branch", p.branch)
	}
	if expected != "" {
		if _, err := e.GitHub.Branch(ctx, repo.FullName, p.branch); err == nil {
			return nil, fmt.Errorf("legacy base branch %s exists; review and retire it before a new fix request", p.branch)
		} else if !isNotFound(err) {
			return nil, fmt.Errorf("check legacy base branch: %w", err)
		}
		delete(e.Store.Data.PendingFixes, request.ID)
		if err := e.Store.Save(); err != nil {
			return nil, fmt.Errorf("clear failed fork push checkpoint: %w", err)
		}
	}
	content, err := e.GitHub.CurrentIssueContent(ctx, issue.NodeID)
	if err != nil {
		return nil, err
	}
	if request.Source.UpdatedAt.IsZero() || content.LastEditedAt != nil && !content.LastEditedAt.Before(request.Source.UpdatedAt) {
		p.reply = "The issue changed after this `fix` request, so no branch was created. Review its current text and post a new `fix` command."
		return p, nil
	}
	if err := e.checkGitHubContext(request, "", content.BodyText); err != nil {
		return nil, err
	}
	p.checkout, err = gitwork.Clone(ctx, e.Runner.WorkRoot, repo.FullName, repo.DefaultBranch, func(ctx context.Context, sha string) (github.GitTree, error) {
		return e.GitHub.GitTree(ctx, repo.FullName, sha)
	})
	if err != nil {
		return nil, err
	}
	if err := p.checkout.NewBranch(ctx, p.branch); err != nil {
		return nil, err
	}
	p.Job, err = e.portableJob(ctx, p, "", content.BodyText)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (e *Orchestrator) publishFix(ctx context.Context, p *Prepared, result Result) error {
	request := p.request
	if len(result.Changes) == 0 {
		if err := e.acknowledge(ctx, request, "AI completed the request without changing files.\n\n"+inertModelOutput(result.Answer)); err != nil {
			return fmt.Errorf("%w: %w", monitor.ErrPostModelPublication, err)
		}
		return nil
	}
	currentRepo, currentFork, err := e.fixFork(ctx, request.Repository)
	if err != nil {
		return err
	}
	repo, fork := p.repository, p.fork
	if currentRepo.ID != repo.ID || currentRepo.DefaultBranch != repo.DefaultBranch || currentRepo.Private != repo.Private || currentRepo.Visibility != repo.Visibility || currentFork.ID != fork.ID || !strings.EqualFold(currentFork.FullName, fork.FullName) {
		return errors.New("repository or fork changed during fix; refusing to push")
	}
	if err := p.checkout.Import(ctx, p.Job.Source.Head, result.Changes); err != nil {
		return fmt.Errorf("%w: import action result: %w", monitor.ErrTerminalJob, err)
	}
	if err := p.checkout.SetPushRepository(fork.FullName); err != nil {
		return err
	}
	if err := p.checkout.CommitAndPush(ctx, p.branch, e.BotLogin, fmt.Sprintf("Fix #%d via %s", request.Number, e.BotLogin), func(sha string) error {
		e.Store.Data.PendingFixes[request.ID] = sha
		return e.Store.Save()
	}); err != nil {
		if errors.Is(err, gitwork.ErrNoChanges) {
			if err := e.acknowledge(ctx, request, "AI completed the request without changing files.\n\n"+inertModelOutput(result.Answer)); err != nil {
				return fmt.Errorf("%w: %w", monitor.ErrPostModelPublication, err)
			}
			return nil
		}
		return err
	}
	return e.publishFixReply(ctx, p)
}

func (e *Orchestrator) publishFixReply(ctx context.Context, p *Prepared) error {
	request, repo, fork, branch := p.request, p.repository, p.fork, p.branch
	expected := e.Store.Data.PendingFixes[request.ID]
	if existing := p.existing; existing != nil {
		if existing.State != "open" || request.Source.Author.ID == 0 || existing.User.ID != request.Source.Author.ID ||
			existing.Head.Ref != branch || existing.Head.Repo == nil || !strings.EqualFold(existing.Head.Repo.FullName, fork.FullName) ||
			!strings.EqualFold(existing.Head.SHA, expected) || existing.Base.Ref != repo.DefaultBranch {
			return fmt.Errorf("branch %s has a pull request without matching requester authorship and recorded head", branch)
		}
		return e.acknowledge(ctx, request, fmt.Sprintf("Fix branch `%s` at `%s` has a pull request opened by @%s: %s", branch, expected, existing.User.Login, existing.HTMLURL))
	}
	compare := fmt.Sprintf("https://github.com/%s/compare/%s...%s?expand=1", repo.FullName, url.PathEscape(repo.DefaultBranch), url.PathEscape(e.BotLogin+":"+branch))
	return e.acknowledge(ctx, request, fmt.Sprintf("Fix branch `%s` is ready in `%s` at `%s`. @%s, [open the pull request yourself](%s) after reviewing the diff. Repository review rules, when enabled, can then evaluate your authorship.", branch, fork.FullName, expected, request.Source.Author.Login, compare))
}
