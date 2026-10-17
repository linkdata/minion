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
	"github.com/linkdata/minion/internal/workspace"
)

// Local fixtures have every blob; production must obtain sizes from GitHub.
func localTreeMetadata(t *testing.T, dir string) func(context.Context, string) (github.GitTree, error) {
	t.Helper()
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
	for _, repo := range []string{strings.Repeat("a", 100) + "/repo", "owner/" + strings.Repeat("a", 100)} {
		if _, err := repositoryURL(repo); err != nil {
			t.Fatalf("repository identifier at its limit was rejected: %v", err)
		}
	}
	for _, repo := range []string{"", "owner", "owner/repo/extra", "../repo", "owner/..", "owner/repo?x=1", "owner/repo space", "owner:token/repo", strings.Repeat("a", 101) + "/repo", "owner/" + strings.Repeat("a", 101)} {
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

func TestCloneFromLocalFixtureIncludesBranchHistory(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.source, "commit", "--allow-empty", "-m", "next")
	tip := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	ctx := context.Background()
	checkout, err := cloneFromURL(ctx, t.TempDir(), "file://"+fixture.remote, "main", localTreeMetadata(t, fixture.source), true)
	if err != nil {
		t.Fatalf("cloneFromURL() error = %v", err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	if head := fixture.git(checkout.path, "rev-parse", "HEAD"); head != tip {
		t.Fatalf("clone HEAD = %s, want %s", head, tip)
	}
	if head, err := checkout.verifiedHead(ctx); err != nil || head != tip {
		t.Fatalf("verifiedHead() = (%q, %v), want %s", head, err, tip)
	}
	if count := fixture.git(checkout.path, "rev-list", "--count", "HEAD"); count != "2" {
		t.Fatalf("cloned history contains %s commits, want 2", count)
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
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "PR head")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")

	if err := fixture.checkout.checkoutPullRequest(ctx, 7, "main", strings.Repeat("f", 40), true); !errors.Is(err, errHeadMoved) {
		t.Fatalf("mismatched PR head error = %v, want errHeadMoved", err)
	}
	if err := fixture.checkout.checkoutPullRequest(ctx, 7, "main", head, true); err != nil {
		t.Fatalf("CheckoutPullRequest() error = %v", err)
	}
	got := fixture.git(fixture.checkout.path, "rev-parse", "HEAD")
	if got != head {
		t.Fatalf("checkout HEAD = %s, want %s", got, head)
	}
	if fixture.git(fixture.checkout.path, "rev-parse", "refs/remotes/minion/base") != fixture.base {
		t.Fatal("base branch was not fetched")
	}
}

func TestCheckoutPullRequestBoundsAliasedTrees(t *testing.T) {
	for _, tt := range []struct {
		name      string
		width     int
		depth     int
		blobBytes int
		blocked   bool
	}{
		{name: "small tree", width: 4, depth: 2, blobBytes: 1024},
		{name: "expanded bytes", width: 8, depth: 2, blobBytes: workspace.MaxBytes/64 + 1, blocked: true},
		{name: "expanded paths", width: 11, depth: 4, blobBytes: 1, blocked: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newFixture(t)
			blobPath := filepath.Join(fixture.source, "aliased-blob")
			writeFile(t, blobPath, strings.Repeat("x", tt.blobBytes))
			blob := fixture.git(fixture.source, "hash-object", "-w", blobPath)
			if err := os.Remove(blobPath); err != nil {
				t.Fatal(err)
			}
			tree := fixture.gitInput(fixture.source, fmt.Sprintf("100644 blob %s\tpayload\n", blob), "mktree")
			for level := 0; level < tt.depth; level++ {
				var entries strings.Builder
				for i := 0; i < tt.width; i++ {
					fmt.Fprintf(&entries, "040000 tree %s\td%02d\n", tree, i)
				}
				tree = fixture.gitInput(fixture.source, entries.String(), "mktree")
			}
			head := fixture.gitInput(fixture.source, "aliased PR head\n", "commit-tree", tree, "-p", fixture.base)
			fixture.git(fixture.source, "push", fixture.remote, head+":refs/pull/7/head")

			entries := tt.width
			for level := 1; level < tt.depth; level++ {
				entries *= tt.width
			}
			lookup := func(ctx context.Context, sha string) (github.GitTree, error) {
				if !tt.blocked {
					return localTreeMetadata(t, fixture.source)(ctx, sha)
				}
				tree := completeTree(sha, make([]github.GitTreeEntry, 0, entries))
				for i := 0; i < entries; i++ {
					size := int64(tt.blobBytes)
					tree.Tree = append(tree.Tree, github.GitTreeEntry{Path: fmt.Sprintf("p%d", i), Type: "blob", Mode: "100644", Size: &size})
				}
				return tree, nil
			}
			fixture.checkout.lookupTree = lookup
			err := fixture.checkout.checkoutPullRequest(context.Background(), 7, "main", head, true)
			if tt.blocked {
				if !errors.Is(err, ErrCheckoutTooLarge) {
					t.Fatalf("CheckoutPullRequest() error = %v, want checkout limit", err)
				}
				if got := fixture.git(fixture.checkout.path, "rev-parse", "HEAD"); got != fixture.base {
					t.Fatalf("oversized tree checked out: HEAD = %s, want %s", got, fixture.base)
				}
			} else if err != nil {
				t.Fatalf("CheckoutPullRequest() error = %v", err)
			}
			path := fixture.checkout.path
			for level := 0; level < tt.depth; level++ {
				path = filepath.Join(path, "d00")
			}
			info, statErr := os.Stat(filepath.Join(path, "payload"))
			if tt.blocked {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("oversized tree materialized: %v", statErr)
				}
			} else if statErr != nil || info.Size() != int64(tt.blobBytes) {
				t.Fatalf("small tree checkout: size = %v, error = %v", info, statErr)
			}
		})
	}
}

func completeTree(sha string, entries []github.GitTreeEntry) github.GitTree {
	truncated := false
	return github.GitTree{SHA: sha, Truncated: &truncated, Tree: entries}
}

func TestSourceTreeRejectsIncompleteMetadata(t *testing.T) {
	sha := strings.Repeat("a", 40)
	size := int64(1)
	for _, tree := range []github.GitTree{
		{SHA: strings.Repeat("b", 40)},
		{SHA: sha},
		{SHA: sha, Tree: []github.GitTreeEntry{}},
		completeTree(sha, nil),
		completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "blob", Mode: "100644"}}),
		completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "blob", Mode: "100644", Size: &size}, {Path: "y", Type: "unknown"}}),
	} {
		if err := checkSourceTree(tree, sha); !errors.Is(err, ErrUnsupportedSource) {
			t.Fatalf("metadata %+v: error = %v, want checkout refusal", tree, err)
		}
	}
}

func TestSourceTreeAcceptsSymlinksAndRejectsInvalidModes(t *testing.T) {
	sha := strings.Repeat("a", 40)
	size := int64(len("../../../private"))
	for _, entry := range []github.GitTreeEntry{
		{Path: "missing-mode", Type: "blob", Size: &size},
		{Path: "unknown-mode", Type: "blob", Mode: "100664", Size: &size},
		{Path: "wrong-tree-mode", Type: "tree", Mode: "120000"},
		{Path: "wrong-gitlink-mode", Type: "commit", Mode: "100644"},
	} {
		tree := completeTree(sha, []github.GitTreeEntry{entry})
		if err := checkSourceTree(tree, sha); !errors.Is(err, ErrUnsupportedSource) {
			t.Errorf("entry %+v: error = %v, want checkout refusal", entry, err)
		}
	}
	for _, entry := range []github.GitTreeEntry{
		{Path: ".agents/skills/escape", Type: "blob", Mode: "120000", Size: &size},
		{Path: "ordinary", Type: "blob", Mode: "100644", Size: &size},
		{Path: "executable", Type: "blob", Mode: "100755", Size: &size},
		{Path: "directory", Type: "tree", Mode: "040000"},
		{Path: "submodule", Type: "commit", Mode: "160000"},
	} {
		tree := completeTree(sha, []github.GitTreeEntry{entry})
		if err := checkSourceTree(tree, sha); err != nil {
			t.Errorf("entry %+v: unexpected error: %v", entry, err)
		}
	}
}

func TestPullRequestSnapshotOmitsSymlinks(t *testing.T) {
	fixture := newFixture(t)
	linkDir := filepath.Join(fixture.source, ".agents", "skills")
	if err := os.MkdirAll(linkDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../private", filepath.Join(linkDir, "escape")); err != nil {
		t.Fatal(err)
	}
	fixture.git(fixture.source, "add", ".agents")
	fixture.git(fixture.source, "commit", "-m", "add skill link")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	if err := fixture.checkout.checkoutPullRequest(context.Background(), 7, "main", head, true); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.checkout.Snapshot(context.Background(), false)
	if err != nil || len(snapshot.Omitted) != 1 || snapshot.Omitted[0] != ".agents/skills/escape" {
		t.Fatalf("omitted=%v error=%v", snapshot.Omitted, err)
	}
	if snapshot.Head != head {
		t.Fatalf("snapshot HEAD=%s want %s", snapshot.Head, head)
	}
	for _, file := range snapshot.Files {
		if file.Path == ".agents/skills/escape" {
			t.Fatal("symlink exported")
		}
	}
}

func TestCheckoutPullRequestPreflightDoesNotFetchMissingBlob(t *testing.T) {
	fixture := newFixture(t)
	fixture.git("", "--git-dir="+fixture.remote, "config", "uploadpack.allowFilter", "true")
	const blobBytes = 2 << 20
	writeFile(t, filepath.Join(fixture.source, "new.dat"), strings.Repeat("x", blobBytes))
	fixture.git(fixture.source, "add", "new.dat")
	fixture.git(fixture.source, "commit", "-m", "new blob")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	blob := fixture.git(fixture.source, "rev-parse", "HEAD:new.dat")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	checkout, err := cloneFromURL(context.Background(), t.TempDir(), "file://"+fixture.remote, "main", localTreeMetadata(t, fixture.source), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	if !blobMissing(t, checkout.path, blob) {
		t.Fatal("fixture did not omit the PR blob")
	}
	oversized := func(_ context.Context, sha string) (github.GitTree, error) {
		size := int64(workspace.MaxBytes + 1)
		return completeTree(sha, []github.GitTreeEntry{{Path: "new.dat", Type: "blob", Mode: "100644", Size: &size}}), nil
	}
	checkout.lookupTree = oversized
	if err := checkout.checkoutPullRequest(context.Background(), 7, "main", head, true); !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("oversized PR error = %v, want checkout limit", err)
	}
	if !blobMissing(t, checkout.path, blob) {
		t.Fatal("size preflight downloaded missing PR blob")
	}
	if got := fixture.git(checkout.path, "rev-parse", "HEAD"); got != fixture.base {
		t.Fatalf("rejected PR moved HEAD to %s", got)
	}
	checkout.lookupTree = localTreeMetadata(t, fixture.source)
	if err := checkout.checkoutPullRequest(context.Background(), 7, "main", head, true); err != nil {
		t.Fatalf("bounded PR checkout: %v", err)
	}
	if info, err := os.Stat(filepath.Join(checkout.path, "new.dat")); err != nil || info.Size() != blobBytes {
		t.Fatalf("bounded PR file size = %v, error = %v", info, err)
	}
}

func TestCheckoutPullRequestDisablesRepositoryTransforms(t *testing.T) {
	fixture := newFixture(t)
	attributes := "payload.txt ident text eol=crlf working-tree-encoding=UTF-16LE\n"
	payload := strings.Repeat("$Id$\n", 1024)
	writeFile(t, filepath.Join(fixture.source, "payload.txt"), payload)
	fixture.git(fixture.source, "add", "payload.txt")
	writeFile(t, filepath.Join(fixture.source, ".gitattributes"), attributes)
	fixture.git(fixture.source, "add", ".gitattributes")
	fixture.git(fixture.source, "commit", "-m", "force checkout transformations")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")

	checkout, err := cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", localTreeMetadata(t, fixture.source), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := checkout.checkoutPullRequest(context.Background(), 7, "main", head, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(checkout.path, "payload.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("checkout transformed %d blob bytes into %d worktree bytes", len(payload), len(got))
	}
}

func TestNewBranchPreservesOmittedGitlink(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.source, "update-index", "--add", "--cacheinfo", "160000,"+fixture.base+",sub")
	fixture.git(fixture.source, "commit", "-m", "add gitlink")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")

	checkout, err := cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", localTreeMetadata(t, fixture.source), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	snapshot, err := checkout.Snapshot(ctx, false)
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
	checkout.pushURL = fixture.remote
	if err := checkout.CommitAndPush(ctx, branch, "bot", "fix", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := fixture.git("", "--git-dir="+fixture.remote, "ls-tree", "refs/heads/"+branch, "sub"); got != "160000 commit "+fixture.base+"\tsub" {
		t.Fatalf("published gitlink changed: %q", got)
	}
}

func TestImportRejectsNestedGitMetadataBeforePublishing(t *testing.T) {
	for _, name := range []string{"sub/.git/config", "sub/.GIT/config", ".git/config"} {
		t.Run(name, func(t *testing.T) {
			fixture := newFixture(t)
			ctx := context.Background()
			const branch = "minion/fix-nested-git"
			if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
				t.Fatal(err)
			}
			changes := []workspace.Change{{Path: name, Mode: workspace.Regular, Data: []byte("[filter \"probe\"]\nclean = malicious-command\n")}, {Path: "payload.txt", Mode: workspace.Regular, Data: []byte("new data")}}
			if err := fixture.checkout.Import(ctx, fixture.base, changes); err == nil {
				t.Fatal("accepted nested Git metadata")
			}
			if _, err := os.Stat(filepath.Join(fixture.checkout.path, "payload.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid import partially applied: %v", err)
			}
			if err := fixture.checkout.CommitAndPush(ctx, branch, "bot", "fix", func(string) error { t.Fatal("invalid data reached publication"); return nil }); !errors.Is(err, ErrNoChanges) {
				t.Fatalf("publication after rejected import=%v", err)
			}
		})
	}
}

func TestCommitAndPushUsesTrustedURLAndSkipsHooks(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	const branch = "minion/fix-123"
	if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkout.Import(ctx, fixture.base, []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("fixed\n")}}); err != nil {
		t.Fatal(err)
	}
	// These hooks would fail a normal commit or push.
	for _, name := range []string{"pre-commit", "pre-push"} {
		hook := filepath.Join(fixture.checkout.path, ".git", "hooks", name)
		writeFile(t, hook, "#!/bin/sh\nexit 1\n")
		if err := os.Chmod(hook, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var recordedHead string
	if err := fixture.checkout.CommitAndPush(ctx, branch, "example-bot", "Minion: fix issue 123", func(sha string) error {
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
	if err := fixture.checkout.CommitAndPush(ctx, branch, "example-bot", "no changes", func(string) error { t.Fatal("unchanged tree reached push"); return nil }); !errors.Is(err, ErrNoChanges) {
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

func TestGitErrorLabelsCommandAfterConfigOptions(t *testing.T) {
	fixture := newFixture(t)
	_, err := runGit(context.Background(), fixture.checkout.path,
		"-c", "user.name=example-bot", "-c", "user.email=example-bot@users.noreply.github.com",
		"commit", "--not-a-real-option")
	if err == nil || !strings.Contains(err.Error(), "git commit:") {
		t.Fatalf("runGit() error = %v, want git commit error", err)
	}
}

func TestImportAndCommitBoundsChanges(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"file count", "new bytes", "patch bytes"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newFixture(t)
			const branch = "minion/fix-limited"
			if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
				t.Fatal(err)
			}
			changes := []workspace.Change{{Path: "large.txt", Mode: workspace.Regular, Data: []byte(strings.Repeat("x", workspace.MaxChangeBytes+1))}}
			if kind == "file count" {
				changes = nil
				for i := 0; i <= workspace.MaxChanges; i++ {
					changes = append(changes, workspace.Change{Path: fmt.Sprintf("file-%03d", i), Mode: workspace.Regular, Data: []byte("x")})
				}
			} else if kind == "patch bytes" {
				changes[0].Data = []byte(strings.Repeat("x\n", workspace.MaxChangeBytes/2-100))
			}
			err := fixture.checkout.Import(ctx, fixture.base, changes)
			if kind == "patch bytes" && err != nil {
				t.Fatal(err)
			}
			if kind != "patch bytes" && err == nil {
				t.Fatal("oversized import succeeded")
			}
			err = fixture.checkout.CommitAndPush(ctx, branch, "example-bot", "Minion: fix", func(string) error { t.Fatal("oversized changes reached before-push recorder"); return nil })
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
	err := fixture.checkout.CommitAndPush(ctx, branch, "bot", "too many paths", func(string) error {
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
		records, err := SplitNUL([]byte(test.data))
		if err != nil || len(records) != len(test.want) {
			t.Fatalf("split %q=%q,%v", test.data, records, err)
		}
		for i, want := range test.want {
			if string(records[i]) != want {
				t.Fatalf("split %q record %d=%q want=%q", test.data, i, records[i], want)
			}
		}
	}
	if _, err := SplitNUL([]byte("one\x00unterminated")); err == nil {
		t.Fatal("accepted unterminated records")
	}
}

func TestChangedGitConfigStopsPublish(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	if err := fixture.checkout.NewBranch(ctx, "minion/fix-config"); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(fixture.checkout.path, ".git", "config")
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
	if err := fixture.checkout.Import(ctx, fixture.base, []workspace.Change{{Path: "fix.txt", Mode: workspace.Regular, Data: []byte("fixed\n")}}); err != nil {
		t.Fatal(err)
	}
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
	fixture.checkout = &Checkout{path: path, url: fixture.remote, pushURL: fixture.remote, expectedHead: fixture.base, lookupTree: localTreeMetadata(t, path)}
	if err := DisableAttributes(filepath.Join(path, ".git")); err != nil {
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
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// shellQuote treats fixture paths as literal shell arguments.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
