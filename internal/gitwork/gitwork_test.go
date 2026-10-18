package gitwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/workspace"
)

// Fixtures provide the same tree metadata as the GitHub lookup.
func localTreeMetadata(dir string) func(context.Context, string) (github.GitTree, error) {
	return func(ctx context.Context, sha string) (github.GitTree, error) {
		cmd := exec.CommandContext(ctx, "git", "-C", dir, "ls-tree", "-r", "-l", "-z", sha)
		output, err := cmd.Output()
		if err != nil {
			return github.GitTree{}, err
		}
		tree := completeTree(sha, []github.GitTreeEntry{})
		for _, line := range bytes.Split(bytes.TrimSuffix(output, []byte{0}), []byte{0}) {
			if len(line) == 0 {
				continue
			}
			metadata, name, ok := bytes.Cut(line, []byte{'\t'})
			fields := strings.Fields(string(metadata))
			if !ok || len(fields) != 4 {
				return github.GitTree{}, errors.New("invalid fixture tree")
			}
			entry := github.GitTreeEntry{Path: string(name), Mode: fields[0], Type: fields[1]}
			if entry.Type == "blob" {
				size, err := strconv.ParseInt(fields[3], 10, 64)
				if err != nil {
					return github.GitTree{}, err
				}
				entry.Size = &size
			}
			tree.Tree = append(tree.Tree, entry)
		}
		return tree, nil
	}
}

func TestResolveCredentialHelperPreservesSymlinkName(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	bin := t.TempDir()
	launcher := filepath.Join(bin, "launcher")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\n[ \"${0##*/}\" = gh ] || exit 9\nprintf 'username=trusted\\npassword=test\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ghPath := filepath.Join(bin, "gh")
	if err := os.Symlink(launcher, ghPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	got := resolveCredentialHelperGH()
	if got != ghPath {
		t.Fatalf("credential helper = %q, want symlink path %q", got, ghPath)
	}
	cmd := exec.Command(gitPath, "-c", "credential.helper=", "-c", credentialHelperConfig(got), "credential", "fill")
	cmd.Dir = t.TempDir()
	cmd.Env = cleanGitEnvironment()
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "username=trusted") {
		t.Fatalf("symlinked credential helper failed: %s: %v", output, err)
	}
}

func TestGitCredentialHelperIgnoresCheckoutRelativePath(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	checkout := t.TempDir()
	trustedBin := filepath.Join(t.TempDir(), "trusted' bin")
	if err := os.Mkdir(trustedBin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(checkout, "tools"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, username := range map[string]string{
		filepath.Join(trustedBin, "gh"):        "trusted",
		filepath.Join(checkout, "gh"):          "planted",
		filepath.Join(checkout, "tools", "gh"): "planted",
	} {
		content := "#!/bin/sh\nprintf 'username=" + username + "\\npassword=test\\n'\n"
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, path := range map[string]string{
		"empty entry":            ":" + trustedBin,
		"empty PATH":             "",
		"relative entry":         "tools:" + trustedBin,
		"all relative":           ".:tools",
		"absolute checkout path": checkout + ":" + trustedBin,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PATH", path)
			cmd := exec.Command(realGit, "-c", "credential.helper=", "-c", credentialHelperConfig(filepath.Join(trustedBin, "gh")), "credential", "fill")
			cmd.Dir = checkout
			cmd.Env = cleanGitEnvironment()
			validPath := false
			for _, entry := range cmd.Env {
				validPath = validPath || strings.HasPrefix(entry, "PATH=") && entry != "PATH="
			}
			if !validPath {
				t.Fatal("clean Git environment has no usable PATH")
			}
			cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
			output, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(output), "username=trusted") || strings.Contains(string(output), "planted") {
				t.Fatalf("credential helper resolved through %q: %s: %v", path, output, err)
			}
		})
	}
}

func TestRepositoryAndRefValidation(t *testing.T) {
	url, err := repositoryURL("Acme/project.name")
	if err != nil || url != "https://github.com/Acme/project.name.git" {
		t.Fatalf("repositoryURL() = (%q, %v)", url, err)
	}
	if _, err := repositoryURL("owner/repo space"); err == nil {
		t.Fatal("repositoryURL accepted an invalid repository")
	}
	for _, branch := range []string{"main", "release/v1.2", "minion/fix-123"} {
		if err := validateBranch(branch); err != nil {
			t.Errorf("validateBranch(%q): %v", branch, err)
		}
	}
	for _, branch := range []string{"", "@", "-bad", "bad..name", "bad.lock", "topic/.hidden", "topic//bad", "topic@{1}", "bad:ref", "bad name", "bad\\ref", "/a", "a/", "a.", "a~b", "a^b", "a?b", "a*b", "a[b", "a\x01b", "a\x7fb"} {
		if err := validateBranch(branch); err == nil {
			t.Errorf("validateBranch(%q) should fail", branch)
		}
	}
	if _, err := Clone(context.Background(), t.TempDir(), "../bad", "main", nil, true); err == nil {
		t.Fatal("Clone accepted an invalid repository")
	}
	if _, err := Clone(context.Background(), t.TempDir(), "owner/repo", "bad..branch", nil, true); err == nil {
		t.Fatal("Clone accepted an invalid default branch")
	}
	if _, err := Clone(context.Background(), t.TempDir(), "owner/repo", "main", nil, true); err == nil {
		t.Fatal("Clone accepted missing tree metadata lookup")
	}
}

func TestHeadRejectsMovedCheckout(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(fixture.checkout.path, "unexpected.txt"), "unexpected\n")
	fixture.git(fixture.checkout.path, "add", ".")
	fixture.git(fixture.checkout.path, "commit", "-m", "moved")
	if _, err := fixture.checkout.verifiedHead(ctx); err == nil || !strings.Contains(err.Error(), "HEAD moved") {
		t.Fatalf("verifiedHead() error = %v, want moved HEAD refusal", err)
	}
}

func TestCheckoutPullRequestVerifiesHead(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(fixture.source, "change.txt"), "PR change\n")
	head := fixture.commit("refs/pull/7/head", "PR head")

	if err := fixture.checkout.checkoutPullRequest(ctx, 7, "main", strings.Repeat("f", 40)); err == nil || !strings.Contains(err.Error(), "pull request head changed: expected "+strings.Repeat("f", 40)+", fetched "+head) {
		t.Fatalf("mismatched PR head error = %v", err)
	}
	if err := fixture.checkout.checkoutPullRequest(ctx, 7, "main", head); err != nil {
		t.Fatalf("CheckoutPullRequest() error = %v", err)
	}
	got := fixture.git(fixture.checkout.path, "rev-parse", "HEAD")
	if got != head {
		t.Fatalf("checkout HEAD = %s, want %s", got, head)
	}
	if fixture.git(fixture.checkout.path, "rev-parse", "refs/remotes/origin/main") != fixture.base {
		t.Fatal("cloned base branch changed")
	}
}

func TestCheckoutPullRequestPreservesAliasedTreeEntries(t *testing.T) {
	const width, depth, blobBytes = 4, 2, 1024
	fixture := newFixture(t)
	blobPath := filepath.Join(fixture.source, "aliased-blob")
	writeFile(t, blobPath, strings.Repeat("x", blobBytes))
	blob := fixture.git(fixture.source, "hash-object", "-w", blobPath)
	if err := os.Remove(blobPath); err != nil {
		t.Fatal(err)
	}
	tree := fixture.gitInput(fixture.source, fmt.Sprintf("100644 blob %s\tpayload\n", blob), "mktree")
	for level := 0; level < depth; level++ {
		var entries strings.Builder
		for i := 0; i < width; i++ {
			fmt.Fprintf(&entries, "040000 tree %s\td%02d\n", tree, i)
		}
		tree = fixture.gitInput(fixture.source, entries.String(), "mktree")
	}
	head := fixture.gitInput(fixture.source, "aliased PR head\n", "commit-tree", tree, "-p", fixture.base)
	fixture.git(fixture.source, "push", fixture.remote, head+":refs/pull/7/head")
	checkout := fixture.clonePullRequest(head)
	path := checkout.path
	for level := 0; level < depth; level++ {
		path = filepath.Join(path, "d00")
	}
	if _, err := os.Stat(filepath.Join(path, "payload")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PR worktree materialized: %v", err)
	}
	snapshot, err := checkout.Snapshot(context.Background())
	if err != nil || len(snapshot.Files) != width*width {
		t.Fatalf("aliased tree snapshot files=%d error=%v", len(snapshot.Files), err)
	}
	for _, file := range snapshot.Files {
		if len(file.Data) != blobBytes {
			t.Fatalf("snapshot file %q bytes=%d", file.Path, len(file.Data))
		}
	}
}

func completeTree(sha string, entries []github.GitTreeEntry) github.GitTree {
	truncated := false
	return github.GitTree{SHA: sha, Truncated: &truncated, Tree: entries}
}

func TestCheckoutPullRequestPreflightsMetadataBeforeSourceRead(t *testing.T) {
	fixture := newFixture(t)
	const blobBytes = 2 << 20
	writeFile(t, filepath.Join(fixture.source, "new.dat"), strings.Repeat("x", blobBytes))
	fixture.git(fixture.source, "add", "new.dat")
	fixture.git(fixture.source, "commit", "-m", "new blob")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	blob := fixture.git(fixture.source, "rev-parse", "HEAD:new.dat")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	checkout := fixture.keepCheckout(cloneFromURL(context.Background(), t.TempDir(), "file://"+fixture.remote, "main", localTreeMetadata(fixture.source), true))
	log := recordSnapshotContentCalls(t)
	oversized := func(_ context.Context, sha string) (github.GitTree, error) {
		size := int64(workspace.MaxBytes + 1)
		return completeTree(sha, []github.GitTreeEntry{{Path: "new.dat", Type: "blob", Mode: "100644", Size: &size}}), nil
	}
	checkout.lookupTree = oversized
	if err := checkout.checkoutPullRequest(context.Background(), 7, "main", head); !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("oversized PR error = %v, want checkout limit", err)
	}
	assertBlobPresent(fixture, checkout.path, blob)
	if data, err := os.ReadFile(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata preflight requested source content: %q error=%v", data, err)
	}
	if got := fixture.git(checkout.path, "rev-parse", "HEAD"); got != fixture.base {
		t.Fatalf("rejected PR moved HEAD to %s", got)
	}
}

func TestCloneFailureRemovesOwnedDirectoryAndPreservesCause(t *testing.T) {
	fixture := newFixture(t)
	root := t.TempDir()
	// A failed clone can leave data inside the directory it was given.
	fakeOnPath(t, "git", "#!/bin/sh\nfor arg do destination=$arg; done\nprintf retained > \"$destination/partial\"\nprintf 'clone fixture failure' >&2\nexit 7\n")
	checkout, err := cloneRepository(context.Background(), root, fixture.remote, "main", localTreeMetadata(fixture.source))
	var exit *exec.ExitError
	if checkout != nil || !errors.As(err, &exit) || exit.ExitCode() != 7 || !strings.Contains(err.Error(), "clone fixture failure") {
		t.Fatalf("failed clone lost its cause: checkout=%v error=%v", checkout, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("failed clone leaked owned data: entries=%v error=%v", entries, err)
	}
}

func TestNewBranchPreservesOmittedGitlink(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.source, "update-index", "--add", "--cacheinfo", "160000,"+fixture.base+",sub")
	fixture.git(fixture.source, "commit", "-m", "add gitlink")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")

	checkout := fixture.clone()
	ctx := context.Background()
	snapshot, err := checkout.Snapshot(ctx)
	if err != nil || len(snapshot.Omitted) != 1 || snapshot.Omitted[0] != "sub" {
		t.Fatalf("gitlink snapshot=%+v error=%v", snapshot, err)
	}
	const branch = "minion/fix-gitlink"
	if err := checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if err := checkout.Import(ctx, snapshot.Head, []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("fixed\n")}}); err != nil {
		t.Fatal(err)
	}
	if err := checkout.commitAndPush(ctx, fixture.remote, branch, "bot", "fix", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := fixture.git("", "--git-dir="+fixture.remote, "ls-tree", "refs/heads/"+branch, "sub"); got != "160000 commit "+fixture.base+"\tsub" {
		t.Fatalf("published gitlink changed: %q", got)
	}
}

func TestCommitAndPushPublishesRecordedBotCommit(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	const branch = "minion/fix-123"
	if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkout.Import(ctx, fixture.base, []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("fixed\n")}}); err != nil {
		t.Fatal(err)
	}
	var recordedHead string
	if err := fixture.checkout.commitAndPush(ctx, fixture.remote, branch, "example-bot", "Minion: fix issue 123", func(sha string) error {
		recordedHead = sha
		return nil
	}); err != nil {
		t.Fatalf("CommitAndPush() error = %v", err)
	}
	localHead := fixture.git(fixture.checkout.path, "rev-parse", "HEAD")
	remoteHead := fixture.git("", "--git-dir="+fixture.remote, "rev-parse", "refs/heads/"+branch)
	if localHead != remoteHead || recordedHead != remoteHead {
		t.Fatalf("pushed %s, local HEAD %s, recorded HEAD %s", remoteHead, localHead, recordedHead)
	}
	identity := fixture.git(fixture.checkout.path, "show", "-s", "--format=%an <%ae>|%cn <%ce>", "HEAD")
	if want := "example-bot <example-bot@users.noreply.github.com>|example-bot <example-bot@users.noreply.github.com>"; identity != want {
		t.Fatalf("commit identity = %q, want %q", identity, want)
	}
	if err := fixture.checkout.commitAndPush(ctx, fixture.remote, branch, "example-bot", "no changes", func(string) error { t.Fatal("unchanged tree reached push"); return nil }); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("repeat publication=%v", err)
	}
	path := fixture.checkout.path
	if err := fixture.checkout.Close(); err != nil {
		t.Fatal(err)
	}
	if fixture.checkout.path != "" {
		t.Fatal("closed checkout still exposes path")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkout path still exists: %v", err)
	}
	if err := fixture.checkout.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestImportAndCommitBoundsChanges(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"file count", "patch bytes"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newFixture(t)
			const branch = "minion/fix-limited"
			if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
				t.Fatal(err)
			}
			var changes []workspace.Change
			switch kind {
			case "file count":
				for i := 0; i <= workspace.MaxChanges; i++ {
					changes = append(changes, workspace.Change{Path: fmt.Sprintf("file-%03d", i), Mode: workspace.Regular, Data: []byte("x")})
				}
			case "patch bytes":
				changes = []workspace.Change{{Path: "large.txt", Mode: workspace.Regular, Data: []byte(strings.Repeat("x\n", workspace.MaxChangeBytes/2-100))}}
			}
			err := fixture.checkout.Import(ctx, fixture.base, changes)
			if kind == "patch bytes" && err != nil {
				t.Fatal(err)
			}
			if kind == "file count" && (err == nil || !strings.Contains(err.Error(), "changed file limit")) {
				t.Fatalf("oversized import lost file count limit: %v", err)
			}
			err = fixture.checkout.commitAndPush(ctx, fixture.remote, branch, "example-bot", "Minion: fix", func(string) error { t.Fatal("oversized changes reached before-push recorder"); return nil })
			want := ErrNoChanges
			if kind == "patch bytes" {
				want = ErrDiffTooLarge
			}
			if !errors.Is(err, want) {
				t.Fatalf("publication after %s error=%v want=%v", kind, err, want)
			}
			if _, err := exec.Command("git", "--git-dir="+fixture.remote, "show-ref", "--verify", "refs/heads/"+branch).CombinedOutput(); err == nil {
				t.Fatal("oversized changes were pushed")
			}
		})
	}
}

func TestCommitAndPushBoundsDirectlyStagedPathCount(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	const branch = "minion/fix-staged-limit"
	if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= workspace.MaxChanges; i++ {
		writeFile(t, filepath.Join(fixture.checkout.path, fmt.Sprintf("f%d", i)), "x")
	}
	fixture.git(fixture.checkout.path, "add", ".")
	err := fixture.checkout.commitAndPush(ctx, fixture.remote, branch, "bot", "too many paths", func(string) error {
		t.Fatal("oversized stage reached before-push recorder")
		return nil
	})
	if !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("short staged paths bypassed count limit: %v", err)
	}
	if head := fixture.git(fixture.checkout.path, "rev-parse", "HEAD"); head != fixture.base {
		t.Fatalf("oversized stage was committed: %s", head)
	}
	if output, err := exec.Command("git", "--git-dir="+fixture.remote, "show-ref", "--verify", "refs/heads/"+branch).CombinedOutput(); err == nil {
		t.Fatalf("oversized stage was pushed: %s", output)
	}
}

func TestSplitNULRecords(t *testing.T) {
	for _, test := range []struct {
		data string
		want []string
	}{
		{"", nil},
		{"one\x00", []string{"one"}},
		{"one\x00two\x00", []string{"one", "two"}},
		{"\x00", []string{""}},
	} {
		records, err := splitNUL([]byte(test.data))
		if err != nil || len(records) != len(test.want) {
			t.Fatalf("split %q=%q,%v", test.data, records, err)
		}
		for i, want := range test.want {
			if string(records[i]) != want {
				t.Fatalf("split %q record %d=%q want=%q", test.data, i, records[i], want)
			}
		}
	}
	if _, err := splitNUL([]byte("one\x00unterminated")); err == nil {
		t.Fatal("accepted unterminated records")
	}
}

func TestChangedGitConfigStopsPublish(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	if err := fixture.checkout.NewBranch(ctx, "minion/fix-config"); err != nil {
		t.Fatal(err)
	}
	fixture.git(fixture.checkout.path, "config", "url.https://example.invalid/.insteadOf", "https://github.com/")
	if err := fixture.checkout.commitAndPush(ctx, fixture.remote, "minion/fix-config", "example-bot", "Minion: fix", func(string) error { return nil }); err == nil || !strings.Contains(err.Error(), "Git config changed") {
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
	if err := fixture.checkout.Import(ctx, fixture.base, []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("fixed\n")}}); err != nil {
		t.Fatal(err)
	}
	recordFailure := errors.New("cannot persist commit SHA")
	err := fixture.checkout.commitAndPush(ctx, fixture.remote, branch, "example-bot", "Minion: fix", func(string) error { return recordFailure })
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
	fixture.base = fixture.commit("refs/heads/main", "initial")
	path := filepath.Join(root, "checkout")
	fixture.git("", "clone", "--no-checkout", "--branch", "main", fixture.remote, path)
	fixture.git(path, "checkout", "--detach", "refs/remotes/origin/main")
	fixture.checkout = &Checkout{path: path, expectedHead: fixture.base, lookupTree: localTreeMetadata(path)}
	if err := disableAttributes(filepath.Join(path, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkout.recordConfig(); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f gitFixture) git(dir string, args ...string) string {
	return f.gitInput(dir, "", args...)
}

func (f gitFixture) clone() *Checkout {
	f.t.Helper()
	return f.keepCheckout(cloneFromURL(context.Background(), f.t.TempDir(), f.remote, "main", localTreeMetadata(f.source), true))
}

func (f gitFixture) clonePullRequest(head string) *Checkout {
	f.t.Helper()
	return f.keepCheckout(clonePullRequestFromURL(context.Background(), f.t.TempDir(), f.remote, 7, "main", head, localTreeMetadata(f.source)))
}

func (f gitFixture) keepCheckout(checkout *Checkout, err error) *Checkout {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		if err := checkout.Close(); err != nil {
			f.t.Error(err)
		}
	})
	return checkout
}

func (f gitFixture) commit(ref, message string) string {
	f.t.Helper()
	f.git(f.source, "add", "--all")
	f.git(f.source, "commit", "-m", message)
	head := f.git(f.source, "rev-parse", "HEAD")
	f.git(f.source, "push", f.remote, "HEAD:"+ref)
	return head
}

func (f gitFixture) gitInput(dir, input string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
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
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// shellQuote treats fixture paths as literal shell arguments.
var shellQuote = testsupport.ShellQuote
