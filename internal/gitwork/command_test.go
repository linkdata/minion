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

func TestCloneSkipsTagsAndPublicationAcceptsAmbiguousShortName(t *testing.T) {
	fixture := newFixture(t)
	const branch = "minion/fix-same-tag"
	fixture.git(fixture.source, "tag", branch)
	fixture.git(fixture.source, "push", fixture.remote, "refs/tags/"+branch)
	checkout, err := cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", localTreeMetadata(t, fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := fixture.git(checkout.path, "tag", "--list"); got != "" {
		t.Fatalf("clone auto-followed remote tag: %s", got)
	}
	checkout.pushURL = fixture.remote
	fixture.git(checkout.path, "tag", branch)
	if err := checkout.NewBranch(context.Background(), branch); err != nil {
		t.Fatal(err)
	}
	if err := checkout.Import(context.Background(), fixture.base, []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("reviewed\n")}}); err != nil {
		t.Fatal(err)
	}
	if err := checkout.CommitAndPush(context.Background(), branch, "bot", "reviewed", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := fixture.git("", "--git-dir="+fixture.remote, "show", "refs/heads/"+branch+":README.md"); got != "reviewed" {
		t.Fatalf("wrong published branch: %s", got)
	}
}

func TestGitEnvironmentDisablesReplacementAndSystemAttributes(t *testing.T) {
	t.Setenv("GIT_NO_REPLACE_OBJECTS", "0")
	t.Setenv("GIT_ATTR_NOSYSTEM", "0")
	env := strings.Join(cleanGitEnvironment(), "\n")
	for _, setting := range []string{"GIT_NO_REPLACE_OBJECTS=1", "GIT_ATTR_NOSYSTEM=1"} {
		if !strings.Contains(env, "\n"+setting+"\n") {
			t.Fatalf("missing hardening %s", setting)
		}
	}
}
