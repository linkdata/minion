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
	"time"
)

func TestRepositoryAndRefValidation(t *testing.T) {
	url, err := repositoryURL("Acme/project.name")
	if err != nil || url != "https://github.com/Acme/project.name.git" {
		t.Fatalf("repositoryURL() = (%q, %v)", url, err)
	}
	for _, repo := range []string{"", "owner", "owner/repo/extra", "../repo", "owner/..", "owner/repo?x=1", "owner/repo space", "owner:token/repo"} {
		if _, err := repositoryURL(repo); err == nil {
			t.Errorf("repositoryURL(%q) should fail", repo)
		}
	}
	for _, branch := range []string{"main", "release/v1.2", "minion/fix-123"} {
		if err := validateBranch(branch); err != nil {
			t.Errorf("validateBranch(%q): %v", branch, err)
		}
	}
	for _, branch := range []string{"", "-bad", "bad..name", "bad.lock", "topic/.hidden", "topic//bad", "topic@{1}", "bad:ref", "bad name", "bad\\ref"} {
		if err := validateBranch(branch); err == nil {
			t.Errorf("validateBranch(%q) should fail", branch)
		}
	}
	if !validSHA(strings.Repeat("a", 40)) || validSHA("bad") || validSHA(strings.Repeat("g", 40)) {
		t.Fatal("SHA validation failed")
	}
	if _, err := Clone(context.Background(), t.TempDir(), "../bad", "main"); err == nil {
		t.Fatal("Clone accepted an invalid repository")
	}
	if _, err := Clone(context.Background(), t.TempDir(), "owner/repo", "bad..branch"); err == nil {
		t.Fatal("Clone accepted an invalid default branch")
	}
}

func TestCloneFromLocalFixtureIncludesTags(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.source, "tag", "v1.0.0")
	fixture.git(fixture.source, "push", fixture.remote, "refs/tags/v1.0.0")
	ctx := context.Background()
	checkout, err := cloneFromURL(ctx, t.TempDir(), fixture.remote, "main")
	if err != nil {
		t.Fatalf("cloneFromURL() error = %v", err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	if head := fixture.git(checkout.Path(), "rev-parse", "HEAD"); head != fixture.base {
		t.Fatalf("clone HEAD = %s, want %s", head, fixture.base)
	}
	if tag := fixture.git(checkout.Path(), "rev-parse", "refs/tags/v1.0.0"); tag != fixture.base {
		t.Fatalf("cloned tag = %s, want %s", tag, fixture.base)
	}
	if head, err := checkout.Head(ctx); err != nil || head != fixture.base {
		t.Fatalf("Head() = (%q, %v), want %s", head, err, fixture.base)
	}
	if exists, err := checkout.HasTag(ctx, "v1.0.0"); err != nil || !exists {
		t.Fatalf("HasTag(v1.0.0) = (%t, %v), want true", exists, err)
	}
	if exists, err := checkout.HasTag(ctx, "v2.0.0"); err != nil || exists {
		t.Fatalf("HasTag(v2.0.0) = (%t, %v), want false", exists, err)
	}
	if _, err := checkout.HasTag(ctx, "v2/.bad"); err == nil {
		t.Fatal("HasTag accepted an invalid ref name")
	}
}

func TestHeadRejectsMovedCheckout(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(fixture.checkout.Path(), "unexpected.txt"), "unexpected\n")
	fixture.git(fixture.checkout.Path(), "add", ".")
	fixture.git(fixture.checkout.Path(), "commit", "-m", "moved")
	if _, err := fixture.checkout.Head(ctx); err == nil || !strings.Contains(err.Error(), "HEAD moved") {
		t.Fatalf("Head() error = %v, want moved HEAD refusal", err)
	}
}

func TestCheckoutPullRequestVerifiesHead(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(fixture.source, "change.txt"), "PR change\n")
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "PR head")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")

	if err := fixture.checkout.CheckoutPullRequest(ctx, 7, "main", strings.Repeat("f", 40)); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("mismatched PR head error = %v, want ErrHeadMoved", err)
	}
	if err := fixture.checkout.CheckoutPullRequest(ctx, 7, "main", head); err != nil {
		t.Fatalf("CheckoutPullRequest() error = %v", err)
	}
	got := fixture.git(fixture.checkout.Path(), "rev-parse", "HEAD")
	if got != head {
		t.Fatalf("checkout HEAD = %s, want %s", got, head)
	}
	if fixture.git(fixture.checkout.Path(), "rev-parse", "refs/remotes/minion/base") != fixture.base {
		t.Fatal("base branch was not fetched")
	}
}

func TestNestedGitMetadataCannotRunFilter(t *testing.T) {
	t.Setenv("GH_TOKEN", "DUMMY_CANARY")
	t.Setenv("GITHUB_TOKEN", "DUMMY_CANARY")
	fixture := newFixture(t)
	child := filepath.Join(t.TempDir(), "child")
	fixture.git("", "init", "--initial-branch=main", child)
	writeFile(t, filepath.Join(child, ".gitattributes"), "*.txt filter=probe\n")
	writeFile(t, filepath.Join(child, "payload.txt"), "original\n")
	fixture.git(child, "add", ".")
	fixture.git(child, "commit", "-m", "child")
	childHead := fixture.git(child, "rev-parse", "HEAD")
	fixture.git(fixture.source, "update-index", "--add", "--cacheinfo", "160000,"+childHead+",sub")
	fixture.git(fixture.source, "commit", "-m", "add gitlink")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")

	ctx := context.Background()
	checkout, err := cloneFromURL(ctx, t.TempDir(), fixture.remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer checkout.Close()
	if changed, err := checkout.HasChanges(ctx); err != nil || changed {
		t.Fatalf("clean gitlink HasChanges() = (%t, %v)", changed, err)
	}
	const branch = "minion/fix-nested-git"
	if err := checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(checkout.Path(), "sub")
	fixture.git(sub, "init", "--initial-branch=main")
	filter := filepath.Join(t.TempDir(), "filter.sh")
	marker := filepath.Join(t.TempDir(), "filter-ran")
	writeFile(t, filter, "#!/bin/sh\ncat\nprintf x >> \"$1\"\n")
	if err := os.Chmod(filter, 0700); err != nil {
		t.Fatal(err)
	}
	fixture.git(sub, "config", "filter.probe.clean", filter+" "+marker)
	writeFile(t, filepath.Join(sub, ".gitattributes"), "*.txt filter=probe\n")
	writeFile(t, filepath.Join(sub, "payload.txt"), "original\n")
	fixture.git(sub, "add", ".")
	writeFile(t, filepath.Join(sub, "payload.txt"), "altered!\n") // Same size forces Git to clean the file.
	when := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(sub, "payload.txt"), when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := checkout.HasChanges(ctx); err == nil || !strings.Contains(err.Error(), "nested Git metadata") {
		t.Fatalf("HasChanges() error = %v, want nested Git refusal", err)
	}
	if err := checkout.CommitAndPush(ctx, branch, "bot", "fix", func(string) error { return nil }); err == nil || !strings.Contains(err.Error(), "nested Git metadata") {
		t.Fatalf("CommitAndPush() error = %v, want nested Git refusal", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || len(data) != 0 {
		t.Fatalf("nested filter ran: marker = %q, error = %v", data, err)
	}

	cleanCheckout, err := cloneFromURL(ctx, t.TempDir(), fixture.remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanCheckout.Close()
	cleanSub := filepath.Join(cleanCheckout.Path(), "sub")
	fixture.git("", "clone", child, cleanSub) // Its HEAD matches the tracked gitlink.
	fixture.git(cleanSub, "config", "filter.probe.clean", filter+" "+marker)
	when = time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(cleanSub, "payload.txt"), when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanCheckout.HasChanges(ctx); err == nil || !strings.Contains(err.Error(), "nested Git metadata") {
		t.Fatalf("clean nested repo HasChanges() error = %v, want refusal", err)
	}
	data, err = os.ReadFile(marker)
	if err != nil || len(data) != 0 {
		t.Fatalf("clean nested filter ran: marker = %q, error = %v", data, err)
	}
}

func TestNestedGitScanBoundsAndDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := rejectNestedGitMetadata(context.Background(), root, 1); err != nil {
		t.Fatalf("root Git metadata or symlink target was scanned: %v", err)
	}
	writeFile(t, filepath.Join(root, "second"), "x")
	if err := rejectNestedGitMetadata(context.Background(), root, 1); err == nil || !strings.Contains(err.Error(), "scan exceeds") {
		t.Fatalf("bounded scan error = %v, want entry limit", err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(nested, ".git"), "gitdir: elsewhere\n")
	if err := rejectNestedGitMetadata(context.Background(), root, 10); err == nil || !strings.Contains(err.Error(), "nested Git metadata") {
		t.Fatalf("Git metadata file error = %v, want refusal", err)
	}
}

func TestCommitAndPushUsesTrustedURLAndSkipsHooks(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	const branch = "minion/fix-123"
	if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.checkout.Path(), "fix.txt"), "fixed\n")
	// These hooks would fail a normal commit or push.
	for _, name := range []string{"pre-commit", "pre-push"} {
		hook := filepath.Join(fixture.checkout.Path(), ".git", "hooks", name)
		writeFile(t, hook, "#!/bin/sh\nexit 1\n")
		if err := os.Chmod(hook, 0700); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := fixture.checkout.HasChanges(ctx)
	if err != nil || !changed {
		t.Fatalf("HasChanges() = (%t, %v), want true", changed, err)
	}
	var recordedHead string
	if err := fixture.checkout.CommitAndPush(ctx, branch, "example-bot", "Minion: fix issue 123", func(sha string) error {
		recordedHead = sha
		return nil
	}); err != nil {
		t.Fatalf("CommitAndPush() error = %v", err)
	}
	localHead := fixture.git(fixture.checkout.Path(), "rev-parse", "HEAD")
	remoteHead := fixture.git("", "--git-dir="+fixture.remote, "rev-parse", "refs/heads/"+branch)
	if localHead != remoteHead || recordedHead != remoteHead {
		t.Fatalf("pushed %s, local HEAD %s, recorded HEAD %s", remoteHead, localHead, recordedHead)
	}
	identity := fixture.git(fixture.checkout.Path(), "show", "-s", "--format=%an <%ae>|%cn <%ce>", "HEAD")
	if want := "example-bot <example-bot@users.noreply.github.com>|example-bot <example-bot@users.noreply.github.com>"; identity != want {
		t.Fatalf("commit identity = %q, want %q", identity, want)
	}
	changed, err = fixture.checkout.HasChanges(ctx)
	if err != nil || changed {
		t.Fatalf("HasChanges() after push = (%t, %v), want false", changed, err)
	}
	path := fixture.checkout.Path()
	if err := fixture.checkout.Close(); err != nil {
		t.Fatal(err)
	}
	if fixture.checkout.Path() != "" {
		t.Fatal("closed checkout still exposes path")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkout path still exists: %v", err)
	}
	if err := fixture.checkout.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestGitErrorLabelsCommandAfterConfigOptions(t *testing.T) {
	fixture := newFixture(t)
	_, err := runGit(context.Background(), fixture.checkout.Path(),
		"-c", "user.name=example-bot", "-c", "user.email=example-bot@users.noreply.github.com",
		"commit", "--not-a-real-option")
	if err == nil || !strings.Contains(err.Error(), "git commit:") {
		t.Fatalf("runGit() error = %v, want git commit error", err)
	}
}

func TestCommitAndPushBoundsChanges(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		write func(t *testing.T, path string)
		want  error
	}{
		{
			name: "file count",
			write: func(t *testing.T, path string) {
				for i := 0; i <= maxChangedFiles; i++ {
					writeFile(t, filepath.Join(path, fmt.Sprintf("file-%03d.txt", i)), "x\n")
				}
			},
			want: ErrTooManyFiles,
		},
		{
			name: "changed file size",
			write: func(t *testing.T, path string) {
				writeFile(t, filepath.Join(path, "large.txt"), strings.Repeat("x", maxChangedFileSize+1))
			},
			want: ErrChangedFileLarge,
		},
		{
			name: "patch size",
			write: func(t *testing.T, path string) {
				writeFile(t, filepath.Join(path, "many-lines.txt"), strings.Repeat("x\n", maxChangedFileSize/2-100))
			},
			want: ErrDiffTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newFixture(t)
			const branch = "minion/fix-limited"
			if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
				t.Fatal(err)
			}
			tt.write(t, fixture.checkout.Path())
			if err := fixture.checkout.CommitAndPush(ctx, branch, "example-bot", "Minion: fix", func(string) error {
				t.Fatal("oversized changes reached before-push recorder")
				return nil
			}); !errors.Is(err, tt.want) {
				t.Fatalf("CommitAndPush() error = %v, want %v", err, tt.want)
			}
			if _, err := exec.Command("git", "--git-dir="+fixture.remote, "show-ref", "--verify", "refs/heads/"+branch).CombinedOutput(); err == nil {
				t.Fatal("oversized changes were pushed")
			}
		})
	}
}

func TestChangedGitConfigStopsPublish(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	if err := fixture.checkout.NewBranch(ctx, "minion/fix-config"); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(fixture.checkout.Path(), ".git", "config")
	f, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[url \"https://example.invalid/\"]\n\tinsteadOf = https://github.com/\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkout.CommitAndPush(ctx, "minion/fix-config", "example-bot", "Minion: fix", func(string) error { return nil }); err == nil || !strings.Contains(err.Error(), "Git config changed") {
		t.Fatalf("CommitAndPush() error = %v, want config change refusal", err)
	}
}

func TestCommitAndPushStopsWhenRecordFails(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	const branch = "minion/fix-record"
	if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.checkout.Path(), "fix.txt"), "fixed\n")
	recordFailure := errors.New("cannot persist commit SHA")
	err := fixture.checkout.CommitAndPush(ctx, branch, "example-bot", "Minion: fix", func(string) error { return recordFailure })
	if !errors.Is(err, recordFailure) {
		t.Fatalf("CommitAndPush() error = %v, want recorder error", err)
	}
	if _, err := exec.Command("git", "--git-dir="+fixture.remote, "show-ref", "--verify", "refs/heads/"+branch).CombinedOutput(); err == nil {
		t.Fatal("commit was pushed despite recorder failure")
	}
}

type gitFixture struct {
	t        *testing.T
	remote   string
	source   string
	base     string
	checkout *Checkout
}

func newFixture(t *testing.T) gitFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	fixture := gitFixture{t: t, remote: filepath.Join(root, "remote.git"), source: filepath.Join(root, "source")}
	fixture.git("", "init", "--bare", fixture.remote)
	fixture.git("", "init", "--initial-branch=main", fixture.source)
	writeFile(t, filepath.Join(fixture.source, "README.md"), "fixture\n")
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "initial")
	fixture.base = fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	path := filepath.Join(root, "checkout")
	fixture.git("", "clone", "--no-checkout", "--branch", "main", fixture.remote, path)
	fixture.git(path, "checkout", "--detach", "refs/remotes/origin/main")
	fixture.checkout = &Checkout{path: path, url: fixture.remote, expectedHead: fixture.base}
	if err := fixture.checkout.recordConfig(); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f gitFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
