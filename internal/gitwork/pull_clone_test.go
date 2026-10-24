package gitwork

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestClonePullRequestFetchesOnlyPinnedHeadAfterBaseClone(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "README.md"), "PR head\n")
	head := fixture.commit("refs/pull/7/head", "PR head")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "git-calls")
	fakeOnPath(t, "git", `#!/bin/sh
for argument do
  case "$argument" in
    clone) printf '%s\n' "$*" >> `+shellQuote(log)+`; break ;;
    fetch)
      printf '%s\n' "$*" >> `+shellQuote(log)+`
      cat .git/config > `+shellQuote(log+".before")+`
      `+shellQuote(git)+` "$@"
      result=$?
      cat .git/config > `+shellQuote(log+".after")+`
      exit "$result"
      ;;
  esac
done
exec `+shellQuote(git)+` "$@"
`)
	checkout := fixture.clonePullRequest(head)
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(calls) != 2 || !strings.Contains(calls[0], "clone ") || !strings.Contains(calls[0], "--branch main") ||
		!strings.Contains(calls[1], "fetch ") || !strings.Contains(calls[1], "-- origin +refs/pull/7/head:refs/remotes/minion/pr-head") || strings.Contains(calls[1], "+refs/heads/") {
		t.Fatalf("base fetched again or pinned head omitted: %q", calls)
	}
	for _, restricted := range []string{"--filter", "--single-branch", "--no-tags", "--depth", "--shallow"} {
		if strings.Contains(calls[0], restricted) {
			t.Fatalf("clone restricted repository contents with %s: %q", restricted, calls[0])
		}
	}
	if !strings.Contains(calls[0], "--reject-shallow") {
		t.Fatalf("clone accepted shallow repositories: %q", calls[0])
	}
	before, err := os.ReadFile(log + ".before")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(log + ".after")
	if err != nil || string(before) != string(after) {
		t.Fatalf("PR fetch changed trusted config: before=%q after=%q error=%v", before, after, err)
	}
	if checkout.comparisonBase != fixture.base || checkout.expectedHead != head {
		t.Fatalf("PR identity changed: base=%s head=%s", checkout.comparisonBase, checkout.expectedHead)
	}
}

func TestSnapshotRejectsUnrelatedPullRequestHistories(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.source, "checkout", "--orphan", "unrelated")
	fixture.git(fixture.source, "commit", "-m", "unrelated root")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	checkout := fixture.keepCheckout(clonePullRequestFromURL(t.Context(), t.TempDir(), fixture.remote, 7, "main", head))
	_, err := checkout.Snapshot(t.Context())
	if !errors.Is(err, ErrUnsupportedSource) || !strings.Contains(err.Error(), "no common ancestor") {
		t.Fatalf("unrelated histories were retryable: %v", err)
	}
}

func TestClonePullRequestSkipsUnusedBaseTipPreflight(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "README.md"), "PR head\n")
	head := fixture.commit("refs/pull/7/head", "PR head")
	fixture.git(fixture.source, "checkout", "--detach", fixture.base)
	unused := "base-only"
	path := filepath.Join(fixture.source, unused)
	writeSparse(t, path, workspace.MaxBytes+1)
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "advance base")
	base := fixture.git(fixture.source, "rev-parse", "HEAD")
	blob := fixture.git(fixture.source, "rev-parse", "HEAD:"+unused)
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	url := "file://" + fixture.remote
	if checkout, err := cloneFromURL(t.Context(), t.TempDir(), url, "main", true); !errors.Is(err, ErrCheckoutTooLarge) {
		if checkout != nil {
			_ = checkout.Close()
		}
		t.Fatalf("default clone error=%v want=%v", err, ErrCheckoutTooLarge)
	}
	checkout := fixture.keepCheckout(clonePullRequestFromURL(t.Context(), t.TempDir(), url, 7, "main", head))
	snapshot, err := checkout.Snapshot(t.Context())
	if err != nil || snapshot.Head != head || snapshot.Base != base || snapshot.MergeBase != fixture.base || len(snapshot.Files) != 1 || string(snapshot.Files[0].Data) != "PR head\n" {
		t.Fatalf("PR snapshot=%+v error=%v", snapshot, err)
	}
	if _, err := os.Lstat(filepath.Join(checkout.path, unused)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unused base path materialized: %v", err)
	}
	source, _ := repositoryFixture(t, snapshot)
	assertModelRepositoryGit(t, source, head+"\n", "rev-parse", "HEAD")
	assertModelRepositoryGit(t, source, head+"\n", "rev-parse", "refs/remotes/minion/pr-head")
	assertModelRepositoryGit(t, source, base+"\n", "rev-parse", "refs/remotes/origin/main")
	assertModelRepositoryGit(t, source, fixture.base+"\n", "merge-base", "HEAD", "refs/remotes/origin/main")
	assertModelRepositoryGit(t, source, blob+"\n", "rev-parse", "refs/remotes/origin/main:"+unused)
}

func TestPullRequestRejectsDeletedMergeBaseMetadataAliasBeforeContent(t *testing.T) {
	const name = "git~1/hooks/x"
	fixture := newFixture(t)
	path := filepath.Join(fixture.source, filepath.FromSlash(name))
	writeFile(t, path, strings.Repeat("alias blob", 1024))
	fixture.git(fixture.source, "-c", "core.protectNTFS=false", "-c", "core.protectHFS=false", "add", ".")
	fixture.git(fixture.source, "commit", "-m", "unsupported merge base")
	blob := fixture.git(fixture.source, "rev-parse", "HEAD:"+name)
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fixture.git(fixture.source, "-c", "core.protectNTFS=false", "-c", "core.protectHFS=false", "add", "--all")
	fixture.git(fixture.source, "commit", "-m", "remove unsupported path")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	checkout := fixture.keepCheckout(clonePullRequestFromURL(t.Context(), t.TempDir(), "file://"+fixture.remote, 7, "main", head))
	assertBlobPresent(fixture, checkout.path, blob)
	if err := os.Rename(fixture.remote, fixture.remote+".offline"); err != nil {
		t.Fatal(err)
	}
	log := recordSnapshotContentCalls(t)
	if _, err := checkout.Snapshot(t.Context()); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("merge-base alias accepted: %v", err)
	}
	if data, err := os.ReadFile(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("alias preflight requested source content: %q error=%v", data, err)
	}
}

func TestPullRequestInputGatesBeforeClone(t *testing.T) {
	for _, tt := range []struct {
		name       string
		number     int
		base, head string
		want       string
	}{
		{"zero number", 0, "main", strings.Repeat("a", 40), "invalid pull request number 0"},
		{"refspec injection", 1, "main:refs/heads/other", strings.Repeat("a", 40), "invalid branch \"main:refs/heads/other\""},
		{"invalid SHA", 1, "main", "missing", "invalid expected pull request SHA \"missing\""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if c, err := clonePullRequestFromURL(t.Context(), root, "file://"+filepath.Join(t.TempDir(), "missing.git"), tt.number, tt.base, tt.head); err == nil || c != nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("invalid input accepted: %v,%v", c, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid input created clone: %v,%v", entries, err)
			}
		})
	}
}

func TestNewBranchRejectsInvalidNameBeforeGit(t *testing.T) {
	f := newFixture(t)
	before := f.git(f.checkout.path, "for-each-ref", "--format=%(refname) %(objectname)")
	const name = "bad:ref"
	if err := f.checkout.NewBranch(t.Context(), name); err == nil || !strings.Contains(err.Error(), "invalid branch \"bad:ref\"") {
		t.Fatalf("invalid branch error=%v", err)
	}
	if after := f.git(f.checkout.path, "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
		t.Fatalf("invalid branch changed refs: %s", after)
	}
	if head := f.git(f.checkout.path, "rev-parse", "HEAD"); head != f.base {
		t.Fatalf("invalid branch moved HEAD: %s", head)
	}
	cmd := exec.Command("git", "-C", f.checkout.path, "symbolic-ref", "--quiet", "HEAD")
	cmd.Env = cleanGitEnvironment()
	if output, err := cmd.Output(); err == nil || len(output) != 0 {
		t.Fatalf("invalid branch attached HEAD: %q, %v", output, err)
	}
}

func TestClonePullRequestRemovesCheckoutAfterHeadMoves(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Join(f.source, "change.txt"), "new PR head\n")
	head := f.commit("refs/pull/7/head", "PR head")
	root := t.TempDir()
	checkout, err := clonePullRequestFromURL(t.Context(), root, f.remote, 7, "main", f.base)
	if checkout != nil || err == nil || !strings.Contains(err.Error(), "pull request head changed: expected "+f.base+", fetched "+head) {
		t.Fatalf("moved head returned checkout=%v error=%v", checkout, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed PR preparation retained checkout: %v, %v", entries, err)
	}
}
