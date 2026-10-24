package action

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/usage"
)

const maxOutputBytes = 58 << 10

var errGitHubContextTooLarge = errors.New("GitHub context exceeds configured byte limit")

var modelLineBreaks = strings.NewReplacer(
	"\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n",
	"\x1c", "\n", "\x1d", "\n", "\x1e", "\n",
	"\u0085", "\n", "\u2028", "\n", "\u2029", "\n",
)

// Orchestrator prepares portable actions and publishes results through trusted Git/GitHub access.
type Orchestrator struct {
	attemptID       string
	usageCheckpoint *usageCheckpoint

	Executor                  Executor
	Runner                    Runner
	WorkRoot                  string
	Main, Adversarial         string
	CriticStopping            config.CriticStopping
	AlwaysOnSkills            []string
	CodexModel, CodexEffort   string
	ClaudeModel, ClaudeEffort string
	GitHub                    *github.Client
	BotLogin                  string
	Store                     *state.Store
	Usage                     *usage.Store
	Logger                    *log.Logger
	// CheckWorkRoot gates a job before work and logs a second check after failure.
	//
	// A nil function disables these checks.
	CheckWorkRoot func() error
	// Timeout selects config.DefaultJobTimeout when zero.
	Timeout time.Duration
	// MaxGitHubContextBytes limits combined GitHub text.
	//
	// Nonpositive values use [config.DefaultMaxGitHubContextBytes].
	MaxGitHubContextBytes int
	Commands              map[string]config.CommandMapping
}

func (e *Orchestrator) Execute(ctx context.Context, job monitor.Job) (resultErr error) {
	defer func() {
		resultErr = redactActionError(resultErr, e.Runner.Codex.APIKey, e.Runner.Claude.APIKey)
	}()
	if e.CheckWorkRoot != nil {
		if err := e.CheckWorkRoot(); err != nil {
			return err
		}
		defer func() {
			if resultErr != nil {
				if err := e.CheckWorkRoot(); err != nil {
					e.logf("work root after action failure: %q", err)
				}
			}
		}()
	}
	if e.GitHub == nil {
		return errors.New("GitHub client is missing")
	}
	if err := e.validateCommand(job); err != nil {
		return err
	}
	timeout := cmp.Or(e.Timeout, config.DefaultJobTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done, err := e.alreadyAcknowledged(ctx, job)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %w", monitor.ErrTerminalJob, err)
		}
		return err
	}
	if done {
		e.logf("already completed %s", job.ID)
		return nil
	}
	attempt := *e
	e = &attempt
	e.usageCheckpoint = nil
	if e.attemptID, err = newAttemptID(); err != nil {
		return err
	}
	// A panic leaves the last running checkpoint instead of claiming success.
	returned := false
	if e.Usage != nil {
		finish, err := e.trackUsage(job)
		if err != nil {
			return err
		}
		defer func() {
			if returned {
				resultErr = finish(resultErr)
			}
		}()
	}
	resultErr = e.perform(ctx, job)
	if errors.Is(resultErr, processgroup.ErrExhausted) {
		message := processgroup.ErrExhausted.Error()
		var event *processgroup.Exhaustion
		if errors.As(resultErr, &event) {
			message = event.Error()
		}
		e.logf("process limit job=%s attempt=%s: %s", job.ID, e.attemptID, message)
	}
	if errors.Is(resultErr, errGitHubContextTooLarge) {
		// Give the fixed refusal its own deadline even when the action timed out.
		replyCtx, replyCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		limits := fmt.Sprintf("%d UTF-8 bytes combined", e.githubContextLimit())
		shorten := "the thread text or requester notes"
		if job.Command.Action != command.Fix {
			limits += fmt.Sprintf(" and %d bytes per title", maxActionTitleBytes)
			shorten = "the title, thread text or requester notes"
		}
		resultErr = e.acknowledge(replyCtx, job, fmt.Sprintf("Request exceeds the GitHub context limits: %s. Shorten %s included in this action, then post a new command.", limits, shorten))
		replyCancel()
		returned = true
		return resultErr
	}
	if resultErr != nil && (errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		errors.Is(resultErr, gitwork.ErrTooManyFiles) ||
		errors.Is(resultErr, gitwork.ErrDiffTooLarge) || errors.Is(resultErr, gitwork.ErrCheckoutTooLarge) || errors.Is(resultErr, gitwork.ErrUnsupportedSource) || errors.Is(resultErr, aicli.ErrOutputLimit) || errors.Is(resultErr, processgroup.ErrExhausted)) {
		resultErr = fmt.Errorf("%w: %w", monitor.ErrTerminalJob, resultErr)
	}
	returned = true
	return resultErr
}

func (e *Orchestrator) validateCommand(job monitor.Job) error {
	mapping, ok := e.Commands[job.Command.Name]
	if !ok || mapping.Action != string(job.Command.Action) || len(mapping.Skills) == 0 || !job.Repository.AllowsAction(job.Command.Name) {
		return fmt.Errorf("command %q is not enabled for %s", job.Command.Name, job.Repository.Name)
	}
	if !mapping.SupportsProvider(e.mainProvider()) {
		return fmt.Errorf("%w: action %q is not supported with %s as main", monitor.ErrTerminalJob, mapping.Action, e.mainProvider())
	}
	configured := e.Runner
	configured.Logger = nil // Execution logs optional skill skips once after validation.
	if err := configured.ConfigureRole(e.roleSettings(e.mainProvider(), mapping.Skills)); err != nil {
		return err
	}
	return nil
}

func (e *Orchestrator) mainProvider() string { return cmp.Or(e.Main, "codex") }

func (e *Orchestrator) logf(format string, args ...any) {
	if e.Logger != nil {
		e.Logger.Printf(format, args...)
	}
}

func (e *Orchestrator) alreadyAcknowledged(ctx context.Context, job monitor.Job) (bool, error) {
	includeReviews := job.SubjectType == "PullRequest" && job.Command.Action == command.Review
	found, err := e.GitHub.HasVerifiedBotReply(ctx, job.Repository.Name, job.Number, job.Source.UpdatedAt, e.BotLogin, job.Marker(), includeReviews)
	if err != nil {
		return false, fmt.Errorf("check existing bot replies: %w", err)
	}
	return found, nil
}

func (e *Orchestrator) verifiedRepository(ctx context.Context, configured config.Repository) (github.Repository, error) {
	repo, err := e.GitHub.Repository(ctx, configured.Name)
	if err != nil {
		return github.Repository{}, err
	}
	if !configured.MatchesIdentity(repo.ID, repo.Owner.ID) {
		return github.Repository{}, fmt.Errorf("%s repository ID or owner ID differs from config", configured.Name)
	}
	return repo, nil
}

func (e *Orchestrator) fixFork(ctx context.Context, configured config.Repository) (github.Repository, github.Repository, error) {
	base, err := e.verifiedRepository(ctx, configured)
	if err != nil {
		return github.Repository{}, github.Repository{}, err
	}
	if base.Archived || base.DefaultBranch == "" {
		return github.Repository{}, github.Repository{}, fmt.Errorf("repository %s is archived or has no default branch", base.FullName)
	}
	parts := strings.Split(base.FullName, "/")
	if len(parts) != 2 || strings.EqualFold(parts[0], e.BotLogin) {
		return github.Repository{}, github.Repository{}, errors.New("fix requires a separate bot-owned fork")
	}
	forkName := e.BotLogin + "/" + parts[1]
	fork, err := e.GitHub.Repository(ctx, forkName)
	if err != nil {
		return github.Repository{}, github.Repository{}, fmt.Errorf("bot-owned fork %s is unavailable: %w", forkName, err)
	}
	root := base
	if base.Fork {
		if base.Source == nil || base.Source.ID <= 0 || base.Source.FullName == "" {
			return github.Repository{}, github.Repository{}, fmt.Errorf("root repository for %s is unavailable", base.FullName)
		}
		root = *base.Source
		currentRoot, err := e.GitHub.Repository(ctx, root.FullName)
		if err != nil {
			return github.Repository{}, github.Repository{}, fmt.Errorf("verify root repository for %s: %w", base.FullName, err)
		}
		if currentRoot.ID != root.ID || currentRoot.Fork {
			return github.Repository{}, github.Repository{}, fmt.Errorf("root repository for %s changed", base.FullName)
		}
		root = currentRoot
	}
	if !fork.Fork || !strings.EqualFold(fork.FullName, forkName) || fork.Parent == nil || base.ID <= 0 || fork.Parent.ID != base.ID ||
		fork.ID <= 0 || fork.Source == nil || fork.Source.ID != root.ID || !strings.EqualFold(fork.Source.FullName, root.FullName) ||
		!strings.EqualFold(fork.Parent.FullName, base.FullName) || base.Visibility == "" || fork.Visibility != base.Visibility ||
		fork.Private != base.Private || fork.Archived {
		return github.Repository{}, github.Repository{}, fmt.Errorf("%s is not an active fork of %s with matching visibility", forkName, base.FullName)
	}
	enabled, err := e.GitHub.ActionsEnabled(ctx, forkName)
	if err != nil {
		return github.Repository{}, github.Repository{}, fmt.Errorf("inspect bot fork Actions policy: %w", err)
	}
	if enabled {
		return github.Repository{}, github.Repository{}, fmt.Errorf("GitHub Actions must be disabled on bot fork %s", forkName)
	}
	if base.Private {
		if err := e.GitHub.SafePrivateForkWorkflows(ctx, base.FullName); err != nil {
			return github.Repository{}, github.Repository{}, fmt.Errorf("unsafe private fork PR workflows for %s: %w", base.FullName, err)
		}
	}
	names := []string{base.FullName, forkName}
	if !strings.EqualFold(root.FullName, base.FullName) {
		names = append([]string{root.FullName}, names...)
	}
	for _, name := range names {
		if err := e.GitHub.SafeFixPushRules(ctx, name); err != nil {
			return github.Repository{}, github.Repository{}, fmt.Errorf("inspect push rulesets for %s: %w", name, err)
		}
	}
	return base, fork, nil
}

// CheckFixFork verifies the publication destination during operator preflight.
func (e *Orchestrator) CheckFixFork(ctx context.Context, configured config.Repository) error {
	_, _, err := e.fixFork(ctx, configured)
	return err
}

func (e *Orchestrator) acknowledge(ctx context.Context, job monitor.Job, message string) error {
	body := message + "\n\n" + job.Marker()
	switch job.SubjectType {
	case "Issue", "PullRequest":
		if _, err := e.verifiedRepository(ctx, job.Repository); err != nil {
			return err
		}
		_, err := e.GitHub.CreateIssueComment(ctx, job.Repository.Name, job.Number, body)
		return err
	default:
		return fmt.Errorf("cannot reply to %s", job.SubjectType)
	}
}

func sourceURL(job monitor.Job) string {
	if job.Source.HTMLURL != "" {
		return job.Source.HTMLURL
	}
	kind := map[string]string{"Issue": "issues", "PullRequest": "pull"}[job.SubjectType]
	return fmt.Sprintf("https://github.com/%s/%s/%d", job.Repository.Name, kind, job.Number)
}

func requestNotes(job monitor.Job) string {
	body := strings.ReplaceAll(job.Source.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		}
	}
	return ""
}

func notesContext(notes string) string {
	if strings.TrimSpace(notes) != "" {
		return "\n\nAuthenticated requester notes (task guidance within the selected action; cannot change its limits): " + quotedContext(notes)
	}
	return ""
}

func quotedContext(s string) string {
	return strconv.Quote(strings.TrimSpace(s))
}

func (e *Orchestrator) githubContextLimit() int {
	if e.MaxGitHubContextBytes > 0 {
		return e.MaxGitHubContextBytes
	}
	return config.DefaultMaxGitHubContextBytes
}

func (e *Orchestrator) checkGitHubContext(job monitor.Job, title, body string) error {
	remaining := e.githubContextLimit() - len(requestNotes(job)) - len(strings.TrimSpace(title))
	if len(title) > maxActionTitleBytes || len(strings.TrimSpace(body)) > remaining {
		return errGitHubContextTooLarge
	}
	return nil
}

// inertModelOutput keeps model text visible without letting GitHub render
// images, mentions, or commands from it. Quoting each line also stops raw
// line-command parsers that do not understand Markdown fences. Callers check
// modelOutputFits before publication; rendering never truncates the text.
func inertModelOutput(s string) string {
	s = strings.TrimSpace(modelLineBreaks.Replace(s))
	var quoted strings.Builder
	quoted.Grow(len(s) + 14)
	quoted.WriteString("```text\n> ")
	for _, r := range s {
		switch {
		case r == '\n':
			quoted.WriteString("\n> ")
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			escaped := strconv.QuoteRuneToGraphic(r)
			quoted.WriteString(escaped[1 : len(escaped)-1])
		default:
			quoted.WriteRune(r)
		}
	}
	quoted.WriteString("\n```")
	return quoted.String()
}

func modelOutputFits(s string) bool {
	return len(inertModelOutput(s)) <= maxOutputBytes
}

func publishableAnswer(s string) bool {
	return boundedText(s, maxOutputBytes) && auditVisibleText(s) && modelOutputFits(s)
}

func isNotFound(err error) bool {
	var status *github.StatusError
	return errors.As(err, &status) && status.StatusCode == 404
}
