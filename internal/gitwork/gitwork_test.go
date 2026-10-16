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
	"time"

	"github.com/linkdata/minion/internal/github"
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
	if _, err := Clone(context.Background(), t.TempDir(), "../bad", "main", nil); err == nil {
		t.Fatal("Clone accepted an invalid repository")
	}
	if _, err := Clone(context.Background(), t.TempDir(), "owner/repo", "bad..branch", nil); err == nil {
		t.Fatal("Clone accepted an invalid default branch")
	}
	if _, err := Clone(context.Background(), t.TempDir(), "owner/repo", "main", nil); err == nil {
		t.Fatal("Clone accepted missing tree metadata lookup")
	}
}

func TestCloneFromLocalFixtureIncludesBranchHistory(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.source, "commit", "--allow-empty", "-m", "next")
	tip := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	ctx := context.Background()
	checkout, err := cloneFromURL(ctx, t.TempDir(), "file://"+fixture.remote, "main", localTreeMetadata(t, fixture.source))
	if err != nil {
		t.Fatalf("cloneFromURL() error = %v", err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	if head := fixture.git(checkout.Path(), "rev-parse", "HEAD"); head != tip {
		t.Fatalf("clone HEAD = %s, want %s", head, tip)
	}
	if head, err := checkout.Head(ctx); err != nil || head != tip {
		t.Fatalf("Head() = (%q, %v), want %s", head, err, tip)
	}
	if count := fixture.git(checkout.Path(), "rev-list", "--count", "HEAD"); count != "2" {
		t.Fatalf("cloned history contains %s commits, want 2", count)
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

	if err := fixture.checkout.CheckoutPullRequest(ctx, 7, "main", strings.Repeat("f", 40), smallTree); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("mismatched PR head error = %v, want ErrHeadMoved", err)
	}
	if err := fixture.checkout.CheckoutPullRequest(ctx, 7, "main", head, localTreeMetadata(t, fixture.source)); err != nil {
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

func TestCheckoutPullRequestBoundsAliasedTrees(t *testing.T) {
	for _, tt := range []struct {
		name      string
		width     int
		depth     int
		blobBytes int
		blocked   bool
	}{
		{name: "small tree", width: 4, depth: 2, blobBytes: 1024},
		{name: "expanded bytes", width: 8, depth: 2, blobBytes: maxCheckoutBytes/64 + 1, blocked: true},
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
			err := fixture.checkout.CheckoutPullRequest(context.Background(), 7, "main", head, lookup)
			if tt.blocked {
				if !errors.Is(err, ErrCheckoutTooLarge) {
					t.Fatalf("CheckoutPullRequest() error = %v, want checkout limit", err)
				}
				if got := fixture.git(fixture.checkout.Path(), "rev-parse", "HEAD"); got != fixture.base {
					t.Fatalf("oversized tree checked out: HEAD = %s, want %s", got, fixture.base)
				}
			} else if err != nil {
				t.Fatalf("CheckoutPullRequest() error = %v", err)
			}
			path := fixture.checkout.Path()
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

func smallTree(_ context.Context, sha string) (github.GitTree, error) {
	size := int64(32)
	return completeTree(sha, []github.GitTreeEntry{{Path: "change.txt", Type: "blob", Mode: "100644", Size: &size}}), nil
}

func completeTree(sha string, entries []github.GitTreeEntry) github.GitTree {
	truncated := false
	return github.GitTree{SHA: sha, Truncated: &truncated, Tree: entries}
}

func TestCheckoutPullRequestRejectsIncompleteTreeMetadata(t *testing.T) {
	sha := strings.Repeat("a", 40)
	size := int64(1)
	truncated := true
	for _, tree := range []github.GitTree{
		{SHA: strings.Repeat("b", 40)},
		{SHA: sha, Truncated: &truncated},
		{SHA: sha},
		{SHA: sha, Tree: []github.GitTreeEntry{}},
		completeTree(sha, nil),
		completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "blob", Mode: "100644"}}),
		completeTree(sha, []github.GitTreeEntry{{Path: "x", Type: "blob", Mode: "100644", Size: &size}, {Path: "y", Type: "unknown"}}),
	} {
		if err := checkPullRequestTree(tree, sha); !errors.Is(err, ErrCheckoutTooLarge) {
			t.Fatalf("metadata %+v: error = %v, want checkout refusal", tree, err)
		}
	}
}

func TestPullRequestTreeRejectsSymlinks(t *testing.T) {
	sha := strings.Repeat("a", 40)
	size := int64(len("../../../private"))
	for _, entry := range []github.GitTreeEntry{
		{Path: ".agents/skills/escape", Type: "blob", Mode: "120000", Size: &size},
		{Path: "missing-mode", Type: "blob", Size: &size},
		{Path: "unknown-mode", Type: "blob", Mode: "100664", Size: &size},
		{Path: "wrong-tree-mode", Type: "tree", Mode: "120000"},
		{Path: "wrong-gitlink-mode", Type: "commit", Mode: "100644"},
	} {
		tree := completeTree(sha, []github.GitTreeEntry{entry})
		if err := checkPullRequestTree(tree, sha); !errors.Is(err, ErrCheckoutTooLarge) {
			t.Errorf("entry %+v: error = %v, want checkout refusal", entry, err)
		}
	}
	for _, entry := range []github.GitTreeEntry{
		{Path: "ordinary", Type: "blob", Mode: "100644", Size: &size},
		{Path: "executable", Type: "blob", Mode: "100755", Size: &size},
		{Path: "directory", Type: "tree", Mode: "040000"},
		{Path: "submodule", Type: "commit", Mode: "160000"},
	} {
		tree := completeTree(sha, []github.GitTreeEntry{entry})
		if err := checkPullRequestTree(tree, sha); err != nil {
			t.Errorf("entry %+v: unexpected error: %v", entry, err)
		}
	}
}

func TestPullRequestSymlinkRejectedBeforeCheckout(t *testing.T) {
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
	lookup := func(_ context.Context, sha string) (github.GitTree, error) {
		size := int64(len("../../../../private"))
		return completeTree(sha, []github.GitTreeEntry{{Path: ".agents/skills/escape", Type: "blob", Mode: "120000", Size: &size}}), nil
	}
	if err := fixture.checkout.CheckoutPullRequest(context.Background(), 7, "main", head, lookup); !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("symlink PR error = %v, want checkout refusal", err)
	}
	if got := fixture.git(fixture.checkout.Path(), "rev-parse", "HEAD"); got != fixture.base {
		t.Fatalf("refused PR changed checkout HEAD to %s", got)
	}
	if _, err := os.Lstat(filepath.Join(fixture.checkout.Path(), ".agents")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused PR materialized skill directory: %v", err)
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
	checkout, err := cloneFromURL(context.Background(), t.TempDir(), "file://"+fixture.remote, "main", localTreeMetadata(t, fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	missing := func() bool {
		cmd := exec.Command("git", "-C", checkout.Path(), "cat-file", "-e", blob)
		cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
		return cmd.Run() != nil
	}
	if !missing() {
		t.Fatal("fixture did not omit the PR blob")
	}
	oversized := func(_ context.Context, sha string) (github.GitTree, error) {
		size := int64(maxCheckoutBytes + 1)
		return completeTree(sha, []github.GitTreeEntry{{Path: "new.dat", Type: "blob", Mode: "100644", Size: &size}}), nil
	}
	if err := checkout.CheckoutPullRequest(context.Background(), 7, "main", head, oversized); !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("oversized PR error = %v, want checkout limit", err)
	}
	if !missing() {
		t.Fatal("size preflight downloaded missing PR blob")
	}
	if got := fixture.git(checkout.Path(), "rev-parse", "HEAD"); got != fixture.base {
		t.Fatalf("rejected PR moved HEAD to %s", got)
	}
	if err := checkout.CheckoutPullRequest(context.Background(), 7, "main", head, localTreeMetadata(t, fixture.source)); err != nil {
		t.Fatalf("bounded PR checkout: %v", err)
	}
	if info, err := os.Stat(filepath.Join(checkout.Path(), "new.dat")); err != nil || info.Size() != blobBytes {
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

	checkout, err := cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", localTreeMetadata(t, fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := checkout.CheckoutPullRequest(context.Background(), 7, "main", head, localTreeMetadata(t, fixture.source)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(checkout.Path(), "payload.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("checkout transformed %d blob bytes into %d worktree bytes", len(payload), len(got))
	}
}

func TestNewBranchRejectsTrackedGitlink(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.source, "update-index", "--add", "--cacheinfo", "160000,"+fixture.base+",sub")
	fixture.git(fixture.source, "commit", "-m", "add gitlink")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")

	checkout, err := cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", localTreeMetadata(t, fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := checkout.NewBranch(context.Background(), "minion/fix-gitlink"); err == nil || !strings.Contains(err.Error(), "gitlink") {
		t.Fatalf("NewBranch() error = %v, want gitlink refusal", err)
	}
	if got := fixture.git(checkout.Path(), "branch", "--list", "minion/fix-gitlink"); got != "" {
		t.Fatalf("NewBranch() created branch despite gitlink: %q", got)
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
	checkout, err := cloneFromURL(ctx, t.TempDir(), fixture.remote, "main", localTreeMetadata(t, fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	if changed, err := checkout.HasChanges(ctx); err != nil || changed {
		t.Fatalf("clean gitlink HasChanges() = (%t, %v)", changed, err)
	}
	const branch = "minion/fix-nested-git"
	// Bypass NewBranch's preflight to test the post-run metadata guard independently.
	fixture.git(checkout.Path(), "switch", "--create", branch)
	checkout.branch = branch
	if err := checkout.recordConfig(); err != nil {
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

	cleanCheckout, err := cloneFromURL(ctx, t.TempDir(), fixture.remote, "main", localTreeMetadata(t, fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanCheckout.Close(); err != nil {
			t.Error(err)
		}
	}()
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
	if err := rejectNestedGitMetadata(context.Background(), root, 2); err != nil {
		t.Fatalf("root Git metadata or symlink target was scanned: %v", err)
	}
	writeFile(t, filepath.Join(root, "second"), "x")
	if err := rejectNestedGitMetadata(context.Background(), root, 2); err == nil || !strings.Contains(err.Error(), "scan exceeds") {
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
	fixture.checkout = &Checkout{path: path, url: fixture.remote, pushURL: fixture.remote, expectedHead: fixture.base, lookupTree: localTreeMetadata(t, path)}
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
