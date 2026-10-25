package github

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLiveClientReadOnly(t *testing.T) {
	cfg := loadLiveTestConfig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	owner := liveClient(t, ctx, cfg.OwnerLogin)
	bot := liveClient(t, ctx, cfg.BotLogin)

	for _, account := range []struct {
		client *Client
		login  string
	}{{owner, cfg.OwnerLogin}, {bot, cfg.BotLogin}} {
		user, err := account.client.AuthenticatedUser(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(user.Login, account.login) {
			t.Fatalf("authenticated user = %q, want %q", user.Login, account.login)
		}
	}

	repo, err := bot.Repository(ctx, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(repo.Owner.Login, cfg.OwnerLogin) || repo.DefaultBranch == "" {
		t.Fatalf("repository metadata = %+v", repo)
	}
	checkCurrent := func(t *testing.T, item Item) {
		t.Helper()
		current, err := bot.CurrentItem(ctx, item)
		if err != nil {
			t.Fatal(err)
		}
		if current.NodeID != item.NodeID || current.Body != item.Body || !strings.EqualFold(current.Author.Login, item.Author.Login) {
			t.Fatalf("current item = %+v, want node %q, body %q, author %q", current, item.NodeID, item.Body, item.Author.Login)
		}
	}

	if strings.EqualFold(cfg.Repo, "linkdata/miniontesting") {
		t.Run("bot permission", func(t *testing.T) {
			permission, err := owner.Permission(ctx, cfg.Repo, cfg.BotLogin)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.EqualFold(permission.User.Login, cfg.BotLogin) {
				t.Fatalf("permission user = %q, want %q", permission.User.Login, cfg.BotLogin)
			}
			// The public fixture is readable before an invitation and writable after one.
			if permission.Permission == "" || (permission.RoleName != "read" && permission.RoleName != "write" && permission.RoleName != "maintain" && permission.RoleName != "admin") {
				t.Fatalf("bot permission = %+v, want public read or collaborator write access", permission)
			}
			if permission.RoleName == "read" {
				return
			}
			actor, err := bot.Permission(ctx, cfg.Repo, cfg.OwnerLogin)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.EqualFold(actor.User.Login, cfg.OwnerLogin) || actor.Permission != "admin" {
				t.Fatalf("owner command actor permission = %+v, want admin", actor)
			}
		})
	}

	t.Run("notifications", func(t *testing.T) {
		notifications, interval, err := bot.ListRepositoryNotifications(ctx, cfg.Repo, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if interval <= 0 {
			t.Fatalf("notification poll interval = %s", interval)
		}
		for _, notification := range notifications {
			if notification.ID == "" || !strings.EqualFold(notification.Repository.FullName, cfg.Repo) || notification.Subject.Type == "" || notification.UpdatedAt.IsZero() {
				t.Fatalf("incomplete notification = %+v", notification)
			}
			if notification.Subject.Type == "Issue" || notification.Subject.Type == "PullRequest" {
				if !strings.HasPrefix(notification.Subject.URL, "https://api.github.com/repos/"+notification.Repository.FullName+"/") {
					t.Fatalf("notification subject URL = %q for %s", notification.Subject.URL, notification.Repository.FullName)
				}
			}
		}
	})

	t.Run("branch", func(t *testing.T) {
		branch, err := bot.Branch(ctx, cfg.Repo, repo.DefaultBranch)
		if err != nil {
			t.Fatal(err)
		}
		if branch.Name != repo.DefaultBranch || len(branch.Commit.SHA) != 40 {
			t.Fatalf("default branch = %+v", branch)
		}
	})

	if cfg.IssueNumber > 0 {
		t.Run("issue and comment", func(t *testing.T) {
			issue, err := bot.IssueDetails(ctx, cfg.Repo, cfg.IssueNumber)
			if err != nil {
				t.Fatal(err)
			}
			if issue.Number != cfg.IssueNumber || issue.NodeID == "" || issue.Title == "" {
				t.Fatalf("issue metadata = %+v", issue)
			}
			content, err := bot.CurrentIssueContent(ctx, issue.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			if content.Title != issue.Title || content.BodyText == "" {
				t.Fatalf("rendered issue content = %+v, want title %q and nonempty text", content, issue.Title)
			}
			checkCurrent(t, Item{NodeID: issue.NodeID, Body: issue.Body, Author: issue.User})
			items, err := bot.ThreadItems(ctx, cfg.Repo, "Issue", cfg.IssueNumber, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var comment *Item
			var issueBody bool
			for i := range items {
				if items[i].Key == strings.ToLower(cfg.Repo)+"#issue:"+strconv.Itoa(cfg.IssueNumber) && items[i].NodeID == issue.NodeID {
					issueBody = true
				}
				if comment == nil && strings.Contains(items[i].Key, "#issue-comment:") && strings.EqualFold(items[i].Author.Login, cfg.OwnerLogin) {
					comment = &items[i]
				}
			}
			if !issueBody {
				t.Fatalf("issue %d body missing from thread items", cfg.IssueNumber)
			}
			if comment == nil || comment.NodeID == "" {
				t.Fatalf("issue %d has no owner comment with a node ID", cfg.IssueNumber)
			}
			checkCurrent(t, *comment)
			if !comment.UpdatedAt.After(issue.CreatedAt.Add(time.Second)) {
				t.Fatal("issue fixture comment must postdate its body by more than one second")
			}
			later, err := bot.ThreadItems(ctx, cfg.Repo, "Issue", cfg.IssueNumber, comment.UpdatedAt)
			if err != nil {
				t.Fatal(err)
			}
			var foundComment bool
			for _, item := range later {
				if item.Key == strings.ToLower(cfg.Repo)+"#issue:"+strconv.Itoa(cfg.IssueNumber) {
					t.Fatal("issue body reappeared after its creation timestamp")
				}
				foundComment = foundComment || item.Key == comment.Key
			}
			if !foundComment {
				t.Fatal("issue comment missing at its update timestamp")
			}
		})
	}

	if cfg.PullRequestNumber > 0 {
		t.Run("pull request and comment", func(t *testing.T) {
			pull, err := bot.PullRequestDetails(ctx, cfg.Repo, cfg.PullRequestNumber)
			if err != nil {
				t.Fatal(err)
			}
			if pull.Number != cfg.PullRequestNumber || pull.NodeID == "" || pull.Head.SHA == "" || pull.Base.Ref == "" || pull.Head.Ref == "" || !pull.Draft || pull.State != "open" {
				t.Fatalf("pull request metadata = %+v", pull)
			}
			found, err := bot.FindPullRequestByHead(ctx, cfg.Repo, pull.Base.Ref, pull.Head.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if found == nil || found.Number != pull.Number || found.Head.SHA != pull.Head.SHA {
				t.Fatalf("pull request by head = %+v, want #%d at %s", found, pull.Number, pull.Head.SHA)
			}
			items, err := bot.ThreadItems(ctx, cfg.Repo, "PullRequest", cfg.PullRequestNumber, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var body, comment, reviewComment, review *Item
			for i := range items {
				item := &items[i]
				if item.NodeID == pull.NodeID && item.Key == strings.ToLower(cfg.Repo)+"#pull:"+strconv.Itoa(cfg.PullRequestNumber) {
					body = item
				}
				if strings.Contains(item.Key, "#issue-comment:") && strings.EqualFold(item.Author.Login, cfg.OwnerLogin) && item.Body != "" {
					comment = item
				}
				if strings.Contains(item.Key, "#review-comment:") && strings.EqualFold(item.Author.Login, cfg.BotLogin) && item.Body != "" && item.NodeID != "" {
					reviewComment = item
				}
				if strings.Contains(item.Key, "#review:") && strings.EqualFold(item.Author.Login, cfg.BotLogin) && item.Body != "" && item.NodeID != "" {
					review = item
				}
			}
			if body == nil || comment == nil {
				t.Fatalf("pull request thread missing body or owner comment: %+v", items)
			}
			checkCurrent(t, *body)
			checkCurrent(t, *comment)
			if strings.EqualFold(cfg.Repo, "linkdata/miniontesting") {
				if reviewComment == nil || review == nil {
					t.Fatalf("pull request fixture missing bot review comment or submitted review: %+v", items)
				}
				checkCurrent(t, *reviewComment)
				checkCurrent(t, *review)
			}
			if !comment.UpdatedAt.After(pull.CreatedAt.Add(time.Second)) {
				t.Fatal("pull request fixture comment must postdate its body by more than one second")
			}
			later, err := bot.ThreadItems(ctx, cfg.Repo, "PullRequest", cfg.PullRequestNumber, comment.UpdatedAt)
			if err != nil {
				t.Fatal(err)
			}
			var foundComment bool
			for _, item := range later {
				if item.Key == body.Key {
					t.Fatal("pull request body reappeared after its creation timestamp")
				}
				foundComment = foundComment || item.Key == comment.Key
			}
			if !foundComment {
				t.Fatal("pull request comment missing at its update timestamp")
			}
		})
	}
}
