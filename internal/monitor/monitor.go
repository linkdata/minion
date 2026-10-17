package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

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
	ListRepositoryInvitations(context.Context) ([]github.Invitation, error)
	AcceptRepositoryInvitation(context.Context, int64) error
	DeclineRepositoryInvitation(context.Context, int64) error
	ThreadItems(context.Context, string, string, int, time.Time) ([]github.Item, error)
	CurrentItem(context.Context, github.Item) (github.Item, error)
	Permission(context.Context, string, string) (github.Permission, error)
	ReactEyes(context.Context, string) error
	CreateIssueComment(context.Context, string, int, string) (github.Comment, error)
}

// ErrTerminalJob marks a failed execution that cannot succeed by rerunning the models.
var ErrTerminalJob = errors.New("terminal job failure")

// ErrPostModelPublication marks a failed GitHub write after model execution completed.
//
// Retrying it must consume a job attempt even when the write error is transient.
var ErrPostModelPublication = errors.New("post-model publication failure")

const (
	maxJobAttempts          = 3
	maxFailureReplyAttempts = 3
	failureReply            = "Minion could not complete this request. No further automatic attempts will run. Check the service log and post a new command after the cause is fixed."
)

type Executor interface {
	Execute(context.Context, Job) error
}

type Job struct {
	Repository  config.Repository
	SubjectType string
	Number      int
	Source      github.Item
	Command     command.Command
	ID          string
}

func (j Job) Marker() string {
	return "<!-- linkdataminion:" + j.ID + " -->"
}

type Monitor struct {
	Config     config.Config
	ConfigPath string
	GitHub     GitHub
	Executor   Executor
	Logger     *log.Logger
	Now        func() time.Time
}

func (m *Monitor) logf(format string, args ...any) {
	if m.Logger != nil {
		m.Logger.Printf(format, args...)
	}
}

// RunOnce processes new notifications in whitelisted repositories.
//
// It persists each completed command before advancing that repository's cursor.
// Failed threads remain queued for retries without blocking other repositories.
func (m *Monitor) RunOnce(ctx context.Context, store *state.Store) (time.Time, error) {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	if !now().UTC().Before(store.Data.NextInviteCheckAt) {
		if err := m.acceptWhitelistedInvitations(ctx); err != nil {
			// Invitation failures should be visible, but should not stop scans of
			// repositories for which this account already has access.
			m.logf("invitation check: %v", err)
		}
		store.Data.NextInviteCheckAt = now().UTC().Add(15 * time.Minute)
		if err := store.Save(); err != nil {
			return store.Data.NextPollAt, err
		}
	}
	started := now().UTC()
	if started.Before(store.Data.NextPollAt) {
		return store.Data.NextPollAt, nil
	}
	minInterval := time.Duration(m.Config.PollIntervalSeconds) * time.Second
	if minInterval < time.Minute {
		minInterval = time.Minute
	}
	next := started.Add(minInterval)
	store.Data.NextPollAt = next
	if store.Data.RepositoryCursors == nil {
		store.Data.RepositoryCursors = make(map[string]time.Time)
	}
	for _, repo := range m.Config.Repositories {
		if _, ok := store.Data.RepositoryCursors[repo.Name]; !ok {
			// Migrate the old account cursor before any repository poll can fail.
			store.Data.RepositoryCursors[repo.Name] = store.Data.Cursor
		}
	}
	if err := store.Save(); err != nil {
		return next, err
	}
	commandSets := make(map[string]map[string]command.Action, len(m.Config.Repositories))
	for _, repo := range m.Config.Repositories {
		commandSets[repo.Name] = m.availableCommands(repo)
	}

	type scan struct {
		notification github.Notification
		since        time.Time
		key          string
	}
	var scans []scan
	seen := make(map[string]bool)
	var failures []error
	var polled []string
	for _, repo := range m.Config.Repositories {
		since := store.Data.RepositoryCursors[repo.Name]
		notifications, githubInterval, err := m.GitHub.ListRepositoryNotifications(ctx, repo.Name, since)
		if githubInterval > minInterval {
			minInterval = githubInterval
			next = started.Add(minInterval)
			store.Data.NextPollAt = next
			if err := store.Save(); err != nil {
				return next, err
			}
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
			scans = append(scans, scan{notification, scanSince, key})
		}
	}
	for key, pending := range store.Data.PendingNotifications {
		if seen[key] || started.Before(pending.RetryAt) {
			continue
		}
		scans = append(scans, scan{github.Notification{
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
	for _, entry := range scans {
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
		if err != nil {
			failures = append(failures, fmt.Errorf("verify %s repository identity: %w", repo.Name, err))
			if err := m.deferNotification(store, entry.key, notification, entry.since, started); err != nil {
				return next, err
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
		items, err := m.GitHub.ThreadItems(ctx, repo.Name, notification.Subject.Type, number, entry.since)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s %s #%d: %w", repo.Name, notification.Subject.Type, number, err))
			if err := m.deferNotification(store, entry.key, notification, entry.since, started); err != nil {
				return next, err
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
				m.logf("skip command in %s: %v", item.HTMLURL, err)
				continue
			}
			if _, done := store.Data.Processed[jobID(item.Key, preliminary.Key)]; done {
				continue
			}
			current, err := m.GitHub.CurrentItem(ctx, item)
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
				m.logf("skip command in %s: %v", item.HTMLURL, err)
				continue
			}
			job := Job{
				Repository:  repo,
				SubjectType: notification.Subject.Type,
				Number:      number,
				Source:      item,
				Command:     cmd,
				ID:          jobID(item.Key, cmd.Key),
			}
			if _, done := store.Data.Processed[job.ID]; done {
				continue
			}
			if item.Edited {
				// The latest editor may not be the actor who introduced a
				// retained command. Do not run commands from edited content.
				m.logf("ignore edited command in %s (last editor @%s)", item.HTMLURL, item.Editor.Login)
				store.Data.Processed[job.ID] = started
				if err := store.Save(); err != nil {
					return next, err
				}
				continue
			}
			if !repo.AllowsAction(cmd.Name) || !repo.AllowsActor(item.Author.Login, item.Author.ID) {
				m.logf("ignore %s from @%s in %s: disabled action or actor", cmd.Action, item.Author.Login, repo.Name)
				store.Data.Processed[job.ID] = started
				if err := store.Save(); err != nil {
					return next, err
				}
				continue
			}
			permission, err := m.GitHub.Permission(ctx, repo.Name, item.Author.Login)
			if err != nil {
				failures = append(failures, fmt.Errorf("check @%s permission in %s: %w", item.Author.Login, repo.Name, err))
				continue
			}
			if permission.User.ID != item.Author.ID || !strings.EqualFold(permission.User.Login, item.Author.Login) {
				m.logf("ignore %s from @%s in %s: permission identity differs", cmd.Action, item.Author.Login, repo.Name)
				store.Data.Processed[job.ID] = started
				if err := store.Save(); err != nil {
					return next, err
				}
				continue
			}
			if permission.Permission != "write" && permission.Permission != "admin" && permission.Permission != "maintain" {
				m.logf("ignore %s from @%s in %s: permission %q", cmd.Action, item.Author.Login, repo.Name, permission.Permission)
				store.Data.Processed[job.ID] = started
				if err := store.Save(); err != nil {
					return next, err
				}
				continue
			}
			jobFailure := store.Data.JobFailures[job.ID]
			if jobFailure.Attempts >= maxJobAttempts {
				if err := m.finishFailedJob(ctx, store, job, items, started, jobFailure); err != nil {
					failures = append(failures, err)
				}
				continue
			}
			if err := m.GitHub.ReactEyes(ctx, item.NodeID); err != nil {
				failures = append(failures, fmt.Errorf("react to %s: %w", item.HTMLURL, err))
				continue
			}
			// Count an interrupted execution before starting it, so a crash cannot
			// restart an expensive job forever without consuming an attempt.
			jobFailure.Attempts++
			store.Data.JobFailures[job.ID] = jobFailure
			if err := store.Save(); err != nil {
				return next, err
			}
			m.logf("run %s from @%s in %s #%d", cmd.Action, item.Author.Login, repo.Name, number)
			if err := m.Executor.Execute(ctx, job); err != nil {
				interrupted := ctx.Err() != nil && errors.Is(err, context.Canceled) && !errors.Is(err, ErrPostModelPublication)
				if interrupted {
					jobFailure.Attempts--
				} else if errors.Is(err, ErrTerminalJob) {
					jobFailure.Attempts = maxJobAttempts
				} else if !errors.Is(err, ErrPostModelPublication) && (errors.Is(err, workroot.ErrUnsafe) || infrastructureFailure(err)) {
					jobFailure.Attempts--
				}
				store.Data.JobFailures[job.ID] = jobFailure
				if saveErr := store.Save(); saveErr != nil {
					return next, errors.Join(err, saveErr)
				}
				if interrupted {
					m.logf("%s from %s interrupted: %v", cmd.Action, item.HTMLURL, err)
					return next, err
				}
				m.logf("%s from %s failed (attempt %d/%d): %v", cmd.Action, item.HTMLURL, jobFailure.Attempts, maxJobAttempts, err)
				if jobFailure.Attempts >= maxJobAttempts {
					if replyErr := m.finishFailedJob(ctx, store, job, items, started, jobFailure); replyErr != nil {
						failures = append(failures, replyErr)
					}
					if ctx.Err() != nil {
						return next, errors.Join(failures...)
					}
					continue
				}
				if ctx.Err() != nil {
					return next, errors.Join(append(failures, err)...)
				}
				failures = append(failures, fmt.Errorf("%s from %s: %w", cmd.Action, item.HTMLURL, err))
				continue
			}
			store.Data.Processed[job.ID] = started
			delete(store.Data.JobFailures, job.ID)
			delete(store.Data.PendingFixes, job.ID)
			if err := store.Save(); err != nil {
				return next, err
			}
			if err := ctx.Err(); err != nil {
				return next, err
			}
		}
		if len(failures) != firstFailure {
			if err := m.deferNotification(store, entry.key, notification, entry.since, started); err != nil {
				return next, err
			}
		} else if _, pending := store.Data.PendingNotifications[entry.key]; pending {
			delete(store.Data.PendingNotifications, entry.key)
			if err := store.Save(); err != nil {
				return next, err
			}
		}
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
		return next, err
	}
	return next, errors.Join(failures...)
}

func (m *Monitor) availableCommands(repo config.Repository) map[string]command.Action {
	available := make(map[string]command.Action, len(repo.Actions))
	for _, name := range repo.Actions {
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
	if err := store.Save(); err != nil {
		return err
	}
	m.logf("retry %s %s after %s (failure %d)", pending.Repository, pending.SubjectURL, pending.RetryAt.Format(time.RFC3339), pending.Failures)
	return nil
}

func infrastructureFailure(err error) bool {
	var status *github.StatusError
	if errors.As(err, &status) && (status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden ||
		status.StatusCode == http.StatusRequestTimeout || status.StatusCode == http.StatusTooManyRequests || status.StatusCode >= http.StatusInternalServerError) {
		return true
	}
	var network *url.Error
	return errors.As(err, &network)
}

func (m *Monitor) finishFailedJob(ctx context.Context, store *state.Store, job Job, items []github.Item, at time.Time, failure state.JobFailure) error {
	marker := job.Marker()
	replied := false
	for _, item := range items {
		if strings.EqualFold(item.Author.Login, m.Config.BotLogin) && strings.HasSuffix(strings.TrimSpace(item.Body), "\n\n"+marker) {
			replied = true
			break
		}
	}
	if !replied {
		repo, err := m.GitHub.Repository(ctx, job.Repository.Name)
		if err != nil {
			return fmt.Errorf("verify %s before failure reply: %w", job.Repository.Name, err)
		}
		if !job.Repository.MatchesIdentity(repo.ID, repo.Owner.ID) {
			return fmt.Errorf("%s identity changed before failure reply", job.Repository.Name)
		}
		replyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, err = m.GitHub.CreateIssueComment(replyCtx, job.Repository.Name, job.Number, failureReply+"\n\n"+marker)
		cancel()
		if err != nil {
			failure.ReplyAttempts++
			if failure.ReplyAttempts < maxFailureReplyAttempts {
				store.Data.JobFailures[job.ID] = failure
				if saveErr := store.Save(); saveErr != nil {
					return errors.Join(err, saveErr)
				}
				return fmt.Errorf("post failure reply to %s: %w", job.Source.HTMLURL, err)
			}
			m.logf("could not post failure reply to %s after %d attempts: %v", job.Source.HTMLURL, failure.ReplyAttempts, err)
		}
	}
	m.logf("stop retrying failed %s from %s", job.Command.Action, job.Source.HTMLURL)
	store.Data.Processed[job.ID] = at
	delete(store.Data.JobFailures, job.ID)
	return store.Save()
}

func (m *Monitor) acceptWhitelistedInvitations(ctx context.Context) error {
	listCtx, cancelList := context.WithTimeout(ctx, 30*time.Second)
	invitations, listErr := m.GitHub.ListRepositoryInvitations(listCtx)
	cancelList()
	if invitationActionsBlocked(listErr) {
		return listErr
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
			return errors.Join(failures...)
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
			m.Config.Repositories = append(m.Config.Repositories, repo)
			m.logf("added %s to monitored repositories in %s", repo.Name, m.ConfigPath)
		}
		if err := m.GitHub.AcceptRepositoryInvitation(actionCtx, invite.ID); err != nil {
			failures = append(failures, fmt.Errorf("accept %s invitation: %w", fullName, err))
			if invitationActionsBlocked(err) {
				return errors.Join(failures...)
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
	return errors.Join(failures...)
}

func invitationActionsBlocked(err error) bool {
	var status *github.StatusError
	return errors.As(err, &status) &&
		(status.StatusCode == http.StatusForbidden || status.StatusCode == http.StatusTooManyRequests)
}

func jobID(itemKey, commandKey string) string {
	sum := sha256.Sum256([]byte(itemKey + "\x00" + commandKey))
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
