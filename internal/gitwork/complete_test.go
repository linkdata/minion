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

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/workspace"
)

func assertBlobPresent(fixture gitFixture, dir, sha string) {
	fixture.t.Helper()
	fixture.git(dir, "cat-file", "-e", sha)
}

func TestClonePreflightsMetadataAfterCompleteClone(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "blob"), "complete local blob\n")
	fixture.commit("refs/heads/main", "new blob")
	blob := fixture.git(fixture.source, "rev-parse", "HEAD:blob")
	root := t.TempDir()
	checked := false
	lookup := func(ctx context.Context, sha string) (github.GitTree, error) {
		dirs, err := os.ReadDir(root)
		if err != nil || len(dirs) != 1 {
			t.Fatalf("clone roots=%v,%v", dirs, err)
		}
		assertBlobPresent(fixture, filepath.Join(root, dirs[0].Name()), blob)
		checked = true
		tree, err := localTreeMetadata(fixture.source)(ctx, sha)
		if err == nil {
			oversized := int64(workspace.MaxBytes + 1)
			tree.Tree[0].Size = &oversized
		}
		return tree, err
	}
	_, err := cloneFromURL(context.Background(), root, "file://"+fixture.remote, "main", lookup, true)
	if !checked || !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("preflight checked=%v error=%v", checked, err)
	}
}

func TestSnapshotPreflightsMergeBaseMetadata(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "old"), "historical local blob\n")
	fixture.git(fixture.source, "add", "old")
	fixture.git(fixture.source, "commit", "-m", "historical blob")
	base := fixture.git(fixture.source, "rev-parse", "HEAD")
	baseTree := fixture.git(fixture.source, "rev-parse", "HEAD^{tree}")
	blob := fixture.git(fixture.source, "rev-parse", "HEAD:old")
	fixture.git(fixture.source, "rm", "old")
	fixture.git(fixture.source, "commit", "-m", "remove old blob")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	lookup := localTreeMetadata(fixture.source)
	checkout := fixture.keepCheckout(cloneFromURL(context.Background(), t.TempDir(), "file://"+fixture.remote, "main", lookup, true))
	assertBlobPresent(fixture, checkout.path, blob)
	checkout.comparisonBase = base
	checkout.lookupTree = func(ctx context.Context, sha string) (github.GitTree, error) {
		tree, err := lookup(ctx, sha)
		if sha == baseTree && err == nil {
			oversized := int64(workspace.MaxBytes + 1)
			tree.Tree[0].Size = &oversized
		}
		return tree, err
	}
	log := recordSnapshotContentCalls(t)
	if _, err := checkout.Snapshot(context.Background()); !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("merge-base preflight error: %v", err)
	}
	if data, err := os.ReadFile(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata preflight requested content: %q error=%v", data, err)
	}
}

func TestSnapshotBoundsBundleBeforeReadingSource(t *testing.T) {
	for _, size := range []int{workspace.MaxBytes, workspace.MaxBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			fixture := newFixture(t)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "source-read")
			script := fmt.Sprintf("#!/bin/sh\nfor argument do\ncase \"$argument\" in bundle) bundle=yes;; --batch) touch %s;; esac\ndone\nif [ \"$bundle\" = yes ]; then head -c %d /dev/zero; exit; fi\nexec %s \"$@\"\n", shellQuote(marker), size, shellQuote(realGit))
			fakeOnPath(t, "git", script)
			snapshot, err := fixture.checkout.Snapshot(context.Background())
			if size == workspace.MaxBytes {
				if err != nil || len(snapshot.Repository) != size || len(snapshot.Files) == 0 {
					t.Fatalf("exact bundle limit: bytes=%d files=%d error=%v", len(snapshot.Repository), len(snapshot.Files), err)
				}
				return
			}
			if !errors.Is(err, ErrCheckoutTooLarge) || !strings.Contains(err.Error(), "export complete repository") || len(snapshot.Repository) != 0 || len(snapshot.Files) != 0 {
				t.Fatalf("oversized bundle was not refused before source read: repository bytes=%d files=%d error=%v", len(snapshot.Repository), len(snapshot.Files), err)
			}
			if data, err := os.ReadFile(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("oversized bundle read source blobs: marker=%q error=%v", data, err)
			}
		})
	}
}
