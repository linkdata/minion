package gitwork

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestGitRejectsRelativeExecutableWhenErrDotDisabled(t *testing.T) {
	root := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Error(err)
		}
	})
	checkout := filepath.Join(root, "checkout")
	marker := filepath.Join(root, "executed")
	for _, dir := range []string{root, checkout} {
		bin := filepath.Join(dir, "relbin")
		if err := os.MkdirAll(bin, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf planted > "+shellQuote(marker)+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", "relbin")
	t.Setenv("GODEBUG", "execerrdot=0")
	if binary, err := exec.LookPath("git"); err != nil || filepath.IsAbs(binary) {
		t.Fatalf("probe did not produce a relative executable: %q, %v", binary, err)
	}
	if _, err := runGit(context.Background(), checkout, "--version"); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("accepted relative Git executable: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("checkout executable ran: %v", err)
	}
}

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
	if err := fixture.checkout.commitAndPush(ctx, fixture.remote, branch, "bot", "reviewed mode", func(string) error { return nil }); err != nil {
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

func TestCloneSkipsTagsAndPublicationAcceptsAmbiguousShortName(t *testing.T) {
	fixture := newFixture(t)
	const branch = "minion/fix-same-tag"
	fixture.git(fixture.source, "tag", branch)
	fixture.git(fixture.source, "push", fixture.remote, "refs/tags/"+branch)
	checkout := fixture.clone()
	if got := fixture.git(checkout.path, "tag", "--list"); got != "" {
		t.Fatalf("clone auto-followed remote tag: %s", got)
	}
	fixture.git(checkout.path, "tag", branch)
	if err := checkout.NewBranch(context.Background(), branch); err != nil {
		t.Fatal(err)
	}
	if err := checkout.Import(context.Background(), fixture.base, []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("reviewed\n")}}); err != nil {
		t.Fatal(err)
	}
	if err := checkout.commitAndPush(context.Background(), fixture.remote, branch, "bot", "reviewed", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := fixture.git("", "--git-dir="+fixture.remote, "show", "refs/heads/"+branch+":README.md"); got != "reviewed" {
		t.Fatalf("wrong published branch: %s", got)
	}
}

func TestGitEnvironmentPinsHardening(t *testing.T) {
	settings := []struct{ key, hostile, want string }{
		{"GIT_NO_REPLACE_OBJECTS", "0", "1"}, {"GIT_ATTR_NOSYSTEM", "0", "1"}, {"LC_ALL", "sv_SE.UTF-8", "C"},
	}
	for _, setting := range settings {
		t.Setenv(setting.key, setting.hostile)
	}
	env := cleanGitEnvironment()
	for _, setting := range settings {
		var matches []string
		for _, entry := range env {
			if strings.HasPrefix(entry, setting.key+"=") {
				matches = append(matches, entry)
			}
		}
		if len(matches) != 1 || matches[0] != setting.key+"="+setting.want {
			t.Fatalf("%s settings=%q", setting.key, matches)
		}
	}
}

func TestCommitAndPushRequiresValidatedDestination(t *testing.T) {
	fixture := newFixture(t)
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
