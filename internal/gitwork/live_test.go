package gitwork

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/workspace"
)

func TestLiveCloneAndPullRequest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "live-tests.local.json"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("live-tests.local.json is absent")
	}
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		RepositoryURL     string `json:"repository_url"`
		BotLogin          string `json:"bot_login"`
		PullRequestNumber int    `json:"pull_request_number"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	const repoName = "linkdata/miniontesting"
	if cfg.RepositoryURL != "https://github.com/"+repoName {
		t.Skipf("live Git checkout uses dedicated %s repository", repoName)
	}
	if cfg.BotLogin == "" {
		t.Fatal("bot_login is absent")
	}
	if cfg.PullRequestNumber <= 0 {
		t.Skip("pull_request_number is absent")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	botToken := liveGitworkToken(t, ctx, cfg.BotLogin)
	client := github.New(botToken)
	user, err := client.AuthenticatedUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(user.Login, cfg.BotLogin) {
		t.Fatalf("%s gh credential belongs to %s", cfg.BotLogin, user.Login)
	}
	t.Setenv("GH_TOKEN", botToken)
	remote, err := client.Repository(ctx, repoName)
	if err != nil {
		t.Fatal(err)
	}
	if remote.DefaultBranch == "" {
		t.Fatal("test repository has no default branch")
	}
	pull, err := client.PullRequestDetails(ctx, repoName, cfg.PullRequestNumber)
	if err != nil {
		t.Fatal(err)
	}
	if pull.Number != cfg.PullRequestNumber || pull.Base.Ref == "" || len(pull.Head.SHA) != 40 {
		t.Fatalf("pull request metadata = %+v", pull)
	}

	defaultCheckout, err := Clone(ctx, t.TempDir(), repoName, remote.DefaultBranch, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := defaultCheckout.Close(); err != nil {
			t.Error(err)
		}
	}()
	if head, err := defaultCheckout.verifiedHead(ctx); err != nil || !workspace.ValidSHA(head) {
		t.Fatalf("cloned default-branch HEAD = %q, %v", head, err)
	}

	checkout, err := ClonePullRequest(ctx, t.TempDir(), repoName, pull.Number, pull.Base.Ref, pull.Head.SHA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	base, err := runGit(ctx, checkout.path, "rev-parse", "--verify", "refs/remotes/origin/"+pull.Base.Ref+"^{commit}")
	if err != nil || !workspace.ValidSHA(base) {
		t.Fatalf("captured pull request base ref is unavailable: %q, %v", base, err)
	}
	if head, err := checkout.verifiedHead(ctx); err != nil || head != pull.Head.SHA {
		t.Fatalf("pull request HEAD = %q, %v; want %s", head, err, pull.Head.SHA)
	}
	snapshot, err := checkout.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Head != pull.Head.SHA || snapshot.Base != base || !workspace.ValidSHA(snapshot.MergeBase) {
		t.Fatalf("pull request snapshot identities = head:%q base:%q merge-base:%q", snapshot.Head, snapshot.Base, snapshot.MergeBase)
	}
	materialized, err := MaterializeRepository(ctx, repositorySource(t, snapshot.Files), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialized.Verify(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLiveCommitAndPush(t *testing.T) {
	if os.Getenv("MINION_LIVE_WRITE") != "1" {
		t.Skip("set MINION_LIVE_WRITE=1 to test Gitwork publishing")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "live-tests.local.json"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("live-tests.local.json is absent")
	}
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		OwnerLogin    string `json:"owner_login"`
		BotLogin      string `json:"bot_login"`
		RepositoryURL string `json:"repository_url"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	const repoName = "linkdata/miniontesting"
	const forkName = "linkdataminion/miniontesting"
	if cfg.RepositoryURL != "https://github.com/"+repoName || cfg.OwnerLogin != "linkdata" || cfg.BotLogin != "linkdataminion" {
		t.Fatal("live Gitwork write test is restricted to linkdata/miniontesting and linkdataminion")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ownerToken, botToken := liveGitworkToken(t, ctx, cfg.OwnerLogin), liveGitworkToken(t, ctx, cfg.BotLogin)
	owner, bot := github.New(ownerToken), github.New(botToken)
	for _, account := range []struct {
		client *github.Client
		login  string
	}{{owner, cfg.OwnerLogin}, {bot, cfg.BotLogin}} {
		user, err := account.client.AuthenticatedUser(ctx)
		if err != nil || !strings.EqualFold(user.Login, account.login) {
			t.Fatalf("GitHub token for %s identifies %s: %v", account.login, user.Login, err)
		}
	}
	repo, err := owner.Repository(ctx, repoName)
	if err != nil || repo.FullName != repoName || repo.Archived || repo.DefaultBranch == "" {
		t.Fatalf("unsafe write-test repository %s: %v", repo.FullName, err)
	}
	fork, err := bot.Repository(ctx, forkName)
	if err != nil || !fork.Fork || fork.Parent == nil || fork.Parent.ID != repo.ID || fork.Archived {
		t.Fatalf("unsafe write-test fork %s: %v", forkName, err)
	}
	if enabled, err := bot.ActionsEnabled(ctx, forkName); err != nil || enabled {
		t.Fatalf("write-test fork Actions must be disabled: enabled=%t: %v", enabled, err)
	}
	permission, err := owner.Permission(ctx, repoName, cfg.BotLogin)
	if err != nil || (permission.Permission != "write" && permission.Permission != "maintain" && permission.Permission != "admin") {
		t.Fatalf("%s needs write access to %s; run TestLiveBotWriteLifecycle first if invitation is needed: %v", cfg.BotLogin, repoName, err)
	}

	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	branch := "minion/live-gitwork-" + id
	t.Logf("test branch: https://github.com/%s/tree/%s", forkName, branch)
	if _, err := bot.Branch(ctx, forkName, branch); !liveGitworkNotFound(err) {
		t.Fatalf("test branch already exists or cannot be checked: %v", err)
	}
	var recordedHead string
	t.Cleanup(func() {
		if recordedHead == "" {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		remote, err := bot.Branch(cleanupCtx, forkName, branch)
		if liveGitworkNotFound(err) {
			return
		}
		if err != nil || remote.Commit.SHA != recordedHead {
			t.Errorf("refuse to delete test branch %s at unexpected HEAD: %v", branch, err)
			return
		}
		cmd := exec.CommandContext(cleanupCtx, "gh", "api", "--hostname", "github.com", "--method", "DELETE", "repos/"+forkName+"/git/refs/heads/"+branch)
		cmd.Env = append(liveGitworkEnv(), "GH_TOKEN="+botToken)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("delete test branch %s: %v: %s", branch, err, strings.TrimSpace(string(output)))
			return
		}
		if _, err := bot.Branch(cleanupCtx, forkName, branch); !liveGitworkNotFound(err) {
			t.Errorf("test branch %s remains after cleanup: %v", branch, err)
		}
	})
	t.Setenv("GH_TOKEN", botToken)
	checkout, err := Clone(ctx, t.TempDir(), repoName, repo.DefaultBranch, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	})
	base, err := checkout.verifiedHead(ctx)
	if err != nil || len(base) != 40 {
		t.Fatalf("cloned default-branch HEAD = %q: %v", base, err)
	}
	if err := checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if err := checkout.CommitAndPush(ctx, forkName, branch, cfg.BotLogin, "test: unchanged branch", func(string) error { t.Fatal("unchanged tree reached push"); return nil }); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("unchanged branch=%v", err)
	}
	name := "minion-live-gitwork-" + id + ".txt"
	if err := checkout.Import(ctx, base, []workspace.Change{{Path: name, Mode: workspace.Regular, Data: []byte("Minion live Gitwork test.\n")}}); err != nil {
		t.Fatal(err)
	}
	if err := checkout.CommitAndPush(ctx, forkName, branch, cfg.BotLogin, "test: Gitwork live publish "+id, func(sha string) error {
		recordedHead = sha
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if head, err := checkout.verifiedHead(ctx); err != nil || head != recordedHead || head == base {
		t.Fatalf("committed HEAD = %q, recorded = %q, base = %q: %v", head, recordedHead, base, err)
	}
	if err := checkout.CommitAndPush(ctx, forkName, branch, cfg.BotLogin, "test: unchanged branch", func(string) error { t.Fatal("unchanged committed tree reached push"); return nil }); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("committed branch=%v", err)
	}
	remote, err := bot.Branch(ctx, forkName, branch)
	if err != nil || remote.Commit.SHA != recordedHead {
		t.Fatalf("pushed branch HEAD = %q, recorded = %q: %v", remote.Commit.SHA, recordedHead, err)
	}
	if _, err := owner.Branch(ctx, repoName, branch); !liveGitworkNotFound(err) {
		t.Fatalf("base repository unexpectedly received a test branch: %v", err)
	}
	identity, err := runGit(ctx, checkout.path, "show", "-s", "--format=%an <%ae>|%cn <%ce>", "HEAD")
	wantIdentity := cfg.BotLogin + " <" + cfg.BotLogin + "@users.noreply.github.com>"
	if err != nil || identity != wantIdentity+"|"+wantIdentity {
		t.Fatalf("committed identity = %q, want %q: %v", identity, wantIdentity+"|"+wantIdentity, err)
	}
}

func liveGitworkToken(t *testing.T, ctx context.Context, login string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com", "--user", login)
	cmd.Env = liveGitworkEnv()
	output, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(output)) == "" {
		t.Fatalf("%s gh credential unavailable: %v", login, err)
	}
	return strings.TrimSpace(string(output))
}

func liveGitworkEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GH_TOKEN=") && !strings.HasPrefix(entry, "GITHUB_TOKEN=") {
			env = append(env, entry)
		}
	}
	return append(env, "GH_PROMPT_DISABLED=1")
}

func liveGitworkNotFound(err error) bool {
	var status *github.StatusError
	return errors.As(err, &status) && status.StatusCode == http.StatusNotFound
}
