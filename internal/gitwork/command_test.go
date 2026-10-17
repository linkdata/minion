package gitwork

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestGitNeutralIdentityIgnoresAmbientEmailAndAllowsBotOverride(t *testing.T) {
	for _, email := range []string{"", "ambient@example.invalid"} {
		t.Run(email, func(t *testing.T) {
			t.Setenv("EMAIL", email)
			if email == "" {
				if err := os.Unsetenv("EMAIL"); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			ctx := context.Background()
			if email, err := runGit(ctx, dir, "config", "user.email"); err != nil || email != "minion@localhost" {
				t.Fatalf("controlled user.email=%q error=%v", email, err)
			}
			identity, err := runGit(ctx, dir, "var", "GIT_COMMITTER_IDENT")
			if err != nil || !strings.HasPrefix(identity, "Minion <minion@localhost> ") {
				t.Fatalf("neutral identity=%q error=%v", identity, err)
			}
			identity, err = runGit(ctx, dir, "-c", "user.name=example-bot", "-c", "user.email=example-bot@users.noreply.github.com", "var", "GIT_COMMITTER_IDENT")
			if err != nil || !strings.HasPrefix(identity, "example-bot <example-bot@users.noreply.github.com> ") {
				t.Fatalf("explicit identity=%q error=%v", identity, err)
			}
		})
	}
}

func TestGitDisablesAutomaticMaintenance(t *testing.T) {
	for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false"} {
		value, err := runGit(context.Background(), t.TempDir(), "config", "--get", key)
		if err != nil || value != want {
			t.Fatalf("controlled Git %s=%q error=%v", key, value, err)
		}
	}
}

func TestImportPreservesExecutableModeAndCommitIgnoresUnstagedFiles(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	const branch = "minion/fix-reviewed-mode"
	fixture.git(fixture.checkout.path, "config", "core.fileMode", "false")
	if err := fixture.checkout.recordConfig(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkout.Import(ctx, fixture.base, []workspace.Change{{Path: "run", Mode: workspace.Executable, Data: []byte("reviewed executable\n")}}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.checkout.path, "unreviewed"), "not imported\n")
	if err := fixture.checkout.CommitAndPush(ctx, branch, "bot", "reviewed mode", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	mode := fixture.git("", "--git-dir="+fixture.remote, "ls-tree", "refs/heads/"+branch, "run")
	if !strings.HasPrefix(mode, "100755 blob ") || fixture.git("", "--git-dir="+fixture.remote, "show", "refs/heads/"+branch+":run") != "reviewed executable" {
		t.Fatalf("published executable lost mode or bytes: %s", mode)
	}
	if got := fixture.git("", "--git-dir="+fixture.remote, "ls-tree", "refs/heads/"+branch, "unreviewed"); got != "" {
		t.Fatalf("commit added unreviewed output: %s", got)
	}
}
