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

func assertBlobPresent(t *testing.T, dir, sha string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "cat-file", "-e", sha)
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("complete clone is missing blob %s: %s: %v", sha, output, err)
	}
}

func TestClonePreflightsMetadataAfterCompleteClone(t *testing.T) {
	fixture := newFixture(t)
	var blobs []string
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("blob-%d", i)
		writeFile(t, filepath.Join(fixture.source, name), strings.Repeat(fmt.Sprint(i), 2<<20))
		fixture.git(fixture.source, "add", name)
	}
	fixture.git(fixture.source, "commit", "-m", "new blobs")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	for i := 0; i < 5; i++ {
		blobs = append(blobs, fixture.git(fixture.source, "rev-parse", fmt.Sprintf("HEAD:blob-%d", i)))
	}
	root := t.TempDir()
	checked := false
	lookup := func(ctx context.Context, sha string) (github.GitTree, error) {
		dirs, err := os.ReadDir(root)
		if err != nil || len(dirs) != 1 {
			t.Fatalf("clone roots=%v,%v", dirs, err)
		}
		for _, blob := range blobs {
			assertBlobPresent(t, filepath.Join(root, dirs[0].Name()), blob)
		}
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
	writeFile(t, filepath.Join(fixture.source, "old"), strings.Repeat("old", 1<<20))
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
	assertBlobPresent(t, checkout.path, blob)
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
