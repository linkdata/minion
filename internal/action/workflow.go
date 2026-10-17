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
	"slices"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

// preparedAction owns orchestration-only publication state. Only Job crosses the worker boundary.
type preparedAction struct {
	job Job

	request    monitor.Job
	checkout   *gitwork.Checkout
	repository github.Repository
	fork       github.Repository
	branch     string
	existing   *github.PullRequest
	reply      string
}

// needsExecution distinguishes supplied work from fixed replies and publication recovery.
func (p *preparedAction) needsExecution() bool { return p.job.Version != 0 }

func (e *Orchestrator) perform(ctx context.Context, request monitor.Job) error {
	prepared, err := e.prepare(ctx, request)
	if err != nil {
		return err
	}
	defer func() {
		if err := prepared.checkout.Close(); err != nil {
			e.logf("close prepared action: %v", err)
		}
	}()
	var result Result
	if prepared.needsExecution() {
		runner := e.Runner
		runner.WorkRoot = e.WorkRoot
		if prepared.job.Critic != nil {
			workRoot, logger, job := e.WorkRoot, e.Logger, prepared.job
			runner.recordReview = func(result Result) error {
				return saveReviewLedger(workRoot, job, result, logger)
			}
		}
		result, err = runner.Run(ctx, prepared.job)
		if err != nil {
			e.logResult(prepared.job, result)
			if len(result.Calls) != 0 || len(result.ReviewLedger) != 0 {
				err = errors.Join(err, e.retainReviewLedger(prepared.job, result))
			}
			err = errors.Join(err, e.retainIncompleteAudit(prepared, result))
			if validationErr := result.validate(prepared.job, false); validationErr != nil {
				return errors.Join(err, fmt.Errorf("%w: invalid incomplete action result: %w", monitor.ErrTerminalJob, validationErr))
			}
			return err
		}
	}
	return e.publish(ctx, prepared, result)
}

func (e *Orchestrator) prepare(ctx context.Context, request monitor.Job) (prepared *preparedAction, resultErr error) {
	p := &preparedAction{request: request}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, p.checkout.Close())
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
	lookupTree := e.treeLookup(repo.FullName)
	if pullHead != "" {
		p.checkout, err = gitwork.ClonePullRequest(ctx, e.WorkRoot, repo.FullName, request.Number, branch, pullHead, lookupTree)
	} else {
		p.checkout, err = gitwork.Clone(ctx, e.WorkRoot, repo.FullName, branch, lookupTree, false)
	}
	if err != nil {
		return nil, err
	}
	if p.job, err = e.portableJob(ctx, p, title, description); err != nil {
		return nil, err
	}
	if err := p.checkout.Close(); err != nil {
		return nil, err
	}
	p.checkout = nil
	return p, nil
}

func (e *Orchestrator) roleSettings(provider string, required []string) RoleSettings {
	role := RoleSettings{Provider: provider, Skills: required, AlwaysOnSkills: e.AlwaysOnSkills}
	role.Model, role.Effort = e.CodexModel, e.CodexEffort
	if provider == "claude" {
		role.Model, role.Effort = e.ClaudeModel, e.ClaudeEffort
	}
	return role
}

func newAttemptID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("create action attempt ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

func (e *Orchestrator) treeLookup(repo string) func(context.Context, string) (github.GitTree, error) {
	return func(ctx context.Context, sha string) (github.GitTree, error) {
		return e.GitHub.GitTree(ctx, repo, sha)
	}
}

func (e *Orchestrator) portableJob(ctx context.Context, p *preparedAction, title, description string) (Job, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return Job{}, errors.New("action preparation requires a deadline")
	}
	request := p.request
	source, err := p.checkout.Snapshot(ctx, request.Command.History)
	if err != nil {
		return Job{}, err
	}
	budget := time.Until(deadline)
	if budget <= 0 {
		if err := ctx.Err(); err != nil {
			return Job{}, err
		}
		return Job{}, fmt.Errorf("%w: %w", monitor.ErrTerminalJob, context.DeadlineExceeded)
	}
	job := Job{
		Version: ProtocolVersion,
		Identity: Identity{
			JobID: request.ID, AttemptID: e.attemptID, ExecutorID: "local",
			Repository: request.Repository.Name, RepositoryID: request.Repository.ID, OwnerID: request.Repository.OwnerID,
			Requester: request.Source.Author.Login, RequesterID: request.Source.Author.ID,
			Number: request.Number, SubjectType: request.SubjectType,
		},
		Command: request.Command.Name, Action: request.Command.Action,
		Title: title, Description: description, Notes: requestNotes(request), Source: source,
		Main: e.roleSettings(e.mainProvider(), e.Commands[request.Command.Name].Skills), RuntimeBudget: budget,
	}
	if e.Adversarial != "" {
		critic := e.roleSettings(e.Adversarial, nil)
		job.Critic = &critic
		job.CriticMaxRounds = cmp.Or(e.CriticMaxRounds, config.DefaultCriticMaxRounds)
		job.CriticNonblockingRounds = config.ResolveCriticNonblockingRounds(job.CriticMaxRounds, e.CriticNonblockingRounds)
	}
	return job, nil
}

// publish validates a worker result before applying data or issuing GitHub writes.
func (e *Orchestrator) publish(ctx context.Context, p *preparedAction, result Result) error {
	if p.reply != "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		return e.acknowledge(ctx, p.request, p.reply)
	}
	if !p.needsExecution() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.request.Command.Action == command.Fix && p.branch != "" {
			if err := e.publishFixReply(ctx, p); err != nil {
				return fmt.Errorf("%w: %w", monitor.ErrPostModelPublication, err)
			}
			return nil
		}
		return errors.New("action has no prepared source")
	}
	e.logResult(p.job, result)
	if err := e.retainReviewLedger(p.job, result); err != nil {
		return errors.Join(err, e.retainIncompleteAudit(p, result))
	}
	if err := result.validate(p.job, true); err != nil {
		return errors.Join(fmt.Errorf("%w: invalid action result: %w", monitor.ErrTerminalJob, err), e.retainIncompleteAudit(p, result))
	}
	if p.job.Action == command.SecurityAudit {
		if err := e.retainAudit(p, result, true); err != nil {
			return fmt.Errorf("%w: retain completed audit: %w", monitor.ErrTerminalJob, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	request := p.request
	var err error
	switch request.Command.Action {
	case command.Review:
		if _, err = e.verifiedRepository(ctx, request.Repository); err != nil {
			break
		}
		body := fmt.Sprintf("Automated review requested by @%s:\n\n%s\n\n%s", request.Source.Author.Login, inertModelOutput(result.Answer), request.Marker())
		_, err = e.GitHub.CreatePullRequestReview(ctx, request.Repository.Name, request.Number, body, p.job.Source.Head)
	case command.Comment:
		err = e.acknowledge(ctx, request, fmt.Sprintf("`%s` requested by @%s at commit `%s`:\n\n%s", request.Command.Name, request.Source.Author.Login, p.job.Source.Head, inertModelOutput(result.Answer)))
	case command.SecurityAudit:
		err = e.acknowledge(ctx, request, fmt.Sprintf("Security audit requested by @%s at commit `%s`:\n\n%s", request.Source.Author.Login, p.job.Source.Head, inertModelOutput(result.Answer)))
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

func (e *Orchestrator) logResult(job Job, result Result) {
	id := job.Identity
	if validDiagnostics(result.Diagnostics) == nil {
		for _, diagnostic := range result.Diagnostics {
			// Full private messages belong in restricted action records, never journal text.
			e.logf("action diagnostic job=%s attempt=%s executor=%s code=%s", id.JobID, id.AttemptID, id.ExecutorID, diagnostic.Code)
		}
	}
	if result.matchesJob(job) && result.validateCalls(job, false) == nil {
		for _, call := range result.Calls {
			e.logf("action call job=%s attempt=%s executor=%s provider=%s role=%s round=%d review_phase=%s approved=%t no_blockers=%t has_blockers=%t policy_met=%t model=%q effort=%q complete=%t", id.JobID, id.AttemptID, id.ExecutorID, call.Provider, call.Role, call.Round, call.ReviewPhase, call.Approved, call.NoBlockers, call.HasBlockers, call.Role == roleCritic && job.acceptsCritique(call.Round, call.Approved, call.NoBlockers), call.Model, call.Effort, call.Complete)
		}
	}
}

func (e *Orchestrator) retainIncompleteAudit(p *preparedAction, result Result) error {
	if p.job.Action != command.SecurityAudit || len(result.Artifacts) == 0 || !result.matchesJob(p.job) || validateResultArtifacts(p.job, result.Artifacts, false) != nil {
		return nil
	}
	return e.retainAudit(p, result, false)
}

func (e *Orchestrator) retainAudit(p *preparedAction, result Result, complete bool) error {
	root, err := privateRecordsDir(e.WorkRoot, "audits")
	if err != nil {
		return err
	}
	prefix := "complete-"
	if !complete {
		prefix = "incomplete-"
	}
	dir, err := os.MkdirTemp(root, fmt.Sprintf("%s%s-%d-", prefix, strings.ReplaceAll(p.job.Identity.Repository, "/", "-"), p.job.Identity.Number))
	if err != nil {
		return err
	}
	files := result.Artifacts
	if !complete {
		files = slices.Concat(files, []workspace.File{{Path: "INCOMPLETE.txt", Mode: workspace.Regular, Data: []byte("The audit failed. These partial artifacts are incomplete and must not be published.\n")}})
	}
	if err := workspace.Materialize(dir, files); err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	e.logf("audit %s #%d at %s; reports: %s", p.job.Identity.Repository, p.job.Identity.Number, p.job.Source.Head, dir)
	return nil
}

func (e *Orchestrator) prepareFix(ctx context.Context, p *preparedAction) (*preparedAction, error) {
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
		if remote.Name != p.branch || expected == "" || remote.Commit.SHA != expected {
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
	p.checkout, err = gitwork.Clone(ctx, e.WorkRoot, repo.FullName, repo.DefaultBranch, e.treeLookup(repo.FullName), true)
	if err != nil {
		return nil, err
	}
	p.job, err = e.portableJob(ctx, p, "", content.BodyText)
	if err != nil {
		return nil, err
	}
	if err := workspace.ValidateFixPaths(p.job.Source.Files, p.job.Source.Omitted); err != nil {
		return nil, fmt.Errorf("%w: %w", gitwork.ErrUnsupportedSource, err)
	}
	if err := p.checkout.NewBranch(ctx, p.branch); err != nil {
		return nil, err
	}
	return p, nil
}

func (e *Orchestrator) publishFix(ctx context.Context, p *preparedAction, result Result) (resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("%w: %w", monitor.ErrPostModelPublication, resultErr)
		}
	}()
	request := p.request
	if len(result.Changes) == 0 {
		return e.acknowledge(ctx, request, "AI completed the request without changing files.\n\n"+inertModelOutput(result.Answer))
	}
	currentRepo, currentFork, err := e.fixFork(ctx, request.Repository)
	if err != nil {
		return err
	}
	repo, fork := p.repository, p.fork
	if currentRepo.ID != repo.ID || currentRepo.DefaultBranch != repo.DefaultBranch || currentRepo.Private != repo.Private || currentRepo.Visibility != repo.Visibility || currentFork.ID != fork.ID || !strings.EqualFold(currentFork.FullName, fork.FullName) {
		return errors.New("repository or fork changed during fix; refusing to push")
	}
	if err := p.checkout.Import(ctx, p.job.Source.Head, result.Changes); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w: import action result: %w", monitor.ErrTerminalJob, err)
	}
	if err := p.checkout.CommitAndPush(ctx, fork.FullName, p.branch, e.BotLogin, fmt.Sprintf("Fix #%d via %s", request.Number, e.BotLogin), func(sha string) error {
		e.Store.Data.PendingFixes[request.ID] = sha
		return e.Store.Save()
	}); err != nil {
		if errors.Is(err, gitwork.ErrNoChanges) {
			return fmt.Errorf("%w: reviewed changes produced no commit: %w", monitor.ErrTerminalJob, err)
		}
		return err
	}
	return e.publishFixReply(ctx, p)
}

func (e *Orchestrator) publishFixReply(ctx context.Context, p *preparedAction) error {
	request, repo, fork, branch := p.request, p.repository, p.fork, p.branch
	expected := e.Store.Data.PendingFixes[request.ID]
	if existing := p.existing; existing != nil {
		if existing.State != "open" || request.Source.Author.ID == 0 || existing.User.ID != request.Source.Author.ID ||
			existing.Head.Ref != branch || existing.Head.Repo == nil || !strings.EqualFold(existing.Head.Repo.FullName, fork.FullName) ||
			existing.Head.SHA != expected || existing.Base.Ref != repo.DefaultBranch {
			return fmt.Errorf("branch %s has a pull request without matching requester authorship and recorded head", branch)
		}
		return e.acknowledge(ctx, request, fmt.Sprintf("Fix branch `%s` at `%s` has a pull request opened by @%s: %s", branch, expected, existing.User.Login, existing.HTMLURL))
	}
	compare := fmt.Sprintf("https://github.com/%s/compare/%s...%s?expand=1", repo.FullName, url.PathEscape(repo.DefaultBranch), url.PathEscape(e.BotLogin+":"+branch))
	return e.acknowledge(ctx, request, fmt.Sprintf("Fix branch `%s` is ready in `%s` at `%s`. @%s, [open the pull request yourself](%s) after reviewing the diff. Repository review rules, when enabled, can then evaluate your authorship.", branch, fork.FullName, expected, request.Source.Author.Login, compare))
}
