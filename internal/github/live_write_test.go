package github

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const liveWriteRepoPath = "/repos/linkdata/miniontesting"

func TestLiveBotCollaborationLifecycle(t *testing.T) {
	if os.Getenv("MINION_LIVE_COLLABORATION") != "1" {
		t.Skip("set MINION_LIVE_COLLABORATION=1 to test collaborator invite and revocation")
	}
	if os.Getenv("MINION_LIVE_WRITE") == "1" {
		t.Skip("run collaborator lifecycle without MINION_LIVE_WRITE to avoid a second invitation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg, owner, bot, _ := liveBotWriteSetup(t, ctx)
	status, pending := liveBotAccessState(t, ctx, owner, cfg)
	if status != http.StatusNotFound || pending != nil {
		t.Fatal("collaboration lifecycle requires no existing bot access or pending invitation")
	}

	cleanup := true
	t.Cleanup(func() {
		if cleanup {
			liveRevokeTestBot(t, owner, liveWriteRepoPath, liveWriteRepoPath+"/collaborators/linkdataminion", cfg.BotLogin)
		}
	})
	_, status, err := owner.request(ctx, http.MethodPut, liveWriteRepoPath+"/collaborators/linkdataminion", map[string]string{"permission": "push"}, nil, nil, http.StatusCreated, http.StatusNoContent)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusCreated {
		cleanup = false // A concurrent collaborator grant belongs to another actor.
		t.Fatal("GitHub updated an existing collaborator instead of creating an invitation")
	}
	liveAcceptBotWriteInvitation(t, ctx, cfg, owner, bot, 0)
}

func liveBotWriteSetup(t *testing.T, ctx context.Context) (cfg liveTestConfig, owner, bot *Client, repository Repository) {
	t.Helper()
	cfg = loadLiveTestConfig(t)
	if cfg.Repo != "linkdata/miniontesting" || cfg.OwnerLogin != "linkdata" || cfg.BotLogin != "linkdataminion" {
		t.Fatal("write test is restricted to linkdata/miniontesting and linkdataminion")
	}
	botToken, err := liveAccountToken(ctx, cfg.BotLogin)
	if err != nil {
		t.Fatal(err)
	}
	ownerToken, err := liveAccountToken(ctx, cfg.OwnerLogin)
	if err != nil {
		t.Fatal(err)
	}
	owner, bot = New(ownerToken), New(botToken)
	for _, account := range []struct {
		client *Client
		login  string
	}{{owner, cfg.OwnerLogin}, {bot, cfg.BotLogin}} {
		user, err := account.client.AuthenticatedUser(ctx)
		if err != nil || user.Login != account.login {
			t.Fatalf("GitHub token for %s identifies %s: %v", account.login, user.Login, err)
		}
	}
	repository, err = owner.Repository(ctx, cfg.Repo)
	if err != nil || repository.FullName != cfg.Repo || repository.Archived {
		t.Fatalf("unsafe write-test repository %s: %v", repository.FullName, err)
	}
	return
}

func liveBotAccessState(t *testing.T, ctx context.Context, owner *Client, cfg liveTestConfig) (status int, pending *Invitation) {
	t.Helper()
	_, status, err := owner.request(ctx, http.MethodGet, liveWriteRepoPath+"/collaborators/linkdataminion", nil, nil, nil, http.StatusNoContent, http.StatusNotFound)
	if err != nil {
		t.Fatal(err)
	}
	invitations, err := liveOwnerInvitations(ctx, owner, liveWriteRepoPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, invitation := range invitations {
		if strings.EqualFold(invitation.Invitee.Login, cfg.BotLogin) {
			if pending != nil {
				t.Fatal("multiple pending bot invitations")
			}
			copy := invitation
			pending = &copy
		}
	}
	return
}

func liveAcceptBotWriteInvitation(t *testing.T, ctx context.Context, cfg liveTestConfig, owner, bot *Client, expectedID int64) {
	t.Helper()
	invitations, err := bot.ListRepositoryInvitations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var invitationID int64
	for _, invitation := range invitations {
		if invitation.Repository.FullName == cfg.Repo && strings.EqualFold(invitation.Invitee.Login, cfg.BotLogin) && strings.EqualFold(invitation.Inviter.Login, cfg.OwnerLogin) && invitation.Permissions == "write" && !invitation.Expired && (expectedID == 0 || invitation.ID == expectedID) {
			invitationID = invitation.ID
		}
	}
	if invitationID <= 0 {
		t.Fatal("bot cannot see a suitable repository invitation")
	}
	if err := bot.AcceptRepositoryInvitation(ctx, invitationID); err != nil {
		t.Fatal(err)
	}
	liveRequireBotWritePermission(t, ctx, cfg, owner)
}

func liveRequireBotWritePermission(t *testing.T, ctx context.Context, cfg liveTestConfig, owner *Client) {
	t.Helper()
	permission, err := owner.Permission(ctx, cfg.Repo, cfg.BotLogin)
	if err != nil || (permission.Permission != "write" && permission.Permission != "maintain" && permission.Permission != "admin") {
		t.Fatalf("bot collaborator permission = %q, want write or higher: %v", permission.Permission, err)
	}
}

func liveEnsureBotWriteAccess(t *testing.T, ctx context.Context, cfg liveTestConfig, owner, bot *Client) {
	t.Helper()
	status, pending := liveBotAccessState(t, ctx, owner, cfg)
	if status == http.StatusNoContent {
		liveRequireBotWritePermission(t, ctx, cfg, owner)
		return
	}
	if pending != nil {
		if pending.Expired {
			t.Fatal("pending bot invitation expired; cancel it before rerunning the write test")
		}
		if !strings.EqualFold(pending.Inviter.Login, cfg.OwnerLogin) || pending.Permissions != "write" {
			t.Fatal("existing bot invitation does not grant write access from the repository owner")
		}
		liveAcceptBotWriteInvitation(t, ctx, cfg, owner, bot, pending.ID)
		return
	}
	_, status, err := owner.request(ctx, http.MethodPut, liveWriteRepoPath+"/collaborators/linkdataminion", map[string]string{"permission": "push"}, nil, nil, http.StatusCreated, http.StatusNoContent)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusCreated {
		t.Fatal("GitHub updated an existing collaborator during the write-test invitation")
	}
	liveAcceptBotWriteInvitation(t, ctx, cfg, owner, bot, 0)
}

func TestEnsureBotWriteAccess(t *testing.T) {
	for _, test := range []struct {
		name       string
		permission string
		pending    bool
		wantPut    int
		wantAccept int
	}{
		{name: "existing write", permission: "write"},
		{name: "existing maintain", permission: "maintain"},
		{name: "existing admin", permission: "admin"},
		{name: "missing access", wantPut: 1, wantAccept: 1},
		{name: "pending invitation", pending: true, wantAccept: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := liveTestConfig{Repo: "linkdata/miniontesting", OwnerLogin: "linkdata", BotLogin: "linkdataminion"}
			state := struct {
				sync.Mutex
				permission string
				pending    bool
				puts       int
				accepts    int
				deletes    int
			}{permission: test.permission, pending: test.pending}
			invitation := Invitation{ID: 42, Repository: Repository{FullName: cfg.Repo}, Invitee: User{Login: cfg.BotLogin}, Inviter: User{Login: cfg.OwnerLogin}, Permissions: "write"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				state.Lock()
				defer state.Unlock()
				wantToken := "owner"
				if strings.HasPrefix(r.URL.Path, "/user/") {
					wantToken = "bot"
				}
				if r.Header.Get("Authorization") != "Bearer "+wantToken {
					t.Errorf("%s %s used wrong account", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == liveWriteRepoPath+"/collaborators/linkdataminion":
					if state.permission == "" {
						w.WriteHeader(http.StatusNotFound)
					} else {
						w.WriteHeader(http.StatusNoContent)
					}
				case r.Method == http.MethodGet && r.URL.Path == liveWriteRepoPath+"/invitations":
					var invitations []Invitation
					if state.pending {
						invitations = []Invitation{invitation}
					}
					if err := json.NewEncoder(w).Encode(invitations); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodGet && r.URL.Path == liveWriteRepoPath+"/collaborators/linkdataminion/permission":
					if err := json.NewEncoder(w).Encode(Permission{Permission: state.permission}); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodPut && r.URL.Path == liveWriteRepoPath+"/collaborators/linkdataminion":
					state.puts++
					var input struct {
						Permission string `json:"permission"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Permission != "push" {
						t.Errorf("invitation permission = %q: %v", input.Permission, err)
					}
					state.pending = true
					w.WriteHeader(http.StatusCreated)
				case r.Method == http.MethodGet && r.URL.Path == "/user/repository_invitations":
					var invitations []Invitation
					if state.pending {
						invitations = []Invitation{invitation}
					}
					if err := json.NewEncoder(w).Encode(invitations); err != nil {
						t.Error(err)
					}
				case r.Method == http.MethodPatch && r.URL.Path == "/user/repository_invitations/42":
					state.accepts++
					state.pending = false
					state.permission = "write"
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodDelete:
					state.deletes++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			owner, bot := New("owner"), New("bot")
			owner.BaseURL, bot.BaseURL = server.URL, server.URL
			liveEnsureBotWriteAccess(t, context.Background(), cfg, owner, bot)
			state.Lock()
			defer state.Unlock()
			if state.puts != test.wantPut || state.accepts != test.wantAccept || state.deletes != 0 || state.permission == "" || state.pending {
				t.Fatalf("access state: puts=%d accepts=%d deletes=%d permission=%q pending=%t", state.puts, state.accepts, state.deletes, state.permission, state.pending)
			}
		})
	}
}

func TestLiveBotWriteLifecycle(t *testing.T) {
	if os.Getenv("MINION_LIVE_WRITE") != "1" {
		t.Skip("set MINION_LIVE_WRITE=1 to enable GitHub writes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg, owner, bot, repository := liveBotWriteSetup(t, ctx)
	if cfg.IssueNumber <= 0 {
		t.Fatal("write test requires an owner-authored issue_number fixture")
	}
	issue, err := owner.IssueDetails(ctx, cfg.Repo, cfg.IssueNumber)
	if err != nil || issue.User.Login != cfg.OwnerLogin {
		t.Fatalf("issue #%d is not owner-authored: %v", cfg.IssueNumber, err)
	}
	reviewEnabled := os.Getenv("MINION_LIVE_REVIEW") == "1"
	var pull PullRequest
	if reviewEnabled {
		if cfg.PullRequestNumber <= 0 {
			t.Fatal("MINION_LIVE_REVIEW requires an owner-authored pull_request_number fixture")
		}
		pull, err = owner.PullRequestDetails(ctx, cfg.Repo, cfg.PullRequestNumber)
		if err != nil || pull.User.Login != cfg.OwnerLogin || !pull.Draft || pull.State != "open" || len(pull.Head.SHA) != 40 {
			t.Fatalf("pull request #%d is not an open owner-authored draft: %v", cfg.PullRequestNumber, err)
		}
	}

	liveEnsureBotWriteAccess(t, ctx, cfg, owner, bot)
	artifacts := &liveWriteArtifacts{issueNumber: cfg.IssueNumber, baseBranch: repository.DefaultBranch}
	t.Cleanup(func() { liveRemoveArtifacts(t, owner, cfg, artifacts) })

	body := "Minion live write test at " + time.Now().UTC().Format(time.RFC3339Nano) + "."
	artifacts.comments = append(artifacts.comments, liveCommentArtifact{author: cfg.BotLogin, body: body})
	comment, err := bot.CreateIssueComment(ctx, cfg.Repo, cfg.IssueNumber, body)
	if err != nil || comment.ID <= 0 || comment.NodeID == "" || comment.User.Login != cfg.BotLogin || comment.Body != body {
		t.Fatalf("bot issue comment was not confirmed: id=%d: %v", comment.ID, err)
	}
	artifacts.comments[0].id = comment.ID
	t.Logf("issue comment: %s", comment.HTMLURL)
	editedBody := body + " Edited by the bot."
	artifacts.comments[0].editedBody = editedBody
	var edited Comment
	_, _, err = bot.request(ctx, http.MethodPatch, liveWriteRepoPath+"/issues/comments/"+strconv.FormatInt(comment.ID, 10), map[string]string{"body": editedBody}, &edited, nil, http.StatusOK)
	if err != nil || edited.Body != editedBody {
		t.Fatalf("bot issue comment edit was not confirmed: %v", err)
	}
	current, err := bot.CurrentItem(ctx, Item{NodeID: comment.NodeID})
	if err != nil || current.Body != editedBody || !strings.EqualFold(current.Author.Login, cfg.BotLogin) || !strings.EqualFold(current.Editor.Login, cfg.BotLogin) || !current.Edited {
		t.Fatalf("edited bot comment provenance = %+v, %v", current, err)
	}

	ownerBody := "Minion live reaction fixture at " + time.Now().UTC().Format(time.RFC3339Nano) + "."
	artifacts.comments = append(artifacts.comments, liveCommentArtifact{author: cfg.OwnerLogin, body: ownerBody})
	ownerComment, err := owner.CreateIssueComment(ctx, cfg.Repo, cfg.IssueNumber, ownerBody)
	if err != nil || ownerComment.ID <= 0 || ownerComment.NodeID == "" || ownerComment.User.Login != cfg.OwnerLogin || ownerComment.Body != ownerBody {
		t.Fatalf("owner reaction fixture was not confirmed: id=%d: %v", ownerComment.ID, err)
	}
	artifacts.comments[1].id = ownerComment.ID
	if err := bot.ReactEyes(ctx, ownerComment.NodeID); err != nil {
		t.Fatal(err)
	}

	base, err := owner.Branch(ctx, cfg.Repo, repository.DefaultBranch)
	if err != nil || len(base.Commit.SHA) != 40 {
		t.Fatalf("default branch HEAD is unavailable: %v", err)
	}
	if _, err := hex.DecodeString(base.Commit.SHA); err != nil {
		t.Fatalf("default branch HEAD is invalid: %v", err)
	}
	id := strconv.FormatInt(comment.ID, 10)
	tag := "minion-live-" + id
	artifacts.tag = tag
	artifacts.tagSHA = base.Commit.SHA
	if err := bot.CreateTagReference(ctx, cfg.Repo, tag, base.Commit.SHA); err != nil {
		t.Fatal(err)
	}
	ref, err := bot.TagReference(ctx, cfg.Repo, tag)
	if err != nil || ref.Ref != "refs/tags/"+tag || ref.Object.SHA != base.Commit.SHA {
		t.Fatalf("created tag %s does not target default branch HEAD: %v", tag, err)
	}
	t.Logf("tag: https://github.com/%s/tree/%s", cfg.Repo, tag)
	artifacts.releaseAttempted = true
	release, err := bot.CreateDraftRelease(ctx, cfg.Repo, tag, base.Commit.SHA, "Minion live test "+id, "Draft release created by the Minion live write test.")
	if err != nil || release.ID <= 0 || !release.Draft || release.TagName != tag || release.Author.Login != cfg.BotLogin {
		t.Fatalf("draft release for %s was not confirmed: id=%d: %v", tag, release.ID, err)
	}
	artifacts.releaseID = release.ID
	t.Logf("draft release: %s (id=%d)", release.HTMLURL, release.ID)
	var readBack Release
	for attempt := 0; attempt < 20; attempt++ {
		readBack, err = bot.ReleaseByTag(ctx, cfg.Repo, tag)
		if err == nil {
			break
		}
		if !liveNotFound(err) || attempt == 19 {
			t.Fatalf("draft release for %s cannot be read back: %v", tag, err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
	if readBack.ID != release.ID || !readBack.Draft {
		t.Fatalf("draft release for %s has unexpected ID or state", tag)
	}
	if cfg.BotSSHKey != "" {
		livePushDraftPullRequest(t, ctx, cfg, bot, artifacts, id)
	}
	if reviewEnabled {
		review, err := bot.CreatePullRequestReview(ctx, cfg.Repo, cfg.PullRequestNumber, "Minion live write test: comment review only.", pull.Head.SHA)
		if err != nil || review.ID <= 0 || review.User.Login != cfg.BotLogin || review.State != "COMMENTED" {
			t.Fatalf("bot comment review was not confirmed: id=%d state=%q: %v", review.ID, review.State, err)
		}
		t.Logf("submitted comment review remains: %s", review.HTMLURL)
	}
}

type liveWriteArtifacts struct {
	issueNumber      int
	comments         []liveCommentArtifact
	tag              string
	tagSHA           string
	releaseAttempted bool
	releaseID        int64
	baseBranch       string
	branch           string
	branchSHA        string
	pullAttempted    bool
	pullNumber       int
}

type liveCommentArtifact struct {
	id         int64
	author     string
	body       string
	editedBody string
}

func (artifact liveCommentArtifact) matches(body string) bool {
	return artifact.body != "" && (body == artifact.body || artifact.editedBody != "" && body == artifact.editedBody)
}

func livePushDraftPullRequest(t *testing.T, ctx context.Context, cfg liveTestConfig, bot *Client, artifacts *liveWriteArtifacts, id string) {
	t.Helper()
	key := liveBotSSHKeyPath(t, cfg.BotSSHKey)
	work := t.TempDir()
	checkout := filepath.Join(work, "repo")
	remote := "git@github.com:linkdata/miniontesting.git"
	liveWriteGit(t, ctx, work, key, "clone", "--depth=1", "--single-branch", "--branch", artifacts.baseBranch, "--", remote, checkout)
	branch := "minion/live-" + id
	artifacts.branch = branch
	liveWriteGit(t, ctx, checkout, key, "checkout", "-b", branch)
	name := "minion-live-" + id + ".txt"
	if err := os.WriteFile(filepath.Join(checkout, name), []byte("Minion live write test fixture "+id+".\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	liveWriteGit(t, ctx, checkout, key, "add", "--", name)
	liveWriteGit(t, ctx, checkout, key, "-c", "user.name=linkdataminion", "-c", "user.email=linkdataminion@users.noreply.github.com", "-c", "commit.gpgsign=false", "commit", "-m", "test: add live fixture "+id)
	head := liveWriteGit(t, ctx, checkout, key, "rev-parse", "HEAD")
	if len(head) != 40 {
		t.Fatalf("fixture commit has invalid SHA %q", head)
	}
	artifacts.branchSHA = head
	liveWriteGit(t, ctx, checkout, key, "push", "--no-verify", remote, "HEAD:refs/heads/"+branch)
	remoteBranch, err := bot.Branch(ctx, cfg.Repo, branch)
	if err != nil || remoteBranch.Commit.SHA != head {
		t.Fatalf("pushed fixture branch %s does not match local commit: %v", branch, err)
	}
	artifacts.pullAttempted = true
	pull, err := bot.CreatePullRequest(ctx, cfg.Repo, artifacts.baseBranch, branch, "Minion live test "+id, "Draft pull request created by the Minion live write test.", true)
	if err != nil || pull.ID <= 0 || !pull.Draft || pull.User.Login != cfg.BotLogin || pull.Head.Ref != branch || pull.Base.Ref != artifacts.baseBranch {
		t.Fatalf("draft pull request from %s was not confirmed: id=%d: %v", branch, pull.ID, err)
	}
	artifacts.pullNumber = pull.Number
	readBack, err := bot.PullRequestDetails(ctx, cfg.Repo, pull.Number)
	if err != nil || readBack.ID != pull.ID || !readBack.Draft || readBack.Head.SHA != head {
		t.Fatalf("draft pull request #%d cannot be read back: %v", pull.Number, err)
	}
	t.Logf("fixture branch: https://github.com/%s/tree/%s", cfg.Repo, branch)
	t.Logf("draft pull request: %s", pull.HTMLURL)
}

func liveWriteGit(t *testing.T, ctx context.Context, dir, key string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=" + os.DevNull}, args...)...)
	cmd.Dir = dir
	cmd.Env = liveBotGitEnv(key)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output))
}

func liveRemoveArtifacts(t *testing.T, owner *Client, cfg liveTestConfig, artifacts *liveWriteArtifacts) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const path = "/repos/linkdata/miniontesting"

	if artifacts.branch != "" {
		var pull *PullRequest
		canDeleteBranch := true
		if artifacts.pullNumber > 0 {
			found, err := owner.PullRequestDetails(ctx, cfg.Repo, artifacts.pullNumber)
			if err != nil {
				t.Errorf("find test pull request for cleanup: %v", err)
				canDeleteBranch = false
			} else {
				pull = &found
			}
		} else {
			found, err := owner.FindPullRequestByHead(ctx, cfg.Repo, artifacts.baseBranch, artifacts.branch)
			if err != nil {
				t.Errorf("find test pull request by head for cleanup: %v", err)
				canDeleteBranch = false
			} else {
				pull = found
			}
		}
		if pull == nil && artifacts.pullAttempted {
			t.Errorf("test pull request for branch %s could not be found; leaving branch intact", artifacts.branch)
			canDeleteBranch = false
		}
		if pull != nil {
			if pull.User.Login != cfg.BotLogin || pull.Head.Ref != artifacts.branch || pull.Base.Ref != artifacts.baseBranch {
				t.Errorf("refuse to alter pull request #%d with unexpected identity", pull.Number)
				canDeleteBranch = false
			} else if pull.State == "open" {
				var closed PullRequest
				_, _, err := owner.request(ctx, http.MethodPatch, path+"/pulls/"+strconv.Itoa(pull.Number), map[string]string{"state": "closed"}, &closed, nil, http.StatusOK)
				if err != nil || closed.State != "closed" {
					t.Errorf("close test pull request #%d: %v", pull.Number, err)
					canDeleteBranch = false
				} else {
					t.Logf("closed draft pull request remains: %s", pull.HTMLURL)
				}
			}
		}
		if canDeleteBranch && artifacts.branchSHA != "" {
			branch, err := owner.Branch(ctx, cfg.Repo, artifacts.branch)
			if err != nil && !liveNotFound(err) {
				t.Errorf("find test branch for cleanup: %v", err)
			} else if err == nil {
				if branch.Commit.SHA != artifacts.branchSHA {
					t.Errorf("refuse to delete changed test branch %s", artifacts.branch)
				} else if _, _, err := owner.request(ctx, http.MethodDelete, path+"/git/refs/heads/"+artifacts.branch, nil, nil, nil, http.StatusNoContent, http.StatusNotFound); err != nil {
					t.Errorf("delete test branch %s: %v", artifacts.branch, err)
				} else if _, err := owner.Branch(ctx, cfg.Repo, artifacts.branch); !liveNotFound(err) {
					t.Errorf("test branch %s remains after cleanup: %v", artifacts.branch, err)
				}
			}
		}
	}

	if artifacts.tag != "" {
		var release Release
		var err error
		if artifacts.releaseID > 0 {
			_, _, err = owner.request(ctx, http.MethodGet, path+"/releases/"+strconv.FormatInt(artifacts.releaseID, 10), nil, &release, nil, http.StatusOK)
			if liveNotFound(err) {
				release, err = owner.ReleaseByTag(ctx, cfg.Repo, artifacts.tag)
			}
		} else {
			release, err = owner.ReleaseByTag(ctx, cfg.Repo, artifacts.tag)
		}
		canDeleteTag := true
		if err != nil && !liveNotFound(err) {
			t.Errorf("find test release for cleanup: %v", err)
			canDeleteTag = false
		} else if liveNotFound(err) && artifacts.releaseAttempted {
			t.Errorf("draft test release %s is not visible; leaving tag intact", artifacts.tag)
			canDeleteTag = false
		} else if err == nil {
			if release.TagName != artifacts.tag || !release.Draft || release.Author.Login != cfg.BotLogin {
				t.Errorf("refuse to delete changed test release %s", artifacts.tag)
				canDeleteTag = false
			} else if _, _, err := owner.request(ctx, http.MethodDelete, path+"/releases/"+strconv.FormatInt(release.ID, 10), nil, nil, nil, http.StatusNoContent, http.StatusNotFound); err != nil {
				t.Errorf("delete draft test release %s: %v", artifacts.tag, err)
				canDeleteTag = false
			} else {
				_, status, err := owner.request(ctx, http.MethodGet, path+"/releases/"+strconv.FormatInt(release.ID, 10), nil, nil, nil, http.StatusOK, http.StatusNotFound)
				if err != nil || status != http.StatusNotFound {
					t.Errorf("draft test release %s remains after cleanup: HTTP %d: %v", artifacts.tag, status, err)
					canDeleteTag = false
				}
			}
		}
		if canDeleteTag {
			ref, err := owner.TagReference(ctx, cfg.Repo, artifacts.tag)
			if err != nil && !liveNotFound(err) {
				t.Errorf("find test tag for cleanup: %v", err)
			} else if err == nil {
				if ref.Ref != "refs/tags/"+artifacts.tag || ref.Object.SHA != artifacts.tagSHA {
					t.Errorf("refuse to delete changed test tag %s", artifacts.tag)
				} else if _, _, err := owner.request(ctx, http.MethodDelete, path+"/git/refs/tags/"+artifacts.tag, nil, nil, nil, http.StatusNoContent, http.StatusNotFound); err != nil {
					t.Errorf("delete test tag %s: %v", artifacts.tag, err)
				} else if _, err := owner.TagReference(ctx, cfg.Repo, artifacts.tag); !liveNotFound(err) {
					t.Errorf("test tag %s remains after cleanup: %v", artifacts.tag, err)
				}
			}
		}
	}

	for _, comment := range artifacts.comments {
		liveRemoveComment(t, ctx, owner, cfg, path, artifacts.issueNumber, comment)
	}
}

func liveRemoveComment(t *testing.T, ctx context.Context, owner *Client, cfg liveTestConfig, path string, issueNumber int, artifact liveCommentArtifact) {
	t.Helper()
	commentID := artifact.id
	if commentID == 0 {
		items, err := owner.ThreadItems(ctx, cfg.Repo, "Issue", issueNumber, time.Time{})
		if err != nil {
			t.Errorf("find test comment for cleanup: %v", err)
		} else {
			for _, item := range items {
				if artifact.matches(item.Body) && strings.EqualFold(item.Author.Login, artifact.author) {
					if suffix, ok := strings.CutPrefix(item.Key, strings.ToLower(cfg.Repo)+"#issue-comment:"); ok {
						if id, err := strconv.ParseInt(suffix, 10, 64); err == nil {
							commentID = id
						}
					}
				}
			}
		}
	}
	if commentID <= 0 {
		return
	}
	commentPath := path + "/issues/comments/" + strconv.FormatInt(commentID, 10)
	var comment Comment
	_, status, err := owner.request(ctx, http.MethodGet, commentPath, nil, &comment, nil, http.StatusOK, http.StatusNotFound)
	if err != nil {
		t.Errorf("read test comment for cleanup: %v", err)
	} else if status == http.StatusOK {
		if !strings.EqualFold(comment.User.Login, artifact.author) || !artifact.matches(comment.Body) {
			t.Errorf("refuse to delete changed test comment %d", commentID)
		} else if _, _, err := owner.request(ctx, http.MethodDelete, commentPath, nil, nil, nil, http.StatusNoContent, http.StatusNotFound); err != nil {
			t.Errorf("delete test comment and reaction %d: %v", commentID, err)
		} else if _, status, err := owner.request(ctx, http.MethodGet, commentPath, nil, nil, nil, http.StatusOK, http.StatusNotFound); err != nil || status != http.StatusNotFound {
			t.Errorf("test comment %d remains after cleanup: HTTP %d: %v", commentID, status, err)
		}
	}
}

func liveNotFound(err error) bool {
	var status *StatusError
	return errors.As(err, &status) && status.StatusCode == http.StatusNotFound
}

func liveOwnerInvitations(ctx context.Context, owner *Client, path string) ([]Invitation, error) {
	query := url.Values{"per_page": {"100"}}
	var invitations []Invitation
	for page := 1; page <= 1000; page++ {
		query.Set("page", strconv.Itoa(page))
		var batch []Invitation
		header, _, err := owner.request(ctx, http.MethodGet, withQuery(path+"/invitations", query), nil, &batch, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		invitations = append(invitations, batch...)
		if !hasNextPage(header.Get("Link")) {
			return invitations, nil
		}
	}
	return nil, fmt.Errorf("too many invitations for %s", path)
}

func liveRevokeTestBot(t *testing.T, owner *Client, path, collaboratorPath, botLogin string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	invitations, err := liveOwnerInvitations(ctx, owner, path)
	if err != nil {
		t.Errorf("list pending test invitations for cleanup: %v", err)
	} else {
		for _, invitation := range invitations {
			if strings.EqualFold(invitation.Invitee.Login, botLogin) {
				_, _, err := owner.request(ctx, http.MethodDelete, path+"/invitations/"+strconv.FormatInt(invitation.ID, 10), nil, nil, nil, http.StatusNoContent, http.StatusNotFound)
				if err != nil {
					t.Errorf("cancel pending test invitation: %v", err)
				}
			}
		}
	}
	if _, _, err := owner.request(ctx, http.MethodDelete, collaboratorPath, nil, nil, nil, http.StatusNoContent, http.StatusNotFound); err != nil {
		t.Errorf("revoke test-granted collaborator: %v", err)
	}
	_, status, err := owner.request(ctx, http.MethodGet, collaboratorPath, nil, nil, nil, http.StatusNoContent, http.StatusNotFound)
	if err != nil || status != http.StatusNotFound {
		t.Errorf("bot collaborator access remains after cleanup: HTTP %d: %v", status, err)
	}
	invitations, err = liveOwnerInvitations(ctx, owner, path)
	if err != nil {
		t.Errorf("verify pending test invitations removed: %v", err)
	} else {
		for _, invitation := range invitations {
			if strings.EqualFold(invitation.Invitee.Login, botLogin) {
				t.Errorf("pending bot invitation remains after cleanup: %d", invitation.ID)
			}
		}
	}
}
