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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := liveClient(t, ctx, cfg)

	user, err := client.AuthenticatedUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(user.Login, cfg.OwnerLogin) {
		t.Fatalf("authenticated user = %q, want %q", user.Login, cfg.OwnerLogin)
	}

	repo, err := client.Repository(ctx, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(repo.Owner.Login, cfg.OwnerLogin) || repo.DefaultBranch == "" {
		t.Fatalf("repository metadata = %+v", repo)
	}

	if strings.EqualFold(cfg.Repo, "linkdata/miniontesting") {
		t.Run("bot permission", func(t *testing.T) {
			permission, err := client.Permission(ctx, cfg.Repo, cfg.BotLogin)
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
		})
	}

	t.Run("notifications", func(t *testing.T) {
		_, interval, err := client.ListNotifications(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if interval <= 0 {
			t.Fatalf("notification poll interval = %s", interval)
		}
	})

	t.Run("branch and tree", func(t *testing.T) {
		branch, err := client.Branch(ctx, cfg.Repo, repo.DefaultBranch)
		if err != nil {
			t.Fatal(err)
		}
		if branch.Name != repo.DefaultBranch || len(branch.Commit.SHA) != 40 {
			t.Fatalf("default branch = %+v", branch)
		}
		treeSHA := liveAPIField(t, ctx, cfg.OwnerLogin, "repos/"+cfg.Repo+"/git/commits/"+branch.Commit.SHA, ".tree.sha")
		tree, err := client.GitTree(ctx, cfg.Repo, treeSHA)
		if err != nil {
			t.Fatal(err)
		}
		if tree.SHA != treeSHA || tree.Truncated == nil || *tree.Truncated || len(tree.Tree) == 0 {
			t.Fatalf("recursive tree = %+v", tree)
		}
	})

	if cfg.IssueNumber > 0 {
		t.Run("issue and comment", func(t *testing.T) {
			issue, err := client.IssueDetails(ctx, cfg.Repo, cfg.IssueNumber)
			if err != nil {
				t.Fatal(err)
			}
			if issue.Number != cfg.IssueNumber || issue.NodeID == "" || issue.Title == "" {
				t.Fatalf("issue metadata = %+v", issue)
			}
			content, err := client.CurrentIssueContent(ctx, issue.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			if content.Title != issue.Title || content.BodyText == "" {
				t.Fatalf("rendered issue content = %+v, want title %q and nonempty text", content, issue.Title)
			}
			currentIssue, err := client.CurrentItem(ctx, Item{NodeID: issue.NodeID})
			if err != nil {
				t.Fatal(err)
			}
			if currentIssue.NodeID != issue.NodeID || currentIssue.Body == "" || !strings.EqualFold(currentIssue.Author.Login, issue.User.Login) {
				t.Fatalf("current issue body = %+v", currentIssue)
			}
			items, err := client.ThreadItems(ctx, cfg.Repo, "Issue", cfg.IssueNumber, time.Time{})
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
			current, err := client.CurrentItem(ctx, *comment)
			if err != nil {
				t.Fatal(err)
			}
			if current.NodeID != comment.NodeID || !strings.EqualFold(current.Author.Login, cfg.OwnerLogin) || current.Body == "" {
				t.Fatalf("current owner comment = %+v", current)
			}
		})
	}

	if cfg.PullRequestNumber > 0 {
		t.Run("pull request and comment", func(t *testing.T) {
			pull, err := client.PullRequestDetails(ctx, cfg.Repo, cfg.PullRequestNumber)
			if err != nil {
				t.Fatal(err)
			}
			if pull.Number != cfg.PullRequestNumber || pull.NodeID == "" || pull.Head.SHA == "" || pull.Base.Ref == "" || pull.Head.Ref == "" || !pull.Draft || pull.State != "open" {
				t.Fatalf("pull request metadata = %+v", pull)
			}
			found, err := client.FindPullRequestByHead(ctx, cfg.Repo, pull.Base.Ref, pull.Head.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if found == nil || found.Number != pull.Number || found.Head.SHA != pull.Head.SHA {
				t.Fatalf("pull request by head = %+v, want #%d at %s", found, pull.Number, pull.Head.SHA)
			}
			items, err := client.ThreadItems(ctx, cfg.Repo, "PullRequest", cfg.PullRequestNumber, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var body, comment bool
			for _, item := range items {
				body = body || item.NodeID == pull.NodeID && item.Key == strings.ToLower(cfg.Repo)+"#pull:"+strconv.Itoa(cfg.PullRequestNumber)
				comment = comment || strings.Contains(item.Key, "#issue-comment:") && strings.EqualFold(item.Author.Login, cfg.OwnerLogin) && item.Body != ""
			}
			if !body || !comment {
				t.Fatalf("pull request thread missing body or owner comment: %+v", items)
			}
		})
	}

	if cfg.ReleaseTag != "" {
		t.Run("release and tag", func(t *testing.T) {
			protected, err := client.ProtectedReleaseTags(ctx, cfg.Repo)
			if err != nil {
				t.Fatal(err)
			}
			if strings.EqualFold(cfg.Repo, "linkdata/miniontesting") && protected {
				t.Fatal("test repository unexpectedly has protected release tags")
			}
			ref, err := client.TagReference(ctx, cfg.Repo, cfg.ReleaseTag)
			if err != nil {
				t.Fatal(err)
			}
			if ref.Ref != "refs/tags/"+cfg.ReleaseTag || len(ref.Object.SHA) != 40 {
				t.Fatalf("tag reference = %+v", ref)
			}
			release, err := client.ReleaseByTag(ctx, cfg.Repo, cfg.ReleaseTag)
			if err != nil {
				t.Fatal(err)
			}
			if release.TagName != cfg.ReleaseTag || release.ID == 0 || !release.Draft {
				t.Fatalf("release = %+v", release)
			}
		})
	}
}
