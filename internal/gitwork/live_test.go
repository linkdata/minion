package gitwork

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/github"
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
		PullRequestNumber int    `json:"pull_request_number"`
		ReleaseTag        string `json:"release_tag"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	const repoName = "linkdata/miniontesting"
	if cfg.RepositoryURL != "https://github.com/"+repoName {
		t.Skipf("live Git checkout uses dedicated %s repository", repoName)
	}
	if cfg.PullRequestNumber <= 0 {
		t.Skip("pull_request_number is absent")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com", "--user", "linkdata")
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	output, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(output)) == "" {
		t.Fatalf("linkdata gh credential unavailable: %v", err)
	}
	client := github.New(strings.TrimSpace(string(output)))
	user, err := client.AuthenticatedUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(user.Login, "linkdata") {
		t.Fatalf("linkdata gh credential belongs to %s", user.Login)
	}
	t.Setenv("GH_TOKEN", strings.TrimSpace(string(output)))
	repo, err := client.Repository(ctx, repoName)
	if err != nil {
		t.Fatal(err)
	}
	if repo.DefaultBranch == "" {
		t.Fatal("test repository has no default branch")
	}
	pull, err := client.PullRequestDetails(ctx, repoName, cfg.PullRequestNumber)
	if err != nil {
		t.Fatal(err)
	}
	if pull.Number != cfg.PullRequestNumber || pull.Base.Ref == "" || len(pull.Head.SHA) != 40 {
		t.Fatalf("pull request metadata = %+v", pull)
	}

	checkout, err := Clone(ctx, t.TempDir(), repoName, repo.DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	if head, err := checkout.Head(ctx); err != nil || len(head) != 40 {
		t.Fatalf("cloned default-branch HEAD = %q, %v", head, err)
	}
	if cfg.ReleaseTag != "" {
		hasTag, err := checkout.HasTag(ctx, cfg.ReleaseTag)
		if err != nil {
			t.Fatal(err)
		}
		if !hasTag {
			t.Fatalf("clone has no %s tag", cfg.ReleaseTag)
		}
	}
	if err := checkout.CheckoutPullRequest(ctx, pull.Number, pull.Base.Ref, pull.Head.SHA, func(ctx context.Context, treeSHA string) (github.GitTree, error) {
		return client.GitTree(ctx, repoName, treeSHA)
	}); err != nil {
		t.Fatal(err)
	}
	if head, err := checkout.Head(ctx); err != nil || !strings.EqualFold(head, pull.Head.SHA) {
		t.Fatalf("pull request HEAD = %q, %v; want %s", head, err, pull.Head.SHA)
	}
}
