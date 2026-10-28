package action

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/state"
)

const (
	liveFixRepository = "linkdata/miniontesting"
	liveFixFork       = "linkdataminion/miniontesting"
)

type liveFixConfig struct {
	OwnerLogin        string `json:"owner_login"`
	BotLogin          string `json:"bot_login"`
	RepositoryURL     string `json:"repository_url"`
	IssueNumber       int    `json:"issue_number"`
	PullRequestNumber int    `json:"pull_request_number"`
}

type liveActionFixture struct {
	cfg        liveFixConfig
	ownerToken string
	botToken   string
	owner      *github.Client
	bot        *github.Client
	repo       github.Repository
	ownerUser  github.User
	botUser    github.User
}

func TestLiveFixPublishesBranch(t *testing.T) {
	if os.Getenv("MINION_LIVE_WRITE") != "1" {
		t.Skip("set MINION_LIVE_WRITE=1 to test a live fix branch")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	fixture := liveActionSetup(t, ctx)
	installActionTestSkills(t, testFixSkillURL)
	cfg, owner, bot, repo := fixture.cfg, fixture.owner, fixture.bot, fixture.repo
	if cfg.IssueNumber <= 0 {
		t.Fatal("live fix test requires an issue_number")
	}
	issue, err := owner.IssueDetails(ctx, liveFixRepository, cfg.IssueNumber)
	if err != nil || !strings.EqualFold(issue.User.Login, cfg.OwnerLogin) || issue.NodeID == "" {
		t.Fatalf("issue #%d is not an owner-authored fixture: %v", cfg.IssueNumber, err)
	}

	base, err := owner.Branch(ctx, liveFixRepository, repo.DefaultBranch)
	if err != nil || len(base.Commit.SHA) != 40 {
		t.Fatalf("default branch HEAD is unavailable: %v", err)
	}

	var randomID [32]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(randomID[:])
	branch := fmt.Sprintf("minion/fix-%d-%s", cfg.IssueNumber, id[:12])
	if _, err := bot.Branch(ctx, liveFixFork, branch); !liveFixNotFound(err) {
		t.Fatalf("test branch %s already exists or cannot be checked: %v", branch, err)
	}
	if pull, err := owner.FindPullRequestByHead(ctx, liveFixRepository, repo.DefaultBranch, cfg.BotLogin+":"+branch); err != nil || pull != nil {
		t.Fatalf("test branch %s has an existing pull request or lookup failed: %v", branch, err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	job := monitor.Job{
		Repository:  config.Repository{Name: liveFixRepository, ID: repo.ID, OwnerID: repo.Owner.ID, Actions: []string{"fix"}},
		SubjectType: "Issue",
		Number:      cfg.IssueNumber,
		ID:          id,
		Source: github.Item{
			Body:      "@linkdataminion fix",
			Author:    fixture.ownerUser,
			UpdatedAt: time.Now(),
			HTMLURL:   issue.HTMLURL,
		},
		Command: command.Command{Name: "fix", Action: command.Fix},
	}
	t.Cleanup(func() { liveFixCleanup(t, owner, bot, fixture.ownerToken, fixture.botToken, cfg, job, branch, store) })
	t.Setenv("GH_TOKEN", fixture.botToken)
	modelCalls := 0
	orchestrator := &Orchestrator{WorkRoot: t.TempDir(), GitHub: bot,
		BotLogin: cfg.BotLogin,
		Store:    store,
		Commands: map[string]config.CommandMapping{"fix": {Action: "fix", Skills: []string{testFixSkillURL}}},
		Runner: Runner{
			runModel: func(_ context.Context, modelRequest modelRun) (aicli.Result, error) {
				dir, writable, outputDir := modelRequest.dir, modelRequest.writable, modelRequest.outputDir
				modelCalls++
				if !writable || outputDir != "" {
					return aicli.Result{}, errors.New("unexpected model invocation for live fix")
				}
				assertModelRepository(t, dir, base.Commit.SHA)
				name := "minion-live-fix-" + id[:12] + ".txt"
				err := os.WriteFile(filepath.Join(dir, name), []byte("Minion live fix test fixture.\n"), 0o600)
				return aicli.Result{Answer: "Live fix fixture created."}, err
			},
		},
	}
	if err := orchestrator.CheckFixFork(ctx, job.Repository); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Execute(ctx, job); err != nil {
		t.Fatal(err)
	}
	if modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1", modelCalls)
	}
	sha := store.Data.PendingFixes[job.ID].SHA
	if len(sha) != 40 {
		t.Fatalf("fix did not record a commit SHA: %q", sha)
	}
	remote, err := bot.Branch(ctx, liveFixFork, branch)
	if err != nil || remote.Name != branch || remote.Commit.SHA != sha {
		t.Fatalf("live fix branch does not match recorded SHA: %v", err)
	}
	var pushedFile struct{ Type, Encoding, Content string }
	status, err := liveFixAPIRepo(ctx, fixture.botToken, liveFixFork, http.MethodGet, "/contents/minion-live-fix-"+id[:12]+".txt?ref="+sha, &pushedFile)
	if err != nil || status != http.StatusOK || pushedFile.Type != "file" || pushedFile.Encoding != "base64" {
		t.Fatalf("read pushed fix bytes: HTTP%d,%v", status, err)
	}
	pushedBytes, err := base64.StdEncoding.DecodeString(pushedFile.Content)
	if err != nil || string(pushedBytes) != "Minion live fix test fixture.\n" {
		t.Fatalf("pushed fixture bytes differ: %q,%v", pushedBytes, err)
	}
	if _, err := owner.Branch(ctx, liveFixRepository, branch); !liveFixNotFound(err) {
		t.Fatalf("base repository unexpectedly received a fix branch: %v", err)
	}
	commentID, reply, err := liveActionReply(ctx, owner, cfg, job)
	if err != nil || commentID == 0 || !strings.Contains(reply.Body, liveFixFork) || !strings.Contains(reply.Body, branch) || !strings.Contains(reply.Body, sha) {
		t.Fatalf("live fix acknowledgement lacks branch or recorded SHA: %v", err)
	}
	if pull, err := owner.FindPullRequestByHead(ctx, liveFixRepository, repo.DefaultBranch, cfg.BotLogin+":"+branch); err != nil || pull != nil {
		t.Fatalf("fix unexpectedly opened a pull request: %v", err)
	}
	if err := orchestrator.Execute(ctx, job); err != nil {
		t.Fatal(err)
	}
	if modelCalls != 1 || store.Data.PendingFixes[job.ID].SHA != sha {
		t.Fatalf("repeat execution changed the fix: model calls = %d, recorded SHA = %q", modelCalls, store.Data.PendingFixes[job.ID].SHA)
	}
	if repeatID, _, err := liveActionReply(ctx, owner, cfg, job); err != nil || repeatID != commentID {
		t.Fatalf("repeat execution changed the acknowledgement: id = %d, want %d: %v", repeatID, commentID, err)
	}
}

func liveActionSetup(t *testing.T, ctx context.Context) liveActionFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "live-tests.local.json"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("live-tests.local.json is absent")
	}
	if err != nil {
		t.Fatal(err)
	}
	var cfg liveFixConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.OwnerLogin != "linkdata" || cfg.BotLogin != "linkdataminion" || cfg.RepositoryURL != "https://github.com/"+liveFixRepository {
		t.Fatal("live action test requires linkdata, linkdataminion, and linkdata/miniontesting")
	}
	ownerToken, err := liveFixToken(ctx, cfg.OwnerLogin)
	if err != nil {
		t.Fatal(err)
	}
	botToken, err := liveFixToken(ctx, cfg.BotLogin)
	if err != nil {
		t.Fatal(err)
	}
	owner, bot := github.New(ownerToken), github.New(botToken)
	ownerUser, err := owner.AuthenticatedUser(ctx)
	if err != nil || ownerUser.Login != cfg.OwnerLogin || ownerUser.ID <= 0 {
		t.Fatalf("owner credential does not identify %s: %v", cfg.OwnerLogin, err)
	}
	botUser, err := bot.AuthenticatedUser(ctx)
	if err != nil || botUser.Login != cfg.BotLogin || botUser.ID <= 0 {
		t.Fatalf("bot credential does not identify %s: %v", cfg.BotLogin, err)
	}
	repo, err := owner.Repository(ctx, liveFixRepository)
	if err != nil || repo.FullName != liveFixRepository || repo.Archived || repo.DefaultBranch == "" {
		t.Fatalf("unsafe live fix repository: %v", err)
	}
	permission, err := owner.Permission(ctx, liveFixRepository, cfg.BotLogin)
	if err != nil {
		t.Fatal(err)
	}
	if permission.Permission != "write" && permission.Permission != "maintain" && permission.Permission != "admin" {
		t.Fatal("bot needs write access; run the ordinary live write test first")
	}
	return liveActionFixture{cfg: cfg, ownerToken: ownerToken, botToken: botToken, owner: owner, bot: bot, repo: repo, ownerUser: ownerUser, botUser: botUser}
}

func liveFixToken(ctx context.Context, login string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com", "--user", login)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GH_TOKEN=") && !strings.HasPrefix(entry, "GITHUB_TOKEN=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GH_PROMPT_DISABLED=1")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token for %s: %w", login, err)
	}
	if token := strings.TrimSpace(string(output)); token != "" {
		return token, nil
	}
	return "", fmt.Errorf("gh auth token for %s is empty", login)
}

func liveActionReply(ctx context.Context, owner *github.Client, cfg liveFixConfig, job monitor.Job) (id int64, reply github.Item, err error) {
	items, err := owner.ThreadItems(ctx, liveFixRepository, job.SubjectType, job.Number, time.Time{})
	if err != nil {
		return 0, github.Item{}, err
	}
	prefix := liveFixRepository + "#issue-comment:"
	for _, item := range items {
		if !strings.EqualFold(item.Author.Login, cfg.BotLogin) || !strings.HasSuffix(strings.TrimSpace(item.Body), "\n\n"+job.Marker()) {
			continue
		}
		value, ok := strings.CutPrefix(item.Key, prefix)
		if !ok {
			return 0, github.Item{}, fmt.Errorf("live action reply has unexpected key %q", item.Key)
		}
		commentID, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || commentID <= 0 || id != 0 {
			return 0, github.Item{}, errors.New("live action reply has an invalid or duplicate comment ID")
		}
		id, reply = commentID, item
	}
	return id, reply, nil
}

func liveFixCleanup(t *testing.T, owner, bot *github.Client, ownerToken, botToken string, cfg liveFixConfig, job monitor.Job, branch string, store *state.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
	defer cancel()
	liveActionReplyCleanup(t, ctx, owner, ownerToken, cfg, job)

	remote, err := bot.Branch(ctx, liveFixFork, branch)
	if liveFixNotFound(err) {
		return
	}
	if err != nil {
		t.Errorf("find live fix branch for cleanup: %v", err)
		return
	}
	sha := store.Data.PendingFixes[job.ID].SHA
	if len(sha) != 40 || remote.Name != branch || remote.Commit.SHA != sha {
		t.Errorf("refuse to delete changed or unrecorded live fix branch %s", branch)
		return
	}
	path := "/git/refs/heads/" + branch
	if status, err := liveFixAPIRepo(ctx, botToken, liveFixFork, http.MethodDelete, path, nil); err != nil || status != http.StatusNoContent {
		t.Errorf("delete live fix branch %s: HTTP %d: %v", branch, status, err)
		return
	}
	if _, err := bot.Branch(ctx, liveFixFork, branch); !liveFixNotFound(err) {
		t.Errorf("live fix branch %s remains after cleanup: %v", branch, err)
	}
}

func liveActionReplyCleanup(t *testing.T, ctx context.Context, owner *github.Client, ownerToken string, cfg liveFixConfig, job monitor.Job) {
	t.Helper()
	commentID, reply, err := liveActionReply(ctx, owner, cfg, job)
	if err != nil {
		t.Errorf("find live action reply for cleanup: %v", err)
	} else if commentID != 0 {
		if current, err := owner.CurrentItem(ctx, reply); err != nil || current.Edited || current.Body != reply.Body ||
			!strings.EqualFold(current.Author.Login, cfg.BotLogin) {
			t.Errorf("refuse to delete edited or changed live action reply %d: %v", commentID, err)
		} else {
			liveActionDeleteReply(t, ctx, ownerToken, cfg, commentID, reply)
		}
	}
}

func liveActionDeleteReply(t *testing.T, ctx context.Context, ownerToken string, cfg liveFixConfig, commentID int64, reply github.Item) {
	t.Helper()
	var current github.Comment
	path := "/issues/comments/" + strconv.FormatInt(commentID, 10)
	status, err := liveFixAPI(ctx, ownerToken, http.MethodGet, path, &current)
	if err != nil || status != http.StatusOK {
		t.Errorf("read live action reply for cleanup: HTTP %d: %v", status, err)
	} else if current.ID != commentID || current.NodeID != reply.NodeID || !strings.EqualFold(current.User.Login, cfg.BotLogin) || current.Body != reply.Body {
		t.Errorf("refuse to delete changed live action reply %d", commentID)
	} else if status, err := liveFixAPI(ctx, ownerToken, http.MethodDelete, path, nil); err != nil || status != http.StatusNoContent {
		t.Errorf("delete live action reply %d: HTTP %d: %v", commentID, status, err)
	} else if status, err := liveFixAPI(ctx, ownerToken, http.MethodGet, path, nil); err != nil || status != http.StatusNotFound {
		t.Errorf("live action reply %d remains after cleanup: HTTP %d: %v", commentID, status, err)
	}
}

func liveFixAPI(ctx context.Context, token, method, path string, output any) (status int, err error) {
	return liveFixAPIRepo(ctx, token, liveFixRepository, method, path, output)
}

func liveFixAPIRepo(ctx context.Context, token, repo, method, path string, output any) (status int, err error) {
	request, err := http.NewRequestWithContext(ctx, method, "https://api.github.com/repos/"+repo+path, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, response.Body.Close()) }()
	if output != nil && response.StatusCode == http.StatusOK {
		err = json.NewDecoder(response.Body).Decode(output)
	}
	return response.StatusCode, err
}

func liveFixNotFound(err error) bool {
	var status *github.StatusError
	return errors.As(err, &status) && status.StatusCode == http.StatusNotFound
}
