package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/state"
)

type GitHub interface {
	ListNotifications(context.Context, time.Time) ([]github.Notification, time.Duration, error)
	ListRepositoryInvitations(context.Context) ([]github.Invitation, error)
	AcceptRepositoryInvitation(context.Context, int64) error
	ThreadItems(context.Context, string, string, int, time.Time) ([]github.Item, error)
	CurrentItem(context.Context, github.Item) (github.Item, error)
	Permission(context.Context, string, string) (github.Permission, error)
	ReactEyes(context.Context, string) error
}

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

// RunOnce polls the account's notifications, inspects new content in the
// whitelisted repositories, and persists each completed command before moving
// the cursor. Any transient failure leaves the cursor in place for a retry.
func (m *Monitor) RunOnce(ctx context.Context, store *state.Store) (time.Time, error) {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	inviteCheck := now().UTC()
	if !inviteCheck.Before(store.Data.NextInviteCheckAt) {
		if err := m.acceptWhitelistedInvitations(ctx); err != nil {
			// Invitation failures should be visible, but should not stop scans of
			// repositories for which this account already has access.
			m.logf("invitation check: %v", err)
			store.Data.NextInviteCheckAt = inviteCheck.Add(5 * time.Minute)
		} else {
			store.Data.NextInviteCheckAt = inviteCheck.Add(15 * time.Minute)
		}
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
	if err := store.Save(); err != nil {
		return next, err
	}

	since := store.Data.Cursor
	notifications, githubInterval, err := m.GitHub.ListNotifications(ctx, since)
	if githubInterval > minInterval {
		minInterval = githubInterval
		next = started.Add(minInterval)
		store.Data.NextPollAt = next
		if saveErr := store.Save(); saveErr != nil {
			return next, saveErr
		}
	}
	if err != nil {
		return next, fmt.Errorf("list notifications: %w", err)
	}

	sort.SliceStable(notifications, func(i, j int) bool {
		return notifications[i].UpdatedAt.Before(notifications[j].UpdatedAt)
	})
	var failures []error
	for _, notification := range notifications {
		repo, ok := m.Config.Repository(notification.Repository.FullName)
		if !ok {
			continue
		}
		if notification.Subject.Type != "Issue" && notification.Subject.Type != "PullRequest" {
			m.logf("skip %s notification %s: unsupported subject %q", repo.Name, notification.ID, notification.Subject.Type)
			continue
		}
		number, err := subjectNumber(notification.Subject.URL, repo.Name, notification.Subject.Type)
		if err != nil {
			m.logf("skip %s notification %s: %v", repo.Name, notification.ID, err)
			continue
		}
		items, err := m.GitHub.ThreadItems(ctx, repo.Name, notification.Subject.Type, number, since)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s %s #%d: %w", repo.Name, notification.Subject.Type, number, err))
			continue
		}
		for _, item := range items {
			if strings.EqualFold(item.Author.Login, m.Config.BotLogin) || item.Author.Login == "" {
				continue
			}
			_, err := command.ParseForLogin(item.Body, m.Config.BotLogin)
			if errors.Is(err, command.ErrNoCommand) {
				continue
			}
			if err != nil {
				m.logf("skip command in %s: %v", item.HTMLURL, err)
				continue
			}
			item, err = m.GitHub.CurrentItem(ctx, item)
			if err != nil {
				failures = append(failures, fmt.Errorf("verify command in %s: %w", item.HTMLURL, err))
				continue
			}
			if strings.EqualFold(item.Author.Login, m.Config.BotLogin) || item.Author.Login == "" {
				continue
			}
			cmd, err := command.ParseForLogin(item.Body, m.Config.BotLogin)
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
			if !repo.AllowsAction(string(cmd.Action)) || !repo.AllowsActor(item.Author.Login) {
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
			if permission.Permission != "write" && permission.Permission != "admin" && permission.Permission != "maintain" {
				m.logf("ignore %s from @%s in %s: permission %q", cmd.Action, item.Author.Login, repo.Name, permission.Permission)
				store.Data.Processed[job.ID] = started
				if err := store.Save(); err != nil {
					return next, err
				}
				continue
			}
			if err := m.GitHub.ReactEyes(ctx, item.NodeID); err != nil {
				failures = append(failures, fmt.Errorf("react to %s: %w", item.HTMLURL, err))
				continue
			}
			m.logf("run %s from @%s in %s #%d", cmd.Action, item.Author.Login, repo.Name, number)
			if err := m.Executor.Execute(ctx, job); err != nil {
				failures = append(failures, fmt.Errorf("%s from %s: %w", cmd.Action, item.HTMLURL, err))
				continue
			}
			store.Data.Processed[job.ID] = started
			delete(store.Data.PendingFixes, job.ID)
			delete(store.Data.PendingReleases, job.ID)
			if err := store.Save(); err != nil {
				return next, err
			}
		}
	}
	if len(failures) != 0 {
		return next, errors.Join(failures...)
	}
	// GitHub timestamps have second precision. Keep a two-second overlap so an
	// update racing with this poll cannot fall on the exclusive since boundary.
	newCursor := started.Add(-2 * time.Second)
	if newCursor.After(store.Data.Cursor) {
		store.Data.Cursor = newCursor
	}
	if err := store.Save(); err != nil {
		return next, err
	}
	return next, nil
}

func (m *Monitor) acceptWhitelistedInvitations(ctx context.Context) error {
	invitations, err := m.GitHub.ListRepositoryInvitations(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, invite := range invitations {
		fullName := invite.Repository.FullName
		owner, name, ok := strings.Cut(fullName, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") ||
			invite.Repository.Owner.Login == "" || !strings.EqualFold(owner, invite.Repository.Owner.Login) || invite.Expired {
			continue
		}
		_, monitored := m.Config.Repository(fullName)
		allowed := monitored
		for _, allowedOwner := range m.Config.InvitationOwners {
			if strings.EqualFold(owner, allowedOwner) {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		if invite.Invitee.Login != "" && !strings.EqualFold(invite.Invitee.Login, m.Config.BotLogin) {
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
			actions := []string{"review", "fix", "release"}
			if m.Config.Codex.SecurityAuditSkill() != "" {
				actions = append(actions, "audit")
			}
			// Persist first: an accepted invitation disappears and cannot recover a failed config write.
			repo, err := config.AddRepository(m.ConfigPath, config.Repository{Name: fullName, Actions: actions})
			if err != nil {
				failures = append(failures, fmt.Errorf("register %s invitation: %w", fullName, err))
				continue
			}
			m.Config.Repositories = append(m.Config.Repositories, repo)
			m.logf("added %s to monitored repositories in %s", repo.Name, m.ConfigPath)
		}
		if err := m.GitHub.AcceptRepositoryInvitation(ctx, invite.ID); err != nil {
			failures = append(failures, fmt.Errorf("accept %s invitation: %w", fullName, err))
			continue
		}
		m.logf("accepted repository invitation for %s (%s)", fullName, invite.Permissions)
		if invite.Permissions == "read" || invite.Permissions == "triage" {
			m.logf("%s is not monitored: commands require write access", fullName)
		}
	}
	return errors.Join(failures...)
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
