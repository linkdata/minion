package gitwork

import (
	"context"
	"errors"
	"fmt"
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

func TestGitNeutralIdentityIgnoresAmbientEmail(t *testing.T) {
	t.Setenv("EMAIL", "ambient@example.invalid")
	dir := t.TempDir()
	ctx := context.Background()
	if email, err := runGit(ctx, dir, "config", "user.email"); err != nil || email != "minion@localhost" {
		t.Fatalf("controlled user.email=%q error=%v", email, err)
	}
	identity, err := runGit(ctx, dir, "var", "GIT_COMMITTER_IDENT")
	if err != nil || !strings.HasPrefix(identity, "Minion <minion@localhost> ") {
		t.Fatalf("neutral identity=%q error=%v", identity, err)
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

func TestCommitAndPushPreservesUnicodeBranchSuffix(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	const branch = "minion/fix-nbsp\u00a0"
	if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkout.Import(ctx, fixture.base, []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("reviewed\n")}}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkout.commitAndPush(ctx, fixture.remote, branch, "bot", "reviewed", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := fixture.git("", "--git-dir="+fixture.remote, "show", "refs/heads/"+branch+":README.md"); got != "reviewed" {
		t.Fatalf("wrong published branch: %q", got)
	}
}

func TestCloneIncludesTagsAndPublicationAcceptsAmbiguousShortName(t *testing.T) {
	fixture := newFixture(t)
	const branch = "minion/fix-same-tag"
	fixture.git(fixture.source, "tag", branch)
	fixture.git(fixture.source, "push", fixture.remote, "refs/tags/"+branch)
	checkout := fixture.clone()
	if got := fixture.git(checkout.path, "tag", "--list"); got != branch {
		t.Fatalf("clone lost remote tag: %s", got)
	}
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
	for _, key := range []string{"GIT_DIR", "GIT_INDEX_FILE"} {
		t.Setenv(key, "/hostile/"+key)
	}
	settings := []struct{ key, hostile, want string }{
		{"GIT_NO_REPLACE_OBJECTS", "0", "1"}, {"GIT_NO_LAZY_FETCH", "0", "1"}, {"GIT_ATTR_NOSYSTEM", "0", "1"}, {"LC_ALL", "sv_SE.UTF-8", "C"},
		{"GIT_CONFIG_GLOBAL", "/hostile/config", os.DevNull}, {"GIT_CONFIG_NOSYSTEM", "0", "1"},
		{"GIT_CONFIG_COUNT", "2", "1"}, {"GIT_CONFIG_KEY_0", "filter.escape.clean", "safe.bareRepository"}, {"GIT_CONFIG_VALUE_0", "touch /hostile/marker", "explicit"},
		{"GIT_TERMINAL_PROMPT", "1", "0"}, {"GIT_ASKPASS", "/hostile/askpass", os.DevNull},
		{"GCM_INTERACTIVE", "always", "never"}, {"GH_PROMPT_DISABLED", "0", "1"},
		{"GIT_OPTIONAL_LOCKS", "1", "0"},
	}
	for _, setting := range settings {
		t.Setenv(setting.key, setting.hostile)
	}
	env := cleanGitEnvironment()
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_DIR=") || strings.HasPrefix(entry, "GIT_INDEX_FILE=") {
			t.Fatalf("inherited Git path reached trusted Git: %q", entry)
		}
	}
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

func TestCommitAndPushRefusesInvalidArgumentsAndHead(t *testing.T) {
	for _, kind := range []string{"branch", "blank message", "NUL message", "moved HEAD", "detached HEAD", "wrong branch", "nil recorder"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			const branch = "minion/fix-input-gates"
			if err := f.checkout.NewBranch(ctx, branch); err != nil {
				t.Fatal(err)
			}
			if err := f.checkout.Import(ctx, f.base, []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("reviewed\n")}}); err != nil {
				t.Fatal(err)
			}
			pushBranch, message, want := branch, "reviewed fix", ""
			switch kind {
			case "branch":
				pushBranch, want = "bad:ref", "invalid branch"
			case "blank message":
				message, want = " \n", "commit message"
			case "NUL message":
				message, want = "bad\x00message", "commit message"
			case "moved HEAD":
				f.git(f.checkout.path, "commit", "-m", "unreviewed commit")
				want = "HEAD moved"
			case "detached HEAD":
				f.git(f.checkout.path, "checkout", "--detach")
			case "wrong branch":
				f.git(f.checkout.path, "switch", "--create", "other-branch")
				want = "does not match push branch"
			case "nil recorder":
				want = "before-push recorder is required"
			}
			head := f.git(f.checkout.path, "rev-parse", "HEAD")
			refs := f.git(f.checkout.path, "for-each-ref", "--format=%(refname) %(objectname)")
			stage := f.git(f.checkout.path, "diff", "--cached", "--raw")
			remoteRefs := f.git("", "--git-dir="+f.remote, "for-each-ref", "--format=%(refname) %(objectname)")
			recorder := func(string) error {
				t.Fatal("invalid publication reached before-push recorder")
				return nil
			}
			if kind == "nil recorder" {
				recorder = nil
			}
			err := f.checkout.commitAndPush(ctx, f.remote, pushBranch, "bot", message, recorder)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s was not refused: %v", kind, err)
			}
			if kind == "detached HEAD" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || !strings.Contains(err.Error(), "symbolic-ref") {
					t.Fatalf("detached HEAD lost Git failure: %v", err)
				}
			}
			if got := f.git(f.checkout.path, "rev-parse", "HEAD"); got != head {
				t.Fatalf("refusal moved HEAD: %s", got)
			}
			if got := f.git(f.checkout.path, "for-each-ref", "--format=%(refname) %(objectname)"); got != refs {
				t.Fatalf("refusal changed local refs: %s", got)
			}
			if got := f.git(f.checkout.path, "diff", "--cached", "--raw"); got != stage {
				t.Fatalf("refusal changed staging: %s", got)
			}
			if got := f.git("", "--git-dir="+f.remote, "for-each-ref", "--format=%(refname) %(objectname)"); got != remoteRefs {
				t.Fatalf("refusal changed remote refs: %s", got)
			}
		})
	}
}

func TestCommitAndPushPreservesDivergentRemote(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const branch = "minion/fix-divergent"
	writeFile(t, filepath.Join(f.source, "remote.txt"), "remote-only\n")
	remoteHead := f.commit("refs/heads/"+branch, "divergent remote")
	if err := f.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if err := f.checkout.Import(ctx, f.base, []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("local-only\n")}}); err != nil {
		t.Fatal(err)
	}
	var recorded string
	err := f.checkout.commitAndPush(ctx, f.remote, branch, "bot", "local fix", func(sha string) error {
		recorded = sha
		return nil
	})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !strings.Contains(err.Error(), "git push") || !strings.Contains(err.Error(), "[rejected]") {
		t.Fatalf("divergent push did not preserve Git failure: %v", err)
	}
	if got := f.git(f.checkout.path, "rev-parse", "HEAD"); recorded == "" || recorded == f.base || recorded != got {
		t.Fatalf("rejected push lost commit checkpoint: recorded=%s local=%s", recorded, got)
	}
	if got := f.git("", "--git-dir="+f.remote, "rev-parse", "refs/heads/"+branch); got != remoteHead {
		t.Fatalf("rejected push changed remote: got=%s want=%s", got, remoteHead)
	}
}

func TestCommitAndPushPreservesPreexistingAncestorRemote(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const branch = "minion/fix-preexisting"
	f.git(f.source, "push", f.remote, f.base+":refs/heads/"+branch)
	if err := f.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if err := f.checkout.Import(ctx, f.base, []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("local fix\n")}}); err != nil {
		t.Fatal(err)
	}
	var recorded string
	err := f.checkout.commitAndPush(ctx, f.remote, branch, "bot", "local fix", func(sha string) error {
		recorded = sha
		return nil
	})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !strings.Contains(err.Error(), "git push") || !strings.Contains(err.Error(), "[rejected]") {
		t.Fatalf("create-only push accepted preexisting branch: %v", err)
	}
	if got := f.git(f.checkout.path, "rev-parse", "HEAD"); recorded == "" || recorded == f.base || recorded != got {
		t.Fatalf("rejected push lost commit checkpoint: recorded=%s local=%s", recorded, got)
	}
	if got := f.git("", "--git-dir="+f.remote, "rev-parse", "refs/heads/"+branch); got != f.base {
		t.Fatalf("rejected push changed remote: got=%s want=%s", got, f.base)
	}
}

func TestStagedPathCountClassifiesPathListTruncation(t *testing.T) {
	f := newFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nfor arg do\nif [ \"$arg\" = --name-only ]; then\nhead -c %d /dev/zero\nexit\nfi\ndone\nexec %s \"$@\"\n", maxChangedPathListBytes+1, shellQuote(realGit))
	fakeOnPath(t, "git", script)
	if count, err := f.checkout.stagedPathCount(context.Background()); count != 0 || !errors.Is(err, ErrTooManyFiles) || errors.Is(err, ErrCheckoutTooLarge) || errors.Is(err, context.Canceled) {
		t.Fatalf("staged path truncation misclassified: %d, %v", count, err)
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
	const destination = "../repo"
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
	if refs := fixture.git("", "--git-dir="+fixture.remote, "for-each-ref", "refs/heads/"+branch); refs != "" {
		t.Fatal("invalid destination pushed a branch")
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
