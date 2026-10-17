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

func blobMissing(t *testing.T, dir, sha string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "cat-file", "-e", sha)
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	return cmd.Run() != nil
}

func TestPartialClonePreflightsMetadataBeforeAnyBlobFetch(t *testing.T) {
	fixture := newFixture(t)
	fixture.git("", "--git-dir="+fixture.remote, "config", "uploadpack.allowFilter", "true")
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
			if !blobMissing(t, filepath.Join(root, dirs[0].Name()), blob) {
				t.Fatal("metadata preflight already downloaded a blob")
			}
		}
		checked = true
		tree, err := localTreeMetadata(t, fixture.source)(ctx, sha)
		if err == nil {
			oversized := int64(workspace.MaxBytes + 1)
			tree.Tree[0].Size = &oversized
		}
		return tree, err
	}
	_, err := cloneFromURL(context.Background(), root, "file://"+fixture.remote, "main", lookup)
	if !checked || !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("preflight checked=%v error=%v", checked, err)
	}
}

func TestPartialSnapshotBulkFetchesChangedBaseBlobs(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.remote, "config", "uploadpack.allowFilter", "true")
	const count = 20
	for i := 0; i < count; i++ {
		writeFile(t, filepath.Join(fixture.source, fmt.Sprintf("file-%02d", i)), fmt.Sprintf("base %02d\n", i))
	}
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "base files")
	base := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	for i := 0; i < count; i++ {
		writeFile(t, filepath.Join(fixture.source, fmt.Sprintf("file-%02d", i)), fmt.Sprintf("head %02d\n", i))
	}
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "PR files")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	checkout, err := clonePullRequestFromURL(context.Background(), t.TempDir(), "file://"+fixture.remote, 7, "main", head, localTreeMetadata(t, fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < count; i++ {
		blob := fixture.git(fixture.source, "rev-parse", fmt.Sprintf("%s:file-%02d", base, i))
		if !blobMissing(t, checkout.path, blob) {
			t.Fatal("fixture already fetched a base blob")
		}
	}
	root := t.TempDir()
	log, upload := filepath.Join(root, "fetches"), filepath.Join(root, "upload-pack")
	gitUpload, err := exec.LookPath("git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf x >> " + shellQuote(log) + "\nexec " + shellQuote(gitUpload) + " \"$@\"\n"
	if err := os.WriteFile(upload, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	fixture.git(checkout.path, "config", "remote.origin.uploadpack", upload)
	if err := checkout.recordConfig(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := checkout.Snapshot(context.Background(), false, true)
	if err != nil || snapshot.Head != head || snapshot.MergeBase != base || len(snapshot.BaseFiles) != count || len(snapshot.ChangedPaths) != count {
		t.Fatalf("partial snapshot base=%s files=%d paths=%d error=%v", snapshot.MergeBase, len(snapshot.BaseFiles), len(snapshot.ChangedPaths), err)
	}
	for i, file := range snapshot.BaseFiles {
		if file.Path != fmt.Sprintf("file-%02d", i) || string(file.Data) != fmt.Sprintf("base %02d\n", i) {
			t.Fatalf("wrong base blob %q: %q", file.Path, file.Data)
		}
	}
	sessions, err := os.ReadFile(log)
	if err != nil || string(sessions) != "x" {
		t.Fatalf("Snapshot fetched missing base blobs in %d sessions, want one: %q error=%v", len(sessions), sessions, err)
	}
}

func TestPartialSnapshotPreflightsMergeBaseAndHistoryDoesNotFetchOldBlobs(t *testing.T) {
	fixture := newFixture(t)
	fixture.git("", "--git-dir="+fixture.remote, "config", "uploadpack.allowFilter", "true")
	writeFile(t, filepath.Join(fixture.source, "old"), strings.Repeat("old", 1<<20))
	fixture.git(fixture.source, "add", "old")
	fixture.git(fixture.source, "commit", "-m", "historical blob")
	base := fixture.git(fixture.source, "rev-parse", "HEAD")
	baseTree := fixture.git(fixture.source, "rev-parse", "HEAD^{tree}")
	blob := fixture.git(fixture.source, "rev-parse", "HEAD:old")
	fixture.git(fixture.source, "rm", "old")
	fixture.git(fixture.source, "commit", "-m", "remove old blob")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	lookup := localTreeMetadata(t, fixture.source)
	checkout, err := cloneFromURL(context.Background(), t.TempDir(), "file://"+fixture.remote, "main", lookup)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	if !blobMissing(t, checkout.path, blob) {
		t.Fatal("partial clone fetched deleted historical content")
	}
	snapshot, err := checkout.Snapshot(context.Background(), true, false)
	if err != nil || !snapshot.HistoryComplete || !strings.Contains(string(snapshot.History), "historical blob") || !blobMissing(t, checkout.path, blob) {
		t.Fatalf("metadata history fetched old contents or lost coverage: complete=%v err=%v", snapshot.HistoryComplete, err)
	}
	fixture.git(checkout.path, "update-ref", "refs/remotes/minion/base", base)
	checkout.lookupTree = func(ctx context.Context, sha string) (github.GitTree, error) {
		tree, err := lookup(ctx, sha)
		if sha == baseTree && err == nil {
			if !blobMissing(t, checkout.path, blob) {
				t.Fatal("merge-base blob fetched before metadata check")
			}
			oversized := int64(workspace.MaxBytes + 1)
			tree.Tree[0].Size = &oversized
		}
		return tree, err
	}
	if _, err := checkout.Snapshot(context.Background(), false, true); !errors.Is(err, ErrCheckoutTooLarge) || !blobMissing(t, checkout.path, blob) {
		t.Fatalf("merge-base preflight fetched blob: %v", err)
	}
}

func TestSnapshotDeclaresOversizedComparisonAndPreservesFiles(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "large"), strings.Repeat("x\n", workspace.MaxContextBytes/2))
	fixture.git(fixture.source, "add", "large")
	fixture.git(fixture.source, "commit", "-m", "large diff")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	if err := fixture.checkout.checkoutPullRequest(context.Background(), 7, "main", head); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.checkout.Snapshot(context.Background(), false, true)
	if err != nil || !snapshot.DiffIncomplete || len(snapshot.Diff) != 0 || len(snapshot.ChangedPaths) != 1 || snapshot.ChangedPaths[0] != "large" || len(snapshot.Files) != 2 {
		t.Fatalf("large comparison incomplete=%v paths=%v files=%d error=%v", snapshot.DiffIncomplete, snapshot.ChangedPaths, len(snapshot.Files), err)
	}
}
