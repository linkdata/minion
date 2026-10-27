package monitor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
	"github.com/linkdata/minion/internal/workroot"
)

// GitHub provides the API operations used by Monitor.
type GitHub interface {
	ListRepositoryNotifications(context.Context, string, time.Time) ([]github.Notification, time.Duration, error)
	Repository(context.Context, string) (github.Repository, error)
	IssueDetails(context.Context, string, int) (github.Issue, error)
	ListRepositoryInvitations(context.Context) ([]github.Invitation, error)
	AcceptRepositoryInvitation(context.Context, int64) error
	DeclineRepositoryInvitation(context.Context, int64) error
	ThreadItems(context.Context, string, string, int, time.Time) ([]github.Item, error)
	CurrentItem(context.Context, github.Item) (github.Item, error)
	HasVerifiedBotReply(context.Context, string, int, time.Time, string, string, bool) (bool, error)
	Permission(context.Context, string, string) (github.Permission, error)
	ReactEyes(context.Context, string) error
	CreateIssueComment(context.Context, string, int, string) (github.Comment, error)
}

// ErrTerminalJob marks a failed execution that cannot succeed by rerunning the models.
var ErrTerminalJob = errors.New("terminal job failure")

// ErrExecutionOutcomeUnknown marks paid execution or returned facts whose outcome cannot be established.
// Such work must not be automatically repeated.
var ErrExecutionOutcomeUnknown = errors.New("execution outcome unknown")

// ErrRequestTransport retains a request failure's infrastructure retry classification.
var ErrRequestTransport = errors.New("request transport failure")

// ErrPostModelPublication marks publication failure after model work or during a fixed recovery reply.
//
// Without a model-call marker, retrying it consumes a job attempt even when the write error is transient.
var ErrPostModelPublication = errors.New("post-model publication failure")

// ErrAcceptanceState means the selected action's durable reservation is uncertain.
// The caller must stop before another action can be dispatched.
var ErrAcceptanceState = errors.New("action acceptance state failure")

// ErrAcceptedDiscarded means final assignment validation durably resolved an invalid request.
var ErrAcceptedDiscarded = errors.New("accepted action discarded before assignment")

// ErrAcceptedRetry means final assignment validation could not establish current validity.
var ErrAcceptedRetry = errors.New("accepted action assignment validation deferred")

const (
	maxJobAttempts          = 3
	maxFailureReplyAttempts = 3
	failureReply            = "Minion could not complete this request. No further automatic attempts will run. Check the service log and post a new command after the cause is fixed."
)

// ActionHandler runs the trusted orchestration and publication workflow.
type ActionHandler interface {
	Execute(context.Context, Job) error
}

// StoredResultPublisher publishes a previously accepted worker result without
// starting another executor. Only the orchestrator implements this optional path.
type StoredResultPublisher interface {
	PublishRecovered(context.Context, Job) error
}

type Job struct {
	Repository  config.Repository
	SubjectType string
	Number      int
	Source      github.Item
	Command     command.Command
	ID          string
	AttemptID   string
	AcceptedAt  time.Time
	Deadline    time.Time
}

type notificationScan struct {
	notification github.Notification
	since        time.Time
	key          string
}

func (j Job) Marker() string {
	return "<!-- linkdataminion:" + j.ID + " -->"
}

type Monitor struct {
	Config             config.Config
	ConfigPath         string
	GitHub             GitHub
	Handler            ActionHandler
	Logger             *log.Logger
	Now                func() time.Time
	SelectedCommands   map[string]bool // Non-nil freezes discovery to the selected worker's commands.
	SelectedExecutorID string          // Nonempty only while a selected local worker owns actions.
}

// NewAttemptID returns a random ID shared by acceptance and action records.
func NewAttemptID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("create action attempt ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

func (m *Monitor) logf(format string, args ...any) {
	if m.Logger != nil {
		m.Logger.Print(InertLogText(fmt.Sprintf(format, args...)))
	}
}

// InertLogText bounds and escapes untrusted text before it reaches a log line.
func InertLogText(s string) string {
	quoted := strconv.QuoteToGraphic(bounded.Text(s))
	visible := quoted[1 : len(quoted)-1]
	if len(visible) <= bounded.DiagnosticBytes {
		return visible
	}
	visible = visible[:bounded.DiagnosticBytes]
	for {
		_, err := strconv.Unquote("\"" + visible + "\"")
		if err == nil && utf8.ValidString(visible) {
			break
		}
		visible = visible[:len(visible)-1]
	}
	return visible + "..."
}

// RunOnce processes new notifications in whitelisted repositories.
//
// It persists each completed command before advancing that repository's cursor.
// Failed threads remain queued for retries without blocking other repositories.
// Parent cancellation preserves earlier failures and unprocessed retry state/cursors.
// Legacy executions reporting parent cancellation refund their attempt unless a model call began, publication began, or the execution outcome is unknown.
// A selected worker assignment without a stored result is terminal even when execution returns cancellation.
func (m *Monitor) RunOnce(ctx context.Context, store *state.Store) (time.Time, error) {
	selectedMode := m.SelectedExecutorID != ""
	if err := ctx.Err(); err != nil {
		if selectedMode {
			if next := store.Data.NextPollAt; !next.IsZero() {
				return earlierWake(next, m.terminalControlWake(store)), err
			}
			return m.terminalControlWake(store), err
		}
		return store.Data.NextPollAt, err
	}
	if store.Data.Accepted == nil {
		// A completed attempt may leave its sidecar after a crash or failed unlink.
		// Remove it before a different attempt can write the fixed sidecar path.
		if present, err := store.HasAcceptedResult(); err != nil {
			return store.Data.NextPollAt, fmt.Errorf("%w: inspect completed result: %w", ErrAcceptanceState, err)
		} else if present {
			if err := store.ClearAcceptedResult(); err != nil {
				return store.Data.NextPollAt, fmt.Errorf("%w: clear completed result: %w", ErrAcceptanceState, err)
			}
		}
	}
	now := m.now
	wake := func(next time.Time, service bool) time.Time {
		if !selectedMode {
			return earlierWake(next, store.Data.NextInviteCheckAt)
		}
		var terminal time.Time
		if service && ctx.Err() == nil {
			terminal = m.serviceTerminalControls(ctx, store)
		} else {
			terminal = m.terminalControlWake(store)
		}
		if next.IsZero() {
			if terminal.IsZero() {
				return next
			}
			return earlierWake(terminal, store.Data.NextInviteCheckAt)
		}
		return earlierWake(earlierWake(next, store.Data.NextInviteCheckAt), terminal)
	}
	if store.Data.Accepted != nil {
		if retryAt := store.Data.Accepted.RetryAt; time.Now().Before(retryAt) {
			if err := m.checkDueInvitations(ctx, store, now().UTC()); err != nil {
				return wake(retryAt, false), err
			}
			return wake(retryAt, true), nil
		}
		err := m.recoverAccepted(ctx, store)
		if errors.Is(err, ErrAcceptedDiscarded) {
			return wake(store.Data.NextPollAt, true), nil
		}
		if err != nil && !errors.Is(err, ErrAcceptanceState) && ctx.Err() == nil && store.Data.Accepted != nil {
			retryAt, saveErr := m.scheduleAcceptedRetry(store)
			return wake(retryAt, false), errors.Join(err, saveErr)
		}
		return wake(store.Data.NextPollAt, err == nil), err
	}
	var capacityErr error
	if selectedMode {
		capacityErr = m.waitCapacityWithInvitations(ctx, store)
	} else if capacityErr = m.checkDueInvitations(ctx, store, now().UTC()); capacityErr == nil {
		capacityErr = m.waitCapacity(ctx)
	}
	if capacityErr != nil {
		return wake(store.Data.NextPollAt, false), capacityErr
	}
	started := now().UTC()
	if started.Before(store.Data.NextPollAt) {
		return wake(store.Data.NextPollAt, false), nil
	}
	minInterval := m.pollInterval()
	next := started.Add(minInterval)
	store.Data.NextPollAt = next
	if store.Data.RepositoryCursors == nil {
		store.Data.RepositoryCursors = make(map[string]time.Time)
	}
	for _, repo := range m.Config.Repositories {
		if _, ok := store.Data.RepositoryCursors[repo.Name]; !ok {
			// Seed newly whitelisted repositories from the account cursor.
			store.Data.RepositoryCursors[repo.Name] = store.Data.Cursor
		}
	}
	if err := store.Save(); err != nil {
		return wake(next, false), err
	}
	commandSets := make(map[string]map[string]command.Action, len(m.Config.Repositories))
	for _, repo := range m.Config.Repositories {
		commandSets[repo.Name] = m.availableCommands(repo)
	}

	var scans []notificationScan
	seen := make(map[string]bool)
	var failures []error
	terminalFailures := make(map[int]string)
	recordTerminalFailure := func(id string, err error) {
		if err != nil {
			terminalFailures[len(failures)] = id
			failures = append(failures, err)
		}
	}
	clearResolvedTerminalFailures := func() {
		for index, id := range terminalFailures {
			if !store.Data.Processed[id].IsZero() {
				failures[index] = nil
				delete(terminalFailures, index)
			}
		}
	}
	stop := func(errs ...error) (time.Time, error) {
		var terminal time.Time
		if selectedMode {
			clearResolvedTerminalFailures()
			terminal = m.terminalControlWake(store)
		}
		err := errors.Join(slices.Concat(failures, errs)...)
		if canceled := ctx.Err(); canceled != nil && !errors.Is(err, canceled) {
			err = errors.Join(err, canceled)
		}
		if err != nil && !errors.Is(err, ErrAcceptanceState) && ctx.Err() == nil && store.Data.Accepted != nil {
			retryAt, saveErr := m.scheduleAcceptedRetry(store)
			return earlierWake(earlierWake(retryAt, store.Data.NextInviteCheckAt), terminal), errors.Join(err, saveErr)
		}
		return earlierWake(earlierWake(next, store.Data.NextInviteCheckAt), terminal), err
	}
	stopAt := func(location string, err error) (time.Time, error) {
		if err == nil {
			err = ctx.Err()
		}
		return stop(fmt.Errorf("%s: %w", location, err))
	}
	var polled []string
	for _, repo := range m.Config.Repositories {
		since := store.Data.RepositoryCursors[repo.Name]
		notifications, githubInterval, err := m.GitHub.ListRepositoryNotifications(ctx, repo.Name, since)
		if githubInterval > minInterval {
			minInterval = githubInterval
			next = started.Add(minInterval)
			store.Data.NextPollAt = next
			if saveErr := store.Save(); saveErr != nil {
				if err != nil {
					err = fmt.Errorf("list %s notifications: %w", repo.Name, err)
				}
				return stop(err, saveErr)
			}
		}
		if ctx.Err() != nil {
			return stopAt("list "+repo.Name+" notifications", err)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("list %s notifications: %w", repo.Name, err))
			continue
		}
		polled = append(polled, repo.Name)
		for _, notification := range notifications {
			key := notificationKey(notification)
			if seen[key] {
				continue
			}
			seen[key] = true
			scanSince := since
			if pending, ok := store.Data.PendingNotifications[key]; ok {
				if pending.RepositoryID != notification.Repository.ID || pending.OwnerID != notification.Repository.Owner.ID {
					delete(store.Data.PendingNotifications, key)
				} else {
					if started.Before(pending.RetryAt) && !notification.UpdatedAt.After(pending.UpdatedAt) {
						continue
					}
					scanSince = pending.Since
				}
			}
			scans = append(scans, notificationScan{notification, scanSince, key})
		}
	}
	for key, pending := range store.Data.PendingNotifications {
		if seen[key] || started.Before(pending.RetryAt) {
			continue
		}
		scans = append(scans, notificationScan{github.Notification{
			ID:         key,
			Repository: github.Repository{ID: pending.RepositoryID, FullName: pending.Repository, Owner: github.User{ID: pending.OwnerID}},
			Subject:    github.Subject{Type: pending.SubjectType, URL: pending.SubjectURL},
			UpdatedAt:  pending.UpdatedAt,
		}, pending.Since, key})
	}
	sort.SliceStable(scans, func(i, j int) bool {
		if scans[i].notification.UpdatedAt.Equal(scans[j].notification.UpdatedAt) {
			return scans[i].key < scans[j].key
		}
		return scans[i].notification.UpdatedAt.Before(scans[j].notification.UpdatedAt)
	})
	for scanIndex, entry := range scans {
		if ctx.Err() != nil {
			return stop()
		}
		notification := entry.notification
		repo, ok := m.Config.Repository(notification.Repository.FullName)
		if !ok {
			delete(store.Data.PendingNotifications, entry.key)
			continue
		}
		if !repo.MatchesIdentity(notification.Repository.ID, notification.Repository.Owner.ID) {
			m.logf("skip %s notification %s: repository identity differs from configured IDs", repo.Name, notification.ID)
			delete(store.Data.PendingNotifications, entry.key)
			continue
		}
		liveRepo, err := m.GitHub.Repository(ctx, repo.Name)
		if ctx.Err() != nil {
			return stopAt("verify "+repo.Name+" repository identity", err)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("verify %s repository identity: %w", repo.Name, err))
			if err := m.deferNotification(store, entry.key, notification, entry.since, started); err != nil {
				return stop(err)
			}
			continue
		}
		if !repo.MatchesIdentity(liveRepo.ID, liveRepo.Owner.ID) {
			m.logf("skip %s notification %s: live repository identity differs from configured IDs", repo.Name, notification.ID)
			delete(store.Data.PendingNotifications, entry.key)
			continue
		}
		if notification.Subject.Type != "Issue" && notification.Subject.Type != "PullRequest" {
			m.logf("skip %s notification %s: unsupported subject %q", repo.Name, notification.ID, notification.Subject.Type)
			delete(store.Data.PendingNotifications, entry.key)
			continue
		}
		number, err := subjectNumber(notification.Subject.URL, repo.Name, notification.Subject.Type)
		if err != nil {
			m.logf("skip %s notification %s: %v", repo.Name, notification.ID, err)
			delete(store.Data.PendingNotifications, entry.key)
			continue
		}
		firstFailure := len(failures)
		entryFailed := func() bool {
			for _, err := range failures[firstFailure:] {
				if err != nil {
					return true
				}
			}
			return false
		}
		entryDeferred := false
		deferEntry := func() error {
			if entryDeferred {
				return nil
			}
			if err := m.deferNotification(store, entry.key, notification, entry.since, started); err != nil {
				return err
			}
			entryDeferred = true
			return nil
		}
		items, err := m.GitHub.ThreadItems(ctx, repo.Name, notification.Subject.Type, number, entry.since)
		if ctx.Err() != nil {
			return stopAt(fmt.Sprintf("%s %s #%d", repo.Name, notification.Subject.Type, number), err)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s %s #%d: %w", repo.Name, notification.Subject.Type, number, err))
			if err := deferEntry(); err != nil {
				return stop(err)
			}
			continue
		}
		for _, item := range items {
			if strings.EqualFold(item.Author.Login, m.Config.BotLogin) || item.Author.Login == "" {
				continue
			}
			preliminary, err := command.ParseForLoginCommands(item.Body, m.Config.BotLogin, commandSets[repo.Name])
			if errors.Is(err, command.ErrNoCommand) {
				continue
			}
			if err != nil {
				m.logf("skip command in %s: %v", itemLocation(item), err)
				continue
			}
			if _, done := store.Data.Processed[jobID(item.Key, preliminary.Name)]; done {
				continue
			}
			current, err := m.GitHub.CurrentItem(ctx, item)
			if ctx.Err() != nil {
				return stopAt("verify command in "+itemLocation(item), err)
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("verify command in %s: %w", itemLocation(item), err))
				continue
			}
			if item.Author.ID <= 0 || current.Author.ID != item.Author.ID || !strings.EqualFold(current.Author.Login, item.Author.Login) {
				m.logf("ignore command in %s: author identity changed or is missing", itemLocation(item))
				continue
			}
			item = current
			if strings.EqualFold(item.Author.Login, m.Config.BotLogin) || item.Author.Login == "" {
				continue
			}
			cmd, err := command.ParseForLoginCommands(item.Body, m.Config.BotLogin, commandSets[repo.Name])
			if errors.Is(err, command.ErrNoCommand) {
				continue
			}
			if err != nil {
				m.logf("skip command in %s: %v", itemLocation(item), err)
				continue
			}
			job := Job{
				Repository:  repo,
				SubjectType: notification.Subject.Type,
				Number:      number,
				Source:      item,
				Command:     cmd,
				ID:          jobID(item.Key, cmd.Name),
			}
			if _, done := store.Data.Processed[job.ID]; done {
				continue
			}
			if item.Edited {
				// The latest editor may not be the actor who introduced a
				// retained command. Do not run commands from edited content.
				m.logf("ignore edited command in %s (last editor @%s)", itemLocation(item), item.Editor.Login)
				store.Data.Processed[job.ID] = started
				delete(store.Data.JobFailures, job.ID)
				if err := store.Save(); err != nil {
					return stop(err)
				}
				continue
			}
			if !repo.AllowsAction(cmd.Name) || !repo.AllowsActor(item.Author.Login, item.Author.ID) {
				m.logf("ignore %s from @%s in %s: disabled action or actor", cmd.Action, item.Author.Login, repo.Name)
				store.Data.Processed[job.ID] = started
				delete(store.Data.JobFailures, job.ID)
				if err := store.Save(); err != nil {
					return stop(err)
				}
				continue
			}
			permission, err := m.GitHub.Permission(ctx, repo.Name, item.Author.Login)
			if ctx.Err() != nil {
				return stopAt(fmt.Sprintf("check @%s permission in %s", item.Author.Login, repo.Name), err)
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("check @%s permission in %s: %w", item.Author.Login, repo.Name, err))
				continue
			}
			if permission.User.ID != item.Author.ID || !strings.EqualFold(permission.User.Login, item.Author.Login) {
				m.logf("ignore %s from @%s in %s: permission identity differs", cmd.Action, item.Author.Login, repo.Name)
				store.Data.Processed[job.ID] = started
				delete(store.Data.JobFailures, job.ID)
				if err := store.Save(); err != nil {
					return stop(err)
				}
				continue
			}
			if permission.Permission != "write" && permission.Permission != "admin" && permission.Permission != "maintain" {
				m.logf("ignore %s from @%s in %s: permission %q", cmd.Action, item.Author.Login, repo.Name, permission.Permission)
				store.Data.Processed[job.ID] = started
				delete(store.Data.JobFailures, job.ID)
				if err := store.Save(); err != nil {
					return stop(err)
				}
				continue
			}
			selected := selectedMode
			jobFailure := store.Data.JobFailures[job.ID]
			jobFailure.NotificationKey = entry.key
			if jobFailure.ModelCallStarted {
				jobFailure.Attempts = maxJobAttempts
			}
			if jobFailure.Attempts >= maxJobAttempts {
				recordTerminalFailure(job.ID, m.finishFailedJob(ctx, store, job, started, jobFailure))
				if ctx.Err() != nil {
					return stop()
				}
				continue
			}
			if !jobFailure.Deadline.IsZero() && !time.Now().Before(jobFailure.Deadline) {
				jobFailure.Attempts = maxJobAttempts
				store.Data.JobFailures[job.ID] = jobFailure
				if err := saveJobResult(store, selected); err != nil {
					return stop(err)
				}
				recordTerminalFailure(job.ID, m.finishFailedJob(ctx, store, job, started, jobFailure))
				continue
			}
			if selected {
				if store.Data.Accepted != nil {
					return stop(fmt.Errorf("%w: another selected action remains accepted", ErrAcceptanceState))
				}
				job.AttemptID, err = NewAttemptID()
				if err != nil {
					return stop(err)
				}
				if err := m.originalTiming(&jobFailure); err != nil {
					return stop(err)
				}
				job.AcceptedAt, job.Deadline = jobFailure.AcceptedAt, jobFailure.Deadline
				accepted := &state.AcceptedAction{
					JobID: job.ID, AttemptID: job.AttemptID, ExecutorID: m.SelectedExecutorID,
					Repository: repo.Name, RepositoryID: repo.ID, OwnerID: repo.OwnerID,
					SubjectType: job.SubjectType, SubjectNumber: number,
					SourceKey: item.Key, SourceNodeID: item.NodeID, SourceURL: item.HTMLURL, SourceUpdatedAt: item.UpdatedAt,
					Requester: item.Author.Login, RequesterID: item.Author.ID,
					CommandName: cmd.Name, Action: string(cmd.Action), AcceptedAt: job.AcceptedAt, Deadline: job.Deadline,
				}
				if err := accepted.Validate(); err != nil {
					return stop(fmt.Errorf("accept %s: %w", itemLocation(item), err))
				}
				// Save an earlier reply failure's backoff before Execute can service this thread.
				if entryFailed() {
					if err := deferEntry(); err != nil {
						return stop(err)
					}
				}
				jobFailure.Attempts++
				store.Data.JobFailures[job.ID] = jobFailure
				store.Data.Accepted = accepted
				for _, unaccepted := range scans[scanIndex:] {
					if pending, exists := store.Data.PendingNotifications[unaccepted.key]; exists &&
						unaccepted.notification.UpdatedAt.After(pending.UpdatedAt) {
						pending.UpdatedAt = unaccepted.notification.UpdatedAt
						pending.RetryAt = time.Time{} // A new thread update supersedes the old retry delay.
						store.Data.PendingNotifications[unaccepted.key] = pending
					}
					rememberNotification(store, unaccepted.key, unaccepted.notification, unaccepted.since)
				}
				if err := saveAcceptanceState(store); err != nil {
					return stop(err)
				}
			}
			err = m.GitHub.ReactEyes(ctx, item.NodeID)
			if ctx.Err() != nil {
				return stopAt("react to "+itemLocation(item), err)
			}
			if err != nil {
				if selected {
					if !time.Now().Before(job.Deadline) {
						if err := m.terminalAccepted(ctx, store, job, jobFailure); err != nil {
							return stop(err)
						}
						continue
					}
					return stop(fmt.Errorf("react to %s: %w", itemLocation(item), err))
				}
				failures = append(failures, fmt.Errorf("react to %s: %w", itemLocation(item), err))
				continue
			}
			if selected && !time.Now().Before(job.Deadline) {
				if err := m.terminalAccepted(ctx, store, job, jobFailure); err != nil {
					return stop(err)
				}
				continue
			}
			if !selected {
				if err := m.originalTiming(&jobFailure); err != nil {
					return stop(err)
				}
				job.AcceptedAt, job.Deadline = jobFailure.AcceptedAt, jobFailure.Deadline
				if !time.Now().Before(job.Deadline) {
					jobFailure.Attempts = maxJobAttempts
					store.Data.JobFailures[job.ID] = jobFailure
					if err := store.Save(); err != nil {
						return stop(err)
					}
					if err := m.finishFailedJob(ctx, store, job, started, jobFailure); err != nil {
						failures = append(failures, err)
					}
					continue
				}
				// Count an interrupted legacy execution before starting it.
				jobFailure.Attempts++
				store.Data.JobFailures[job.ID] = jobFailure
				if err := store.Save(); err != nil {
					return stop(err)
				}
			}
			if selected {
				executeErr := m.executeSelectedWithControls(ctx, store, job, started, entry.key, items)
				clearResolvedTerminalFailures()
				if err := executeErr; err != nil {
					if errors.Is(err, ErrAcceptedDiscarded) {
						return stop() // The refused assignment retired this worker.
					}
					if errors.Is(err, ErrAcceptanceState) || store.Data.Accepted != nil || ctx.Err() != nil {
						return stop(err)
					}
					failures = append(failures, err)
				}
			} else {
				m.logf("run %s from @%s in %s #%d", cmd.Action, item.Author.Login, repo.Name, number)
				err = m.Handler.Execute(ctx, job)
				jobFailure.ModelCallStarted = store.Data.JobFailures[job.ID].ModelCallStarted
				if err != nil {
					failureErr := fmt.Errorf("%s from %s: %w", cmd.Action, itemLocation(item), err)
					publicationFailure := errors.Is(err, ErrPostModelPublication)
					outcomeUnknown := errors.Is(err, ErrExecutionOutcomeUnknown)
					interrupted := !jobFailure.ModelCallStarted && !outcomeUnknown && ctx.Err() != nil && errors.Is(err, context.Canceled) && !publicationFailure
					switch {
					case interrupted:
						jobFailure.Attempts--
					case jobFailure.ModelCallStarted, outcomeUnknown, errors.Is(err, ErrTerminalJob):
						jobFailure.Attempts = maxJobAttempts
					case !publicationFailure && (errors.Is(err, workroot.ErrUnsafe) || infrastructureFailure(err)):
						jobFailure.Attempts--
					}
					if jobFailure == (state.JobFailure{NotificationKey: entry.key}) {
						delete(store.Data.JobFailures, job.ID)
					} else {
						store.Data.JobFailures[job.ID] = jobFailure
					}
					if saveErr := store.Save(); saveErr != nil {
						return stop(failureErr, saveErr)
					}
					if interrupted {
						m.logf("%s from %s interrupted: %v", cmd.Action, itemLocation(item), err)
						return stop(failureErr)
					}
					m.logf("%s from %s failed (attempt %d/%d): %v", cmd.Action, itemLocation(item), jobFailure.Attempts, maxJobAttempts, err)
					if jobFailure.Attempts >= maxJobAttempts {
						recordTerminalFailure(job.ID, m.finishFailedJob(ctx, store, job, started, jobFailure))
					}
					if ctx.Err() != nil {
						return stop(failureErr)
					}
					if jobFailure.Attempts < maxJobAttempts {
						failures = append(failures, failureErr)
					}
				} else {
					store.Data.Processed[job.ID] = started
					delete(store.Data.JobFailures, job.ID)
					delete(store.Data.PendingFixes, job.ID)
					if err := store.Save(); err != nil {
						return stop(err)
					}
					if ctx.Err() != nil {
						return stop()
					}
				}
			}
			var capacityErr error
			if selected {
				capacityErr = m.waitCapacityWithInvitations(ctx, store)
			} else {
				capacityErr = m.waitCapacity(ctx)
			}
			if err := capacityErr; err != nil {
				for _, later := range scans[scanIndex+1:] {
					rememberNotification(store, later.key, later.notification, later.since)
				}
				if entryFailed() {
					if saveErr := deferEntry(); saveErr != nil {
						return stop(err, saveErr)
					}
				} else {
					rememberNotification(store, entry.key, notification, entry.since)
					if saveErr := store.Save(); saveErr != nil {
						return stop(err, saveErr)
					}
				}
				return stop(err)
			}
		}
		if entryFailed() {
			if err := deferEntry(); err != nil {
				return stop(err)
			}
		} else {
			if _, pending := store.Data.PendingNotifications[entry.key]; selectedMode && pending {
				if err := m.retireMissingTerminalFailures(store, entry.key, items, m.controlCommands(repo), nil, started); err != nil {
					return stop(err)
				}
			}
			if pending, exists := store.Data.PendingNotifications[entry.key]; exists &&
				(!selectedMode || !hasTerminalReplay(store, entry.key, "")) {
				delete(store.Data.PendingNotifications, entry.key)
				if err := store.Save(); err != nil {
					store.Data.PendingNotifications[entry.key] = pending
					return stop(err)
				}
			}
		}
	}
	if ctx.Err() != nil {
		return stop()
	}
	// GitHub timestamps have second precision. Keep a two-second overlap so an
	// update racing with this poll cannot fall on the exclusive since boundary.
	newCursor := started.Add(-2 * time.Second)
	for _, repo := range polled {
		if newCursor.After(store.Data.RepositoryCursors[repo]) {
			store.Data.RepositoryCursors[repo] = newCursor
		}
	}
	if newCursor.After(store.Data.Cursor) {
		store.Data.Cursor = newCursor
	}
	if err := store.Save(); err != nil {
		return stop(err)
	}
	return stop()
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func earlierWake(next, invitation time.Time) time.Time {
	if !invitation.IsZero() && invitation.Before(next) {
		return invitation
	}
	return next
}

func (m *Monitor) checkDueInvitations(ctx context.Context, store *state.Store, at time.Time) error {
	var due bool
	_ = store.WithLock(func() error {
		due = !at.Before(store.Data.NextInviteCheckAt)
		return nil
	})
	if !due {
		return nil
	}
	deferred, err := m.acceptWhitelistedInvitations(ctx)
	if err != nil {
		// Invitation failures should be visible, but should not stop scans of
		// repositories for which this account already has access.
		m.logf("invitation check: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.WithLock(func() error {
		if len(deferred) != 0 {
			if store.Data.RepositoryCursors == nil {
				store.Data.RepositoryCursors = make(map[string]time.Time)
			}
			for _, name := range deferred {
				if _, exists := store.Data.RepositoryCursors[name]; !exists {
					store.Data.RepositoryCursors[name] = store.Data.Cursor
				}
			}
		}
		if checkedAt := m.now().UTC(); checkedAt.After(at) {
			at = checkedAt
		}
		store.Data.NextInviteCheckAt = at.Add(15 * time.Minute)
		return store.Save()
	})
}

// Keep the one capacity wait live while the monitor services invitation deadlines.
func (m *Monitor) waitCapacityWithInvitations(ctx context.Context, store *state.Store) error {
	if err := m.checkDueInvitations(ctx, store, m.now().UTC()); err != nil {
		return err
	}
	if _, ok := m.Handler.(interface{ WaitCapacity(context.Context) error }); !ok {
		return fmt.Errorf("%w: selected worker has no readiness check", ErrAcceptanceState)
	}
	nextTerminal := m.serviceTerminalControls(ctx, store)
	waitCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- m.waitCapacity(waitCtx) }()
	waiting := true
	finish := func(err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.checkDueInvitations(ctx, store, m.now().UTC()); err != nil {
			return err
		}
		if !nextTerminal.IsZero() && !m.now().UTC().Before(nextTerminal) {
			m.serviceTerminalControls(ctx, store)
		}
		return ctx.Err()
	}
	defer func() {
		cancel()
		if waiting {
			<-done
		}
	}()
	for {
		due := earlierWake(store.Data.NextInviteCheckAt, nextTerminal)
		timer := time.NewTimer(due.Sub(m.now().UTC()))
		select {
		case err := <-done:
			timer.Stop()
			waiting = false
			return finish(err)
		case <-timer.C:
			select {
			case err := <-done:
				waiting = false
				return finish(err)
			default:
			}
			at := m.now().UTC()
			if at.Before(due) {
				at = due
			}
			if err := m.checkDueInvitations(ctx, store, at); err != nil {
				return err
			}
			nextTerminal = m.serviceTerminalControls(ctx, store)
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

func (m *Monitor) waitCapacity(ctx context.Context) error {
	if handler, ok := m.Handler.(interface{ WaitCapacity(context.Context) error }); ok {
		if err := handler.WaitCapacity(ctx); err != nil {
			return fmt.Errorf("wait for action capacity: %w", err)
		}
	}
	return ctx.Err()
}

func (m *Monitor) availableCommands(repo config.Repository) map[string]command.Action {
	available := make(map[string]command.Action, len(repo.Actions))
	for _, name := range repo.Actions {
		if m.SelectedCommands != nil && !m.SelectedCommands[name] {
			continue
		}
		mapping, ok := m.Config.Commands[name]
		if !ok {
			continue
		}
		installed, err := m.Config.CommandInstalled(name)
		if err != nil {
			m.logf("disable %s in %s: skill unavailable: %v", name, repo.Name, err)
			continue
		}
		if installed {
			available[name] = command.Action(mapping.Action)
		}
	}
	return available
}

// Control replies use configured commands even when the selected worker cannot execute them.
func (m *Monitor) controlCommands(repo config.Repository) map[string]command.Action {
	commands := make(map[string]command.Action, len(repo.Actions))
	for _, name := range repo.Actions {
		if mapping, ok := m.Config.Commands[name]; ok {
			commands[name] = command.Action(mapping.Action)
		}
	}
	return commands
}

func notificationKey(notification github.Notification) string {
	return jobID(strings.ToLower(notification.Repository.FullName), notification.Subject.Type+"\x00"+notification.Subject.URL)
}

func itemLocation(item github.Item) string {
	if item.HTMLURL != "" {
		return item.HTMLURL
	}
	return item.Key
}

func (m *Monitor) deferNotification(store *state.Store, key string, notification github.Notification, since, now time.Time) error {
	pending := deferNotificationState(store, key, notification, since, now)
	if err := store.Save(); err != nil {
		return err
	}
	m.logf("retry %s %s after %s (failure %d)", pending.Repository, pending.SubjectURL, pending.RetryAt.Format(time.RFC3339), pending.Failures)
	return nil
}

func deferNotificationState(store *state.Store, key string, notification github.Notification, since, now time.Time) state.PendingNotification {
	pending := store.Data.PendingNotifications[key]
	if pending.Failures < 7 {
		pending.Failures++
	}
	pending.Repository = notification.Repository.FullName
	pending.RepositoryID = notification.Repository.ID
	pending.OwnerID = notification.Repository.Owner.ID
	pending.SubjectType = notification.Subject.Type
	pending.SubjectURL = notification.Subject.URL
	pending.Since = since
	if notification.UpdatedAt.After(pending.UpdatedAt) {
		pending.UpdatedAt = notification.UpdatedAt
	}
	pending.RetryAt = now.Add(time.Minute << (pending.Failures - 1))
	store.Data.PendingNotifications[key] = pending
	return pending
}

// rememberNotification retains a thread for replay without counting capacity loss as a failure.
// The caller saves all remembered threads before returning.
func rememberNotification(store *state.Store, key string, notification github.Notification, since time.Time) {
	if _, exists := store.Data.PendingNotifications[key]; exists {
		return
	}
	if store.Data.PendingNotifications == nil {
		store.Data.PendingNotifications = make(map[string]state.PendingNotification)
	}
	store.Data.PendingNotifications[key] = state.PendingNotification{
		Repository: notification.Repository.FullName, RepositoryID: notification.Repository.ID,
		OwnerID: notification.Repository.Owner.ID, SubjectType: notification.Subject.Type,
		SubjectURL: notification.Subject.URL, Since: since, UpdatedAt: notification.UpdatedAt,
	}
}

func hasTerminalReplay(store *state.Store, key, exceptID string) bool {
	for id, failure := range store.Data.JobFailures {
		if id != exceptID && failure.NotificationKey == key && store.Data.Processed[id].IsZero() &&
			(failure.Attempts >= maxJobAttempts || failure.ModelCallStarted || !failure.Deadline.IsZero()) {
			return true
		}
	}
	return false
}

type terminalControl struct {
	entry   notificationScan
	checkAt time.Time
}

// Snapshot linked failures before making GitHub calls; Execute may update the store concurrently.
func terminalControlChecks(store *state.Store, exceptID string) (map[string]terminalControl, string) {
	checks := make(map[string]terminalControl)
	var activeJobID string
	_ = store.WithLock(func() error {
		if store.Data.Accepted != nil {
			activeJobID = store.Data.Accepted.JobID
		}
		for id, failure := range store.Data.JobFailures {
			if id == activeJobID || id == exceptID || failure.NotificationKey == "" || !store.Data.Processed[id].IsZero() ||
				(failure.Attempts < maxJobAttempts && !failure.ModelCallStarted && failure.Deadline.IsZero()) {
				continue
			}
			pending, ok := store.Data.PendingNotifications[failure.NotificationKey]
			if !ok {
				continue
			}
			checkAt := failure.Deadline
			if failure.Attempts >= maxJobAttempts || failure.ModelCallStarted {
				checkAt = time.Time{}
			}
			if failure.ReplyAttempts > 0 && pending.RetryAt.After(checkAt) {
				checkAt = pending.RetryAt
			}
			if previous, exists := checks[failure.NotificationKey]; exists && !checkAt.Before(previous.checkAt) {
				continue
			}
			checks[failure.NotificationKey] = terminalControl{
				entry: notificationScan{notification: github.Notification{
					ID: failure.NotificationKey,
					Repository: github.Repository{ID: pending.RepositoryID, FullName: pending.Repository,
						Owner: github.User{ID: pending.OwnerID}},
					Subject:   github.Subject{Type: pending.SubjectType, URL: pending.SubjectURL},
					UpdatedAt: pending.UpdatedAt,
				}, since: pending.Since, key: failure.NotificationKey},
				checkAt: checkAt,
			}
		}
		return nil
	})
	return checks, activeJobID
}

// Return the next future check without making remote calls, including on error paths.
func (m *Monitor) terminalControlWake(store *state.Store) time.Time {
	checks, _ := terminalControlChecks(store, "")
	var next time.Time
	now := m.now().UTC()
	for _, check := range checks {
		at := check.checkAt
		if !at.After(now) {
			at = now.Add(time.Minute)
		}
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next
}

// Service each due linked thread once on the monitor goroutine, then return its next wake.
func (m *Monitor) serviceTerminalControls(ctx context.Context, store *state.Store) time.Time {
	checks, activeJobID := terminalControlChecks(store, "")
	if len(checks) == 0 || ctx.Err() != nil {
		return m.terminalControlWake(store)
	}
	keys := make([]string, 0, len(checks))
	for key := range checks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	commandSets := make(map[string]map[string]command.Action)
	serviced := make(map[string]time.Time)
	for _, key := range keys {
		if ctx.Err() != nil {
			break
		}
		check := checks[key]
		if check.checkAt.After(m.now().UTC()) {
			continue
		}
		if len(commandSets) == 0 {
			for _, repo := range m.Config.Repositories {
				commandSets[repo.Name] = m.controlCommands(repo)
			}
		}
		serviced[key] = m.processTerminalNotification(ctx, store, activeJobID, check.entry, commandSets, nil)
	}
	after, _ := terminalControlChecks(store, "")
	var next time.Time
	now := m.now().UTC()
	for key, check := range after {
		at := check.checkAt
		if !at.After(now) {
			at = serviced[key]
			if !at.After(now) {
				at = now.Add(time.Minute) // A due but unserviceable key must not spin the timer.
			}
		}
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next
}

// A linked failure was first found in the pending notification's scan window.
// A completed replay of that window can therefore prove its command is gone.
func (m *Monitor) retireMissingTerminalFailures(store *state.Store, key string, items []github.Item,
	commands map[string]command.Action, invalid []string, at time.Time) error {
	present := make(map[string]bool, len(items))
	for _, item := range items {
		parsed, err := command.ParseForLoginCommands(item.Body, m.Config.BotLogin, commands)
		if err != nil {
			if !errors.Is(err, command.ErrNoCommand) {
				// A malformed command may still contain the failed command.
				for name := range commands {
					present[jobID(item.Key, name)] = true
				}
			}
			continue
		}
		present[jobID(item.Key, parsed.Name)] = true
	}
	for _, id := range invalid {
		delete(present, id)
	}
	return store.WithLock(func() error {
		retired := make(map[string]state.JobFailure)
		for id, failure := range store.Data.JobFailures {
			if failure.NotificationKey != key || !store.Data.Processed[id].IsZero() || present[id] ||
				store.Data.Accepted != nil && store.Data.Accepted.JobID == id ||
				(failure.Attempts < maxJobAttempts && !failure.ModelCallStarted &&
					(failure.Deadline.IsZero() || time.Now().Before(failure.Deadline))) {
				continue
			}
			retired[id] = failure
			store.Data.Processed[id] = at
			delete(store.Data.JobFailures, id)
		}
		if len(retired) == 0 {
			return nil
		}
		if err := store.Save(); err != nil {
			for id, failure := range retired {
				store.Data.JobFailures[id] = failure
				delete(store.Data.Processed, id)
			}
			return err
		}
		return nil
	})
}

func infrastructureFailure(err error) bool {
	var status *github.StatusError
	if errors.As(err, &status) && (status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden ||
		status.StatusCode == http.StatusRequestTimeout || status.StatusCode == http.StatusTooManyRequests || status.StatusCode >= http.StatusInternalServerError) {
		return true
	}
	var network *url.Error
	return errors.Is(err, ErrRequestTransport) || errors.As(err, &network)
}

func githubResourceMovedOrGone(err error) bool {
	var status *github.StatusError
	return errors.As(err, &status) && (status.StatusCode == http.StatusMovedPermanently || status.StatusCode == http.StatusGone)
}

func (m *Monitor) actionRuntime() time.Duration {
	runtime := time.Duration(m.Config.MaxRuntimeMinutes) * time.Minute
	if runtime <= 0 {
		return config.DefaultMaxRuntime
	}
	return runtime
}

func (m *Monitor) pollInterval() time.Duration {
	interval := time.Duration(m.Config.PollIntervalSeconds) * time.Second
	if interval < time.Minute {
		return time.Minute
	}
	return interval
}

func (m *Monitor) scheduleAcceptedRetry(store *state.Store) (time.Time, error) {
	accepted := store.Data.Accepted
	now := time.Now().UTC()
	retryAt := now.Add(m.pollInterval())
	if now.Before(accepted.Deadline) && accepted.Deadline.Before(retryAt) {
		retryAt = accepted.Deadline
	}
	accepted.RetryAt = retryAt
	if err := saveAcceptanceState(store); err != nil {
		return retryAt, err
	}
	m.logf("accepted %s #%d recovery deferred until %s; new action discovery remains paused", accepted.Repository, accepted.SubjectNumber, retryAt.Format(time.RFC3339))
	return retryAt, nil
}

func (m *Monitor) originalTiming(failure *state.JobFailure) error {
	if err := failure.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrAcceptanceState, err)
	}
	if failure.AcceptedAt.IsZero() {
		failure.AcceptedAt = time.Now().UTC()
		failure.Deadline = failure.AcceptedAt.Add(m.actionRuntime())
	}
	return nil
}

func saveAcceptanceState(store *state.Store) error {
	if err := store.Save(); err != nil {
		return fmt.Errorf("%w: %w", ErrAcceptanceState, err)
	}
	return nil
}

func saveJobResult(store *state.Store, selected bool) error {
	if selected {
		return saveAcceptanceState(store)
	}
	return store.Save()
}

func jobFromAccepted(accepted state.AcceptedAction) Job {
	return Job{
		Repository:  config.Repository{Name: accepted.Repository, ID: accepted.RepositoryID, OwnerID: accepted.OwnerID},
		SubjectType: accepted.SubjectType, Number: accepted.SubjectNumber,
		Source: github.Item{Key: accepted.SourceKey, NodeID: accepted.SourceNodeID, HTMLURL: accepted.SourceURL, UpdatedAt: accepted.SourceUpdatedAt,
			Author: github.User{Login: accepted.Requester, ID: accepted.RequesterID}},
		Command: command.Command{Name: accepted.CommandName, Action: command.Action(accepted.Action)},
		ID:      accepted.JobID, AttemptID: accepted.AttemptID, AcceptedAt: accepted.AcceptedAt, Deadline: accepted.Deadline,
	}
}

func (m *Monitor) terminalAccepted(ctx context.Context, store *state.Store, job Job, failure state.JobFailure) error {
	if !store.Data.Accepted.Terminal || failure.Attempts < maxJobAttempts {
		store.Data.Accepted.Terminal = true
		failure.Attempts = maxJobAttempts
		store.Data.JobFailures[job.ID] = failure
		if err := saveAcceptanceState(store); err != nil {
			return err
		}
	}
	return m.finishFailedJob(ctx, store, job, time.Now().UTC(), failure)
}

func discardAccepted(store *state.Store) error {
	accepted := store.Data.Accepted
	if accepted == nil {
		return fmt.Errorf("%w: cannot discard a missing accepted action", ErrAcceptanceState)
	}
	processed, hadProcessed := store.Data.Processed[accepted.JobID]
	failure, hadFailure := store.Data.JobFailures[accepted.JobID]
	store.Data.Processed[accepted.JobID] = time.Now().UTC()
	delete(store.Data.JobFailures, accepted.JobID)
	store.Data.Accepted = nil
	if err := saveAcceptanceState(store); err != nil {
		if hadProcessed {
			store.Data.Processed[accepted.JobID] = processed
		} else {
			delete(store.Data.Processed, accepted.JobID)
		}
		if hadFailure {
			store.Data.JobFailures[accepted.JobID] = failure
		} else {
			delete(store.Data.JobFailures, accepted.JobID)
		}
		store.Data.Accepted = accepted
		return err
	}
	if err := store.ClearAcceptedResult(); err != nil {
		return fmt.Errorf("%w: clear discarded result: %w", ErrAcceptanceState, err)
	}
	return nil
}

func (m *Monitor) acceptedRepository(accepted state.AcceptedAction) (config.Repository, bool) {
	repo, ok := m.Config.Repository(accepted.Repository)
	if !ok || !repo.MatchesIdentity(accepted.RepositoryID, accepted.OwnerID) ||
		!repo.AllowsAction(accepted.CommandName) || !repo.AllowsActor(accepted.Requester, accepted.RequesterID) {
		return config.Repository{}, false
	}
	mapping, ok := m.Config.Commands[accepted.CommandName]
	return repo, ok && mapping.Action == accepted.Action
}

func (m *Monitor) acceptedIssuePresent(ctx context.Context, accepted state.AcceptedAction) (bool, error) {
	if accepted.SubjectType != "Issue" {
		return true, nil
	}
	issue, err := m.GitHub.IssueDetails(ctx, accepted.Repository, accepted.SubjectNumber)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	if githubResourceMovedOrGone(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("verify accepted issue thread: %w", err)
	}
	return issue.Number == accepted.SubjectNumber, nil
}

func (m *Monitor) validateAcceptedSource(ctx context.Context, accepted state.AcceptedAction, requireInstalled bool) (config.Repository, github.Item, bool, error) {
	repo, ok := m.acceptedRepository(accepted)
	if !ok {
		return config.Repository{}, github.Item{}, false, nil
	}
	var commands map[string]command.Action
	if requireInstalled {
		commands = m.availableCommands(repo)
		if commands[accepted.CommandName] != command.Action(accepted.Action) {
			return config.Repository{}, github.Item{}, false, fmt.Errorf("selected command %q is unavailable", accepted.CommandName)
		}
	} else {
		commands = m.controlCommands(repo)
	}
	liveRepo, err := m.GitHub.Repository(ctx, repo.Name)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return config.Repository{}, github.Item{}, false, ctxErr
	}
	if githubResourceMovedOrGone(err) {
		return config.Repository{}, github.Item{}, false, nil
	}
	if err != nil {
		return config.Repository{}, github.Item{}, false, fmt.Errorf("verify accepted repository identity: %w", err)
	}
	if !repo.MatchesIdentity(liveRepo.ID, liveRepo.Owner.ID) || liveRepo.Archived || liveRepo.DefaultBranch == "" {
		return config.Repository{}, github.Item{}, false, nil
	}
	if present, err := m.acceptedIssuePresent(ctx, accepted); err != nil || !present {
		return config.Repository{}, github.Item{}, false, err
	}
	current, err := m.GitHub.CurrentItem(ctx, jobFromAccepted(accepted).Source)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return config.Repository{}, github.Item{}, false, ctxErr
		}
		// An unreadable node may be a transient GraphQL failure. Confirm its
		// absence in the complete live thread before releasing the reservation.
		items, listErr := m.GitHub.ThreadItems(ctx, repo.Name, accepted.SubjectType, accepted.SubjectNumber, time.Time{})
		if ctxErr := ctx.Err(); ctxErr != nil {
			return config.Repository{}, github.Item{}, false, ctxErr
		}
		if listErr != nil {
			if githubResourceMovedOrGone(listErr) {
				return config.Repository{}, github.Item{}, false, nil
			}
			return config.Repository{}, github.Item{}, false, errors.Join(fmt.Errorf("verify accepted source: %w", err), fmt.Errorf("list accepted source thread: %w", listErr))
		}
		for _, item := range items {
			if item.Key == accepted.SourceKey && item.NodeID == accepted.SourceNodeID {
				if item.Author.ID != accepted.RequesterID || !strings.EqualFold(item.Author.Login, accepted.Requester) {
					return config.Repository{}, github.Item{}, false, nil
				}
				return config.Repository{}, github.Item{}, false, fmt.Errorf("verify accepted source: %w", err)
			}
		}
		return config.Repository{}, github.Item{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		return config.Repository{}, github.Item{}, false, err
	}
	if current.Key != accepted.SourceKey || current.NodeID != accepted.SourceNodeID || current.Edited ||
		current.Author.ID != accepted.RequesterID || !strings.EqualFold(current.Author.Login, accepted.Requester) {
		return config.Repository{}, github.Item{}, false, nil
	}
	parsed, err := command.ParseForLoginCommands(current.Body, m.Config.BotLogin, commands)
	if err != nil || parsed.Name != accepted.CommandName || parsed.Action != command.Action(accepted.Action) {
		return config.Repository{}, github.Item{}, false, nil
	}
	permission, err := m.GitHub.Permission(ctx, repo.Name, accepted.Requester)
	if err != nil {
		return config.Repository{}, github.Item{}, false, fmt.Errorf("verify accepted requester permission: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return config.Repository{}, github.Item{}, false, err
	}
	if permission.User.ID != accepted.RequesterID || !strings.EqualFold(permission.User.Login, accepted.Requester) ||
		(permission.Permission != "write" && permission.Permission != "maintain" && permission.Permission != "admin") {
		return config.Repository{}, github.Item{}, false, nil
	}
	return repo, current, true, nil
}

func assignmentMatchesAccepted(job Job, accepted state.AcceptedAction) bool {
	return job.ID == accepted.JobID && job.AttemptID == accepted.AttemptID &&
		job.Repository.Name == accepted.Repository && job.Repository.ID == accepted.RepositoryID &&
		job.Repository.OwnerID == accepted.OwnerID && job.SubjectType == accepted.SubjectType &&
		job.Number == accepted.SubjectNumber && job.Source.Key == accepted.SourceKey &&
		job.Source.NodeID == accepted.SourceNodeID && job.Source.HTMLURL == accepted.SourceURL &&
		job.Source.UpdatedAt.Equal(accepted.SourceUpdatedAt) &&
		strings.EqualFold(job.Source.Author.Login, accepted.Requester) && job.Source.Author.ID == accepted.RequesterID &&
		job.Command.Name == accepted.CommandName && string(job.Command.Action) == accepted.Action &&
		job.AcceptedAt.Equal(accepted.AcceptedAt) && job.Deadline.Equal(accepted.Deadline)
}

// RevalidateAssignment checks a prepared action against its saved reservation and live GitHub state.
// The caller must finish this check before saving Assigned or sending the worker any assignment bytes.
func (m *Monitor) RevalidateAssignment(ctx context.Context, store *state.Store, job Job) error {
	if m == nil || store == nil {
		return fmt.Errorf("%w: missing accepted action before assignment", ErrAcceptanceState)
	}
	var accepted state.AcceptedAction
	var failure state.JobFailure
	if err := store.WithLock(func() error {
		if store.Data.Accepted == nil {
			return fmt.Errorf("%w: missing accepted action before assignment", ErrAcceptanceState)
		}
		accepted = *store.Data.Accepted
		var ok bool
		failure, ok = store.Data.JobFailures[accepted.JobID]
		if err := accepted.Validate(); err != nil || !assignmentMatchesAccepted(job, accepted) ||
			m.SelectedExecutorID != accepted.ExecutorID || accepted.Terminal ||
			!ok || failure.Attempts <= 0 || failure.ModelCallStarted ||
			!failure.AcceptedAt.Equal(accepted.AcceptedAt) || !failure.Deadline.Equal(accepted.Deadline) {
			return fmt.Errorf("%w: accepted action changed before assignment", ErrAcceptanceState)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		if time.Now().Before(accepted.Deadline) {
			return fmt.Errorf("%w: %w", ErrAcceptedRetry, err)
		}
		return context.DeadlineExceeded
	}
	if !time.Now().Before(accepted.Deadline) {
		return context.DeadlineExceeded
	}
	_, _, valid, err := m.validateAcceptedSource(ctx, accepted, true)
	return store.WithLock(func() error {
		if store.Data.Accepted == nil || *store.Data.Accepted != accepted || store.Data.JobFailures[accepted.JobID] != failure {
			return fmt.Errorf("%w: accepted action changed during assignment validation", ErrAcceptanceState)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if time.Now().Before(accepted.Deadline) {
				return fmt.Errorf("%w: %w", ErrAcceptedRetry, ctxErr)
			}
			return context.DeadlineExceeded
		}
		if !time.Now().Before(accepted.Deadline) {
			return context.DeadlineExceeded
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrAcceptedRetry, err)
		}
		if !valid {
			m.logf("discard accepted %s: source or requester is no longer valid before assignment", itemLocation(job.Source))
			if err := discardAccepted(store); err != nil {
				return err
			}
			return ErrAcceptedDiscarded
		}
		return nil
	})
}

func (m *Monitor) recoverAccepted(ctx context.Context, store *state.Store) error {
	accepted := store.Data.Accepted
	if err := accepted.Validate(); err != nil {
		return fmt.Errorf("%w: invalid accepted action: %v", ErrAcceptanceState, err)
	}
	job := jobFromAccepted(*accepted)
	failure, exists := store.Data.JobFailures[job.ID]
	if !exists || failure.Attempts <= 0 {
		return fmt.Errorf("%w: accepted action has no reserved attempt", ErrAcceptanceState)
	}
	if failure.AcceptedAt.IsZero() {
		failure.AcceptedAt, failure.Deadline = accepted.AcceptedAt, accepted.Deadline
		store.Data.JobFailures[job.ID] = failure
		if err := saveAcceptanceState(store); err != nil {
			return err
		}
	} else if !failure.AcceptedAt.Equal(accepted.AcceptedAt) || !failure.Deadline.Equal(accepted.Deadline) {
		return fmt.Errorf("%w: accepted action deadline differs from its first acceptance", ErrAcceptanceState)
	}
	stored, err := store.HasAcceptedResult()
	if err != nil {
		return fmt.Errorf("%w: inspect stored result: %w", ErrAcceptanceState, err)
	}
	if stored {
		return m.recoverStoredResult(ctx, store, job, *accepted)
	}
	if accepted.Assigned || accepted.Terminal || failure.ModelCallStarted || !time.Now().Before(job.Deadline) {
		return m.terminalAccepted(ctx, store, job, failure)
	}
	if m.SelectedExecutorID == "" || m.SelectedExecutorID != accepted.ExecutorID {
		return fmt.Errorf("%w: selected executor changed before recovery", ErrAcceptanceState)
	}
	resumeCtx, cancel := context.WithDeadline(ctx, job.Deadline)
	defer cancel()
	failed := func(location string, err error) error {
		if ctx.Err() == nil && !time.Now().Before(job.Deadline) {
			return m.terminalAccepted(ctx, store, job, failure)
		}
		return fmt.Errorf("%s: %w", location, err)
	}
	checkContext := func(location string) error {
		if err := resumeCtx.Err(); err != nil {
			return failed(location, err)
		}
		return nil
	}
	if err := m.waitCapacityWithInvitations(resumeCtx, store); err != nil {
		return failed("resume accepted action capacity", err)
	}
	if err := checkContext("resume accepted action capacity"); err != nil {
		return err
	}
	repo, current, valid, err := m.validateAcceptedSource(resumeCtx, *accepted, true)
	if err != nil {
		return failed("revalidate accepted source", err)
	}
	if err := checkContext("revalidate accepted source"); err != nil {
		return err
	}
	if !valid {
		m.logf("discard accepted %s: source or requester is no longer valid", itemLocation(job.Source))
		return discardAccepted(store)
	}
	job.Repository, job.Source = repo, current
	if err := m.GitHub.ReactEyes(resumeCtx, accepted.SourceNodeID); err != nil {
		return failed("react to accepted source", err)
	}
	if err := checkContext("react to accepted source"); err != nil {
		return err
	}
	if !time.Now().Before(job.Deadline) {
		return m.terminalAccepted(ctx, store, job, failure)
	}
	return m.executeSelected(resumeCtx, store, job, time.Now().UTC())
}

func (m *Monitor) recoverStoredResult(ctx context.Context, store *state.Store, job Job, accepted state.AcceptedAction) error {
	publisher, ok := m.Handler.(StoredResultPublisher)
	if !ok {
		return fmt.Errorf("%w: stored result publisher is unavailable", ErrAcceptanceState)
	}
	repo, current, valid, err := m.validateAcceptedSource(ctx, accepted, false)
	if err != nil {
		return fmt.Errorf("revalidate saved result source: %w", err)
	}
	if !valid {
		m.logf("discard accepted %s: source or requester is no longer valid before publication", itemLocation(job.Source))
		if err := discardAccepted(store); err != nil {
			return err
		}
		return ErrAcceptedDiscarded
	}
	job.Repository, job.Source = repo, current
	job.Source.HTMLURL = accepted.SourceURL
	job.Source.UpdatedAt = accepted.SourceUpdatedAt // Publication markers use the original command timestamp.
	if err := publisher.PublishRecovered(ctx, job); err != nil {
		if errors.Is(err, ErrAcceptanceState) {
			return err
		}
		return fmt.Errorf("%w: publish stored result: %w", ErrPostModelPublication, err)
	}
	return m.completeSelected(ctx, store, job, m.now().UTC(), nil)
}

func (m *Monitor) executeSelected(ctx context.Context, store *state.Store, job Job, at time.Time) error {
	return m.executeSelectedWithControls(ctx, store, job, at, "", nil)
}

// Keep control work on the monitor goroutine while the selected worker executes.
// No other action is discovered or assigned until this execution has finished.
func (m *Monitor) executeSelectedWithControls(ctx context.Context, store *state.Store, job Job, at time.Time,
	cachedKey string, cachedItems []github.Item) error {
	checks, _ := terminalControlChecks(store, job.ID)
	terminalChecks := make(map[string]time.Time, len(checks))
	for key, check := range checks {
		terminalChecks[key] = check.checkAt
	}
	commandSets := make(map[string]map[string]command.Action)
	if len(terminalChecks) != 0 {
		for _, configured := range m.Config.Repositories {
			commandSets[configured.Name] = m.controlCommands(configured)
		}
	}
	actionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	m.logf("run %s from @%s in %s #%d", job.Command.Action, job.Source.Author.Login, job.Repository.Name, job.Number)
	go func() {
		result := error(ErrExecutionOutcomeUnknown)
		defer func() { done <- result }()
		result = m.Handler.Execute(actionCtx, job)
	}()
	finish := func(err error) error { return m.completeSelected(ctx, store, job, at, err) }
	var result error
	completed := false
	readDone := func() {
		if !completed {
			select {
			case result = <-done:
				completed = true
			default:
			}
		}
	}
	abort := func(err error) error {
		readDone()
		if completed {
			if ctx.Err() != nil {
				return finish(result)
			}
			return errors.Join(finish(result), fmt.Errorf("%w: control state: %w", ErrAcceptanceState, err))
		}
		cancel()
		result = <-done // The state lock and store must outlive Execute.
		if ctx.Err() != nil {
			return finish(result)
		}
		return fmt.Errorf("%w: control state: %w", ErrAcceptanceState, err)
	}
	nextCheck := make(map[string]time.Time)
	for {
		readDone()
		completedAtStart := completed
		if err := m.checkDueInvitations(ctx, store, m.now().UTC()); err != nil {
			return abort(err)
		}
		var next time.Time
		var due []notificationScan
		_ = store.WithLock(func() error {
			next = store.Data.NextInviteCheckAt
			if len(terminalChecks) == 0 {
				return nil
			}
			for key, pending := range store.Data.PendingNotifications {
				checkAt, relevant := terminalChecks[key]
				if !relevant {
					continue
				}
				if previous, checked := nextCheck[key]; checked {
					if previous.IsZero() {
						continue
					}
					if previous.After(checkAt) {
						checkAt = previous
					}
				}
				if checkAt.After(m.now().UTC()) {
					if next.IsZero() || checkAt.Before(next) {
						next = checkAt
					}
					continue
				}
				due = append(due, notificationScan{notification: github.Notification{
					ID: key, Repository: github.Repository{ID: pending.RepositoryID, FullName: pending.Repository,
						Owner: github.User{ID: pending.OwnerID}}, Subject: github.Subject{Type: pending.SubjectType, URL: pending.SubjectURL},
					UpdatedAt: pending.UpdatedAt,
				}, since: pending.Since, key: key})
			}
			return nil
		})
		for _, entry := range due {
			var items []github.Item
			if entry.key == cachedKey {
				items = cachedItems
				cachedKey = "" // Retries read the current thread again.
			}
			checkAt := m.processTerminalNotification(ctx, store, job.ID, entry, commandSets, items)
			nextCheck[entry.key] = checkAt
			_ = store.WithLock(func() error {
				if !hasTerminalReplay(store, entry.key, job.ID) {
					delete(terminalChecks, entry.key)
				}
				return nil
			})
			if !checkAt.IsZero() && (next.IsZero() || checkAt.Before(next)) {
				next = checkAt
			}
			if err := m.checkDueInvitations(ctx, store, m.now().UTC()); err != nil {
				return abort(err)
			}
		}
		if err := ctx.Err(); err != nil {
			if !completed {
				result = <-done
			}
			return finish(result)
		}
		readDone()
		if completed {
			if completedAtStart && len(due) == 0 {
				return finish(result)
			}
			continue
		}
		if next.IsZero() {
			next = m.now().UTC().Add(15 * time.Minute)
		}
		timer := time.NewTimer(next.Sub(m.now().UTC()))
		select {
		case result = <-done:
			timer.Stop()
			completed = true
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return abort(ctx.Err())
		}
	}
}

func (m *Monitor) processTerminalNotification(ctx context.Context, store *state.Store, activeJobID string,
	entry notificationScan, commandSets map[string]map[string]command.Action, cachedItems []github.Item) time.Time {
	notification := entry.notification
	repo, ok := m.Config.Repository(notification.Repository.FullName)
	if !ok || !repo.MatchesIdentity(notification.Repository.ID, notification.Repository.Owner.ID) ||
		(notification.Subject.Type != "Issue" && notification.Subject.Type != "PullRequest") {
		return time.Time{}
	}
	number, err := subjectNumber(notification.Subject.URL, repo.Name, notification.Subject.Type)
	if err != nil {
		return time.Time{}
	}
	live, err := m.GitHub.Repository(ctx, repo.Name)
	if err != nil {
		m.logf("check terminal controls in %s #%d: %v", repo.Name, number, err)
		return m.now().UTC().Add(time.Minute)
	}
	if !repo.MatchesIdentity(live.ID, live.Owner.ID) {
		return time.Time{}
	}
	items := cachedItems
	if items == nil {
		items, err = m.GitHub.ThreadItems(ctx, repo.Name, notification.Subject.Type, number, entry.since)
		if err != nil {
			m.logf("check terminal controls in %s #%d: %v", repo.Name, number, err)
			return m.now().UTC().Add(time.Minute)
		}
	}
	return m.processTerminalItems(ctx, store, activeJobID, entry, repo, notification.Subject.Type, number, items, commandSets[repo.Name])
}

func (m *Monitor) processTerminalItems(ctx context.Context, store *state.Store, activeJobID string, entry notificationScan,
	repo config.Repository, subjectType string, number int, items []github.Item, commands map[string]command.Action) time.Time {
	var next time.Time
	var invalid []string
	uncertain := false
	passStarted := m.now().UTC()
	var replyRetryAt time.Time
	_ = store.WithLock(func() error {
		replyRetryAt = store.Data.PendingNotifications[entry.key].RetryAt
		return nil
	})
	wake := func(at time.Time) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	linkedFailure := func(item github.Item) bool {
		var linked bool
		_ = store.WithLock(func() error {
			for name := range commands {
				if failure, ok := store.Data.JobFailures[jobID(item.Key, name)]; ok && failure.NotificationKey == entry.key {
					linked = true
					break
				}
			}
			return nil
		})
		return linked
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return next
		}
		if item.Author.Login == "" {
			if linkedFailure(item) {
				uncertain = true
				wake(m.now().UTC().Add(time.Minute))
			}
			continue
		}
		if strings.EqualFold(item.Author.Login, m.Config.BotLogin) {
			continue
		}
		parsed, err := command.ParseForLoginCommands(item.Body, m.Config.BotLogin, commands)
		if err != nil {
			if !errors.Is(err, command.ErrNoCommand) && linkedFailure(item) {
				current, currentErr := m.GitHub.CurrentItem(ctx, item)
				if currentErr == nil && current.Key == item.Key && item.NodeID != "" &&
					current.NodeID == item.NodeID && item.Author.ID > 0 &&
					current.Author.ID == item.Author.ID && strings.EqualFold(current.Author.Login, item.Author.Login) && current.Edited {
					for name := range commands {
						invalid = append(invalid, jobID(item.Key, name))
					}
				} else {
					uncertain = true
					wake(m.now().UTC().Add(time.Minute))
				}
			}
			continue
		}
		id := jobID(item.Key, parsed.Name)
		if id == activeJobID {
			continue
		}
		var terminal bool
		var failure state.JobFailure
		var processed bool
		_ = store.WithLock(func() error {
			_, processed = store.Data.Processed[id]
			failure = store.Data.JobFailures[id]
			terminal = !processed && (failure.Attempts >= maxJobAttempts || failure.ModelCallStarted ||
				!failure.Deadline.IsZero() && !time.Now().Before(failure.Deadline))
			return nil
		})
		if !terminal {
			if !processed && !failure.Deadline.IsZero() {
				wake(failure.Deadline)
			}
			continue
		}
		if failure.ReplyAttempts > 0 && passStarted.Before(replyRetryAt) {
			wake(replyRetryAt)
			continue
		}
		current, err := m.GitHub.CurrentItem(ctx, item)
		if err != nil {
			uncertain = true
			wake(m.now().UTC().Add(time.Minute))
			continue
		}
		if item.Key == "" || current.Key != item.Key || item.NodeID == "" || current.NodeID != item.NodeID ||
			item.Author.ID <= 0 || current.Author.ID != item.Author.ID || current.Author.Login == "" ||
			!strings.EqualFold(current.Author.Login, item.Author.Login) {
			uncertain = true
			wake(m.now().UTC().Add(time.Minute))
			continue
		}
		if current.Edited {
			invalid = append(invalid, id)
			continue
		}
		parsed, err = command.ParseForLoginCommands(current.Body, m.Config.BotLogin, commands)
		if err != nil {
			if errors.Is(err, command.ErrNoCommand) {
				invalid = append(invalid, id)
			} else {
				uncertain = true
				wake(m.now().UTC().Add(time.Minute))
			}
			continue
		}
		if jobID(current.Key, parsed.Name) != id || !repo.AllowsAction(parsed.Name) ||
			!repo.AllowsActor(current.Author.Login, current.Author.ID) {
			invalid = append(invalid, id)
			continue
		}
		permission, err := m.GitHub.Permission(ctx, repo.Name, current.Author.Login)
		if err != nil {
			uncertain = true
			wake(m.now().UTC().Add(time.Minute))
			continue
		}
		if permission.User.ID != current.Author.ID || !strings.EqualFold(permission.User.Login, current.Author.Login) {
			uncertain = true
			wake(m.now().UTC().Add(time.Minute))
			continue
		}
		if permission.Permission != "write" && permission.Permission != "admin" && permission.Permission != "maintain" {
			invalid = append(invalid, id)
			continue
		}
		job := Job{Repository: repo, SubjectType: subjectType, Number: number, Source: current, Command: parsed, ID: id}
		if failure.ModelCallStarted || !failure.Deadline.IsZero() && !time.Now().Before(failure.Deadline) {
			failure.Attempts = maxJobAttempts
		}
		failure.NotificationKey = entry.key
		if err := m.finishFailedJobWithRetry(ctx, store, job, m.now().UTC(), failure, &entry); err != nil {
			m.logf("terminal control in %s: %v", itemLocation(current), err)
			var retryAt time.Time
			_ = store.WithLock(func() error {
				if store.Data.JobFailures[id].ReplyAttempts > failure.ReplyAttempts {
					retryAt = store.Data.PendingNotifications[entry.key].RetryAt
				}
				return nil
			})
			if !retryAt.IsZero() {
				wake(retryAt)
				continue
			}
			wake(m.now().UTC().Add(time.Minute))
		}
	}
	if ctx.Err() == nil && !uncertain {
		if err := m.retireMissingTerminalFailures(store, entry.key, items, commands, invalid, m.now().UTC()); err != nil {
			m.logf("retire terminal controls in %s #%d: %v", repo.Name, number, err)
			wake(m.now().UTC().Add(time.Minute))
		}
	}
	return next
}

func (m *Monitor) completeSelected(ctx context.Context, store *state.Store, job Job, at time.Time, err error) error {
	if errors.Is(err, ErrAcceptanceState) {
		return err
	}
	if errors.Is(err, ErrAcceptedDiscarded) {
		if store.Data.Accepted != nil || store.Data.Processed[job.ID].IsZero() {
			return fmt.Errorf("%w: invalid accepted action was not durably resolved", ErrAcceptanceState)
		}
		return ErrAcceptedDiscarded
	}
	accepted := store.Data.Accepted
	if accepted == nil || accepted.JobID != job.ID || accepted.AttemptID != job.AttemptID {
		return fmt.Errorf("%w: selected action reservation changed during execution", ErrAcceptanceState)
	}
	failure, exists := store.Data.JobFailures[job.ID]
	if !exists || failure.Attempts <= 0 || !failure.AcceptedAt.Equal(job.AcceptedAt) || !failure.Deadline.Equal(job.Deadline) {
		return fmt.Errorf("%w: selected action attempt or deadline changed during execution", ErrAcceptanceState)
	}
	if errors.Is(err, ErrAcceptedRetry) {
		if accepted.Assigned || failure.ModelCallStarted {
			return fmt.Errorf("%w: deferred assignment was already delivered", ErrAcceptanceState)
		}
		return err // RunOnce preserves this attempt for recovery with its original deadline.
	}
	if err == nil {
		previousProcessed, hadProcessed := store.Data.Processed[job.ID]
		previousFix, hadFix := store.Data.PendingFixes[job.ID]
		store.Data.Processed[job.ID] = at
		delete(store.Data.JobFailures, job.ID)
		delete(store.Data.PendingFixes, job.ID)
		store.Data.Accepted = nil
		if err := saveAcceptanceState(store); err != nil {
			if hadProcessed {
				store.Data.Processed[job.ID] = previousProcessed
			} else {
				delete(store.Data.Processed, job.ID)
			}
			store.Data.JobFailures[job.ID] = failure
			if hadFix {
				store.Data.PendingFixes[job.ID] = previousFix
			}
			store.Data.Accepted = accepted
			return err
		}
		if err := store.ClearAcceptedResult(); err != nil {
			return fmt.Errorf("%w: clear completed result: %w", ErrAcceptanceState, err)
		}
		return ctx.Err()
	}
	stored, receiptErr := store.HasAcceptedResult()
	if receiptErr != nil {
		return fmt.Errorf("%w: inspect stored result after execution: %w", ErrAcceptanceState, receiptErr)
	}
	if stored {
		// The worker finished. Only publication may be retried from its durable receipt.
		return fmt.Errorf("%w: %w", ErrPostModelPublication, err)
	}
	failureErr := fmt.Errorf("%s from %s: %w", job.Command.Action, itemLocation(job.Source), err)
	publicationFailure := errors.Is(err, ErrPostModelPublication)
	outcomeUnknown := errors.Is(err, ErrExecutionOutcomeUnknown)
	if !accepted.Assigned && !failure.ModelCallStarted && !outcomeUnknown && !publicationFailure &&
		!errors.Is(err, ErrTerminalJob) && time.Now().Before(job.Deadline) {
		installed, _ := m.Config.CommandInstalled(job.Command.Name)
		if !installed {
			return failureErr // The reserved attempt resumes when its required skill returns.
		}
	}
	interrupted := !accepted.Assigned && !failure.ModelCallStarted && !outcomeUnknown && ctx.Err() != nil && errors.Is(err, context.Canceled) && !publicationFailure
	switch {
	case interrupted:
		failure.Attempts--
	case accepted.Assigned, failure.ModelCallStarted, outcomeUnknown, errors.Is(err, ErrTerminalJob), !time.Now().Before(job.Deadline):
		failure.Attempts = maxJobAttempts
	case !publicationFailure && (errors.Is(err, workroot.ErrUnsafe) || infrastructureFailure(err)):
		failure.Attempts--
	}
	if failure.Attempts < 0 {
		return fmt.Errorf("%w: selected action attempt was over-refunded", ErrAcceptanceState)
	}
	if failure.Attempts >= maxJobAttempts {
		accepted.Terminal = true
	}
	store.Data.JobFailures[job.ID] = failure // Keep the first deadline after a proven unused attempt.
	if failure.Attempts < maxJobAttempts && !accepted.Assigned {
		store.Data.Accepted = nil
	}
	if saveErr := saveAcceptanceState(store); saveErr != nil {
		return errors.Join(failureErr, saveErr)
	}
	if interrupted {
		m.logf("%s from %s interrupted: %v", job.Command.Action, itemLocation(job.Source), err)
		return failureErr
	}
	m.logf("%s from %s failed (attempt %d/%d): %v", job.Command.Action, itemLocation(job.Source), failure.Attempts, maxJobAttempts, err)
	if failure.Attempts >= maxJobAttempts {
		return m.finishFailedJob(ctx, store, job, at, failure)
	}
	return failureErr
}

func (m *Monitor) finishFailedJob(ctx context.Context, store *state.Store, job Job, at time.Time, failure state.JobFailure) error {
	return m.finishFailedJobWithRetry(ctx, store, job, at, failure, nil)
}

func (m *Monitor) finishFailedJobWithRetry(ctx context.Context, store *state.Store, job Job, at time.Time,
	failure state.JobFailure, replyRetry *notificationScan) error {
	var accepted *state.AcceptedAction
	_ = store.WithLock(func() error {
		if current := store.Data.Accepted; current != nil && current.JobID == job.ID {
			copy := *current
			accepted = &copy
		}
		return nil
	})
	if accepted != nil {
		if _, ok := m.acceptedRepository(*accepted); !ok {
			m.logf("discard accepted %s: repository, command, or requester is no longer eligible", itemLocation(job.Source))
			return store.WithLock(func() error { return discardAccepted(store) })
		}
	}
	marker := job.Marker()
	if accepted != nil {
		issueCtx, cancelIssue := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		present, err := m.acceptedIssuePresent(issueCtx, *accepted)
		cancelIssue()
		if err != nil {
			return err
		}
		if !present {
			m.logf("discard accepted %s: source thread moved or was deleted", itemLocation(job.Source))
			return store.WithLock(func() error { return discardAccepted(store) })
		}
	}
	lookupCtx, cancelLookup := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelLookup()
	includeReviews := job.SubjectType == "PullRequest" && job.Command.Action == command.Review
	replied, err := m.GitHub.HasVerifiedBotReply(lookupCtx, job.Repository.Name, job.Number, job.Source.UpdatedAt, m.Config.BotLogin, marker, includeReviews)
	if err != nil {
		if accepted != nil && githubResourceMovedOrGone(err) {
			m.logf("discard accepted %s: source thread moved or was deleted", itemLocation(job.Source))
			return store.WithLock(func() error { return discardAccepted(store) })
		}
		return fmt.Errorf("check existing bot replies to %s: %w", itemLocation(job.Source), err)
	}
	if !replied {
		if accepted != nil {
			// Allow several slow GitHub reads without renewing the budget indefinitely.
			verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 150*time.Second)
			configured, current, valid, verifyErr := m.validateAcceptedSource(verifyCtx, *accepted, false)
			if verifyErr == nil {
				verifyErr = verifyCtx.Err()
			}
			cancel()
			if verifyErr != nil {
				return fmt.Errorf("revalidate %s before failure reply: %w", itemLocation(job.Source), verifyErr)
			}
			if !valid {
				m.logf("discard accepted %s: source or requester is no longer valid", itemLocation(job.Source))
				return store.WithLock(func() error { return discardAccepted(store) })
			}
			job.Repository, job.Source = configured, current
		}
		repoCtx, cancelRepo := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		repo, err := m.GitHub.Repository(repoCtx, job.Repository.Name)
		if err == nil {
			err = repoCtx.Err()
		}
		cancelRepo()
		if err != nil {
			return fmt.Errorf("verify %s before failure reply: %w", job.Repository.Name, err)
		}
		if !job.Repository.MatchesIdentity(repo.ID, repo.Owner.ID) {
			return fmt.Errorf("%s identity changed before failure reply", job.Repository.Name)
		}
		replyCtx, cancelReply := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancelReply()
		_, err = m.GitHub.CreateIssueComment(replyCtx, job.Repository.Name, job.Number, failureReply+"\n\n"+marker)
		if err != nil {
			failure.ReplyAttempts++
			if failure.ReplyAttempts < maxFailureReplyAttempts {
				var pending state.PendingNotification
				if saveErr := store.WithLock(func() error {
					previousFailure, hadFailure := store.Data.JobFailures[job.ID]
					var previousPending state.PendingNotification
					var hadPending bool
					if replyRetry != nil {
						previousPending, hadPending = store.Data.PendingNotifications[replyRetry.key]
					}
					store.Data.JobFailures[job.ID] = failure
					if replyRetry != nil {
						pending = deferNotificationState(store, replyRetry.key, replyRetry.notification, replyRetry.since, m.now().UTC())
					}
					if saveErr := saveJobResult(store, accepted != nil); saveErr != nil {
						if hadFailure {
							store.Data.JobFailures[job.ID] = previousFailure
						} else {
							delete(store.Data.JobFailures, job.ID)
						}
						if replyRetry != nil {
							if hadPending {
								store.Data.PendingNotifications[replyRetry.key] = previousPending
							} else {
								delete(store.Data.PendingNotifications, replyRetry.key)
							}
						}
						return saveErr
					}
					return nil
				}); saveErr != nil {
					return errors.Join(err, saveErr)
				}
				if replyRetry != nil {
					m.logf("retry %s %s after %s (failure %d)", pending.Repository, pending.SubjectURL, pending.RetryAt.Format(time.RFC3339), pending.Failures)
				}
				return fmt.Errorf("post failure reply to %s: %w", itemLocation(job.Source), err)
			}
			m.logf("could not post failure reply to %s after %d attempts: %v", itemLocation(job.Source), failure.ReplyAttempts, err)
		}
	}
	err = store.WithLock(func() error {
		processed, hadProcessed := store.Data.Processed[job.ID]
		previousFailure, hadFailure := store.Data.JobFailures[job.ID]
		previousAccepted := store.Data.Accepted
		store.Data.Processed[job.ID] = at
		delete(store.Data.JobFailures, job.ID)
		if accepted != nil {
			store.Data.Accepted = nil
		}
		if err := saveJobResult(store, accepted != nil); err != nil {
			if hadProcessed {
				store.Data.Processed[job.ID] = processed
			} else {
				delete(store.Data.Processed, job.ID)
			}
			if hadFailure {
				store.Data.JobFailures[job.ID] = previousFailure
			} else {
				delete(store.Data.JobFailures, job.ID)
			}
			store.Data.Accepted = previousAccepted
			return err
		}
		return nil
	})
	if err == nil {
		m.logf("stop retrying failed %s from %s", job.Command.Action, itemLocation(job.Source))
	}
	return err
}

func (m *Monitor) acceptWhitelistedInvitations(ctx context.Context) ([]string, error) {
	var deferred []string
	listCtx, cancelList := context.WithTimeout(ctx, 30*time.Second)
	invitations, listErr := m.GitHub.ListRepositoryInvitations(listCtx)
	cancelList()
	if invitationActionsBlocked(listErr) {
		return nil, listErr
	}
	actionCtx, cancelActions := context.WithTimeout(ctx, 30*time.Second)
	defer cancelActions()
	var failures []error
	if listErr != nil {
		failures = append(failures, listErr)
	}
	var outsiders []github.Invitation
	for _, invite := range invitations {
		if err := actionCtx.Err(); err != nil {
			failures = append(failures, err)
			return deferred, errors.Join(failures...)
		}
		fullName := invite.Repository.FullName
		owner, name, ok := strings.Cut(fullName, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") ||
			invite.Repository.Owner.Login == "" || !strings.EqualFold(owner, invite.Repository.Owner.Login) || invite.Expired {
			continue
		}
		monitoredRepo, monitored := m.Config.Repository(fullName)
		if invite.Repository.ID <= 0 || invite.Repository.Owner.ID <= 0 ||
			(monitored && !monitoredRepo.MatchesIdentity(invite.Repository.ID, invite.Repository.Owner.ID)) {
			continue
		}
		allowed := monitored
		ownerListed := false
		for _, allowedOwner := range m.Config.InvitationOwners {
			if strings.EqualFold(owner, allowedOwner) {
				ownerListed = true
				if m.Config.InvitationOwnerIDs[allowedOwner] == invite.Repository.Owner.ID {
					allowed = true
				}
				break
			}
		}
		if invite.Invitee.Login != "" && !strings.EqualFold(invite.Invitee.Login, m.Config.BotLogin) {
			continue
		}
		if !allowed {
			knownIdentity := ownerListed
			for _, id := range m.Config.InvitationOwnerIDs {
				knownIdentity = knownIdentity || id == invite.Repository.Owner.ID
			}
			for _, repo := range m.Config.Repositories {
				knownIdentity = knownIdentity || repo.ID == invite.Repository.ID || repo.OwnerID == invite.Repository.Owner.ID
			}
			if !knownIdentity {
				outsiders = append(outsiders, invite)
			}
			continue
		}
		switch invite.Permissions {
		case "read", "triage", "write", "maintain", "admin":
		default:
			failures = append(failures, fmt.Errorf("%s invitation has unknown permission %q", fullName, invite.Permissions))
			continue
		}
		if monitored && invite.Permissions != "write" && invite.Permissions != "admin" && invite.Permissions != "maintain" {
			failures = append(failures, fmt.Errorf("%s invitation grants %q, but configured actions require write access", fullName, invite.Permissions))
			continue
		}
		if !monitored && (invite.Permissions == "write" || invite.Permissions == "maintain" || invite.Permissions == "admin") {
			// Preserve the cursor even if the config write reports an ambiguous failure.
			deferred = append(deferred, strings.ToLower(fullName))
			actions := m.Config.CommandNames()
			// Persist first: an accepted invitation disappears and cannot recover a failed config write.
			repo, err := config.AddRepository(m.ConfigPath, config.Repository{Name: fullName, ID: invite.Repository.ID, OwnerID: invite.Repository.Owner.ID, Actions: actions})
			if err != nil {
				failures = append(failures, fmt.Errorf("register %s invitation: %w", fullName, err))
				continue
			}
			if !repo.MatchesIdentity(invite.Repository.ID, invite.Repository.Owner.ID) {
				failures = append(failures, fmt.Errorf("%s identity changed while registering invitation", fullName))
				continue
			}
			if m.SelectedExecutorID == "" && m.SelectedCommands == nil {
				m.Config.Repositories = append(m.Config.Repositories, repo)
			}
			m.logf("added %s to monitored repositories in %s", repo.Name, m.ConfigPath)
		}
		if err := m.GitHub.AcceptRepositoryInvitation(actionCtx, invite.ID); err != nil {
			failures = append(failures, fmt.Errorf("accept %s invitation: %w", fullName, err))
			if invitationActionsBlocked(err) {
				return deferred, errors.Join(failures...)
			}
			continue
		}
		m.logf("accepted repository invitation for %s (%s)", fullName, invite.Permissions)
		if invite.Permissions == "read" || invite.Permissions == "triage" {
			m.logf("%s is not monitored: commands require write access", fullName)
		}
	}
	for _, invite := range outsiders {
		if err := actionCtx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if err := m.GitHub.DeclineRepositoryInvitation(actionCtx, invite.ID); err != nil {
			failures = append(failures, fmt.Errorf("decline %s invitation: %w", invite.Repository.FullName, err))
			if invitationActionsBlocked(err) {
				break
			}
		}
	}
	return deferred, errors.Join(failures...)
}

func invitationActionsBlocked(err error) bool {
	var status *github.StatusError
	return errors.As(err, &status) &&
		(status.StatusCode == http.StatusForbidden || status.StatusCode == http.StatusTooManyRequests)
}

func jobID(itemKey, commandName string) string {
	sum := sha256.Sum256([]byte(itemKey + "\x00" + commandName))
	return hex.EncodeToString(sum[:])
}

func subjectNumber(rawURL, repo, subjectType string) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || u.Path == "" {
		return 0, fmt.Errorf("unparseable subject URL %q", rawURL)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	expected := map[string]string{"Issue": "issues", "PullRequest": "pulls"}[subjectType]
	ownerName := strings.Split(repo, "/")
	if len(parts) >= 5 && parts[0] == "repos" && strings.EqualFold(parts[1], ownerName[0]) && strings.EqualFold(parts[2], ownerName[1]) && parts[3] == expected {
		return positiveNumber(parts[4])
	}
	if len(parts) >= 4 && strings.EqualFold(parts[0], ownerName[0]) && strings.EqualFold(parts[1], ownerName[1]) && parts[2] == expected {
		return positiveNumber(parts[3])
	}
	return 0, fmt.Errorf("subject URL does not identify %s %s: %q", repo, subjectType, rawURL)
}

func positiveNumber(value string) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("invalid subject number %q", value)
	}
	return number, nil
}
