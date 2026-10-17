package gitwork

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/workspace"
)

func TestSnapshotRejectsUnrelatedPullRequestHistories(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.source, "checkout", "--orphan", "unrelated")
	fixture.git(fixture.source, "commit", "-m", "unrelated root")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	checkout := fixture.keepCheckout(clonePullRequestFromURL(context.Background(), t.TempDir(), fixture.remote, 7, "main", head, localTreeMetadata(fixture.source)))
	_, err := checkout.Snapshot(context.Background(), false)
	if !errors.Is(err, ErrUnsupportedSource) || !strings.Contains(err.Error(), "no common ancestor") {
		t.Fatalf("unrelated histories were retryable: %v", err)
	}
}

func TestClonePullRequestSkipsUnusedBaseTip(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "README.md"), "PR head\n")
	head := fixture.commit("refs/pull/7/head", "PR head")
	fixture.git(fixture.source, "checkout", "--detach", fixture.base)
	unused := "base-only"
	path := filepath.Join(fixture.source, filepath.FromSlash(unused))
	writeFile(t, path, "unused base-tip blob\n")
	fixture.git(fixture.source, "-c", "core.protectHFS=false", "-c", "core.protectNTFS=false", "add", ".")
	fixture.git(fixture.source, "commit", "-m", "advance base")
	base := fixture.git(fixture.source, "rev-parse", "HEAD")
	baseTree := fixture.git(fixture.source, "rev-parse", "HEAD^{tree}")
	unusedBlob := fixture.git(fixture.source, "rev-parse", "HEAD:"+unused)
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	fixture.git(fixture.remote, "config", "uploadpack.allowFilter", "true")
	lookup := localTreeMetadata(fixture.source)
	baseLookups := 0
	metadata := func(ctx context.Context, sha string) (github.GitTree, error) {
		tree, err := lookup(ctx, sha)
		if sha == baseTree {
			baseLookups++
			for i := range tree.Tree {
				if tree.Tree[i].Path == unused {
					size := int64(workspace.MaxBytes + 1)
					tree.Tree[i].Size = &size
				}
			}
		}
		return tree, err
	}
	url := "file://" + fixture.remote
	if checkout, err := cloneFromURL(context.Background(), t.TempDir(), url, "main", metadata, true); !errors.Is(err, ErrCheckoutTooLarge) {
		if checkout != nil {
			_ = checkout.Close()
		}
		t.Fatalf("default clone error=%v want=%v", err, ErrCheckoutTooLarge)
	}
	if baseLookups != 1 {
		t.Fatalf("default clone skipped base preflight: lookups=%d", baseLookups)
	}
	checkout := fixture.keepCheckout(clonePullRequestFromURL(context.Background(), t.TempDir(), url, 7, "main", head, metadata))
	snapshot, err := checkout.Snapshot(context.Background(), false)
	if err != nil || snapshot.Head != head || snapshot.Base != base || snapshot.MergeBase != fixture.base || len(snapshot.Files) != 1 || string(snapshot.Files[0].Data) != "PR head\n" {
		t.Fatalf("PR snapshot=%+v error=%v", snapshot, err)
	}
	if baseLookups != 1 {
		t.Fatalf("unused base tip was validated during PR preparation: lookups=%d", baseLookups)
	}
	if _, err := os.Lstat(filepath.Join(checkout.path, filepath.FromSlash(unused))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unused base path materialized: %v", err)
	}
	if missing := fixture.git(checkout.path, "rev-list", "--objects", "--missing=print", base); !strings.Contains(missing, "?"+unusedBlob) {
		t.Fatalf("unused base blob was fetched: %s", missing)
	}
}

func TestPullRequestRejectsDeletedMergeBaseMetadataAliasBeforeBlobFetch(t *testing.T) {
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
	fixture.git(fixture.remote, "config", "uploadpack.allowFilter", "true")
	checkout := fixture.keepCheckout(clonePullRequestFromURL(context.Background(), t.TempDir(), "file://"+fixture.remote, 7, "main", head, localTreeMetadata(fixture.source)))
	if !blobMissing(t, checkout.path, blob) {
		t.Fatal("fixture already fetched alias blob")
	}
	if _, err := checkout.Snapshot(context.Background(), false); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("merge-base alias accepted: %v", err)
	}
	if !blobMissing(t, checkout.path, blob) {
		t.Fatal("alias preflight fetched rejected blob")
	}
}

func TestPullRequestInputGatesBeforeClone(t *testing.T) {
	for _, tt := range []struct {
		name       string
		number     int
		base, head string
	}{
		{"zero number", 0, "main", strings.Repeat("a", 40)},
		{"refspec injection", 1, "main:refs/heads/other", strings.Repeat("a", 40)},
		{"invalid SHA", 1, "main", "missing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			lookup := func(context.Context, string) (github.GitTree, error) {
				t.Fatal("input refusal reached tree lookup")
				return github.GitTree{}, nil
			}
			if c, err := ClonePullRequest(context.Background(), root, "owner/repo", tt.number, tt.base, tt.head, lookup); err == nil || c != nil {
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
	for _, name := range []string{"bad:ref"} {
		if err := f.checkout.NewBranch(context.Background(), name); err == nil {
			t.Fatalf("invalid branch accepted: %q", name)
		}
	}
	if after := f.git(f.checkout.path, "for-each-ref", "--format=%(refname) %(objectname)"); after != before || f.checkout.branch != "" {
		t.Fatalf("invalid branch changed refs: %s", after)
	}
}
