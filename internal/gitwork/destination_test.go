package gitwork

import (
	"context"
	"os/exec"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestCommitAndPushRequiresValidatedDestination(t *testing.T) {
	fixture := newFixture(t)
	t.Cleanup(func() {
		if err := fixture.checkout.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	const repo, branch = "owner/destination", "minion/fix-destination"
	// Redirect only this controlled fixture's validated HTTPS URL to its local remote.
	fixture.git(fixture.checkout.path, "config", "url."+fixture.remote+".insteadOf", "https://github.com/"+repo+".git")
	if err := fixture.checkout.recordConfig(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkout.Import(ctx, fixture.base, []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("reviewed destination\n")}}); err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{"", "../repo", "o/r/extra", "https://github.com/o/r.git", "o/r extra"} {
		t.Run(destination, func(t *testing.T) {
			err := fixture.checkout.CommitAndPush(ctx, destination, branch, "bot", "reviewed", func(string) error {
				t.Fatal("invalid destination reached commit checkpoint")
				return nil
			})
			if err == nil {
				t.Fatal("accepted invalid destination")
			}
			if head, err := fixture.checkout.head(ctx); err != nil || head != fixture.base {
				t.Fatalf("invalid destination changed HEAD: %s, %v", head, err)
			}
			if err := exec.Command("git", "--git-dir="+fixture.remote, "show-ref", "--verify", "refs/heads/"+branch).Run(); err == nil {
				t.Fatal("invalid destination pushed a branch")
			}
		})
	}
	var recorded string
	if err := fixture.checkout.CommitAndPush(ctx, repo, branch, "bot", "reviewed", func(sha string) error {
		recorded = sha
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := fixture.git("", "--git-dir="+fixture.remote, "rev-parse", "refs/heads/"+branch); recorded == "" || recorded == fixture.base || got != recorded {
		t.Fatalf("validated destination did not receive checkpointed commit: pushed=%s recorded=%s", got, recorded)
	}
}
