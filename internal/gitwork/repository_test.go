package gitwork

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/workspace"
)

func TestRepositoryCarriesCompleteHistoryOffline(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "deleted"), "historical contents\n")
	fixture.git(fixture.source, "add", "deleted")
	fixture.git(fixture.source, "commit", "-m", "old contents")
	old := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "tag", "old-tag")
	fixture.git(fixture.source, "branch", "other")
	fixture.git(fixture.source, "rm", "deleted")
	fixture.git(fixture.source, "commit", "-m", "delete old contents")
	for i := 0; i < 101; i++ {
		fixture.git(fixture.source, "commit", "--allow-empty", "-m", "complete history")
	}
	fixture.git(fixture.source, "push", fixture.remote, "main", "other", "--tags")
	checkout := fixture.keepCheckout(cloneFromURL(context.Background(), t.TempDir(), "file://"+fixture.remote, "main", localTreeMetadata(fixture.source), false))
	// Preparation and all later inspection must work after the remote disappears.
	if err := os.Rename(fixture.remote, fixture.remote+".offline"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.Chmod(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Materialize(source, snapshot.Files); err != nil {
		t.Fatal(err)
	}
	repo, err := MaterializeRepository(context.Background(), source, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	index := &OfflineIndex{dir: filepath.Join(source, ".git"), source: source, binary: binary}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"show", old + ":deleted"}, "historical contents\n"},
		{[]string{"rev-list", "--count", "HEAD"}, "104\n"},
		{[]string{"rev-parse", "refs/remotes/origin/other"}, old + "\n"},
		{[]string{"rev-parse", "refs/tags/old-tag"}, old + "\n"},
		{[]string{"rev-parse", "--is-shallow-repository"}, "false\n"},
		{[]string{"remote"}, ""},
	} {
		out, err := index.run(context.Background(), 4096, nil, tt.args...)
		if err != nil || string(out) != tt.want {
			t.Fatalf("offline git %v=%q %v; want%q", tt.args, out, err, tt.want)
		}
	}
	config, err := os.ReadFile(filepath.Join(source, ".git", "config"))
	if err != nil || strings.Contains(string(config), fixture.remote) || strings.Contains(string(config), "promisor") {
		t.Fatalf("source config leaked: %s %v", config, err)
	}
	if _, err := os.Stat(filepath.Join(source, ".git", "hooks")); !os.IsNotExist(err) {
		t.Fatalf("hooks copied: %v", err)
	}
	t.Run("wrong merge base", func(t *testing.T) {
		wrong := snapshot
		wrong.Base, wrong.MergeBase = snapshot.Head, old
		root := t.TempDir()
		if err := os.Chmod(root, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := MaterializeRepository(context.Background(), root, wrong); err == nil || !strings.Contains(err.Error(), "merge-base differs") {
			t.Fatalf("mismatched comparison accepted or unrelated failure: %v", err)
		}
	})
	if err := repo.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".git", "HEAD"), []byte(old+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := repo.Verify(context.Background()); err == nil {
		t.Fatal("accepted mutated Git metadata")
	}
}

func TestRepositoryRejectsInvalidOrMismatchedInput(t *testing.T) {
	fixture := newFixture(t)
	original, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"missing", "malformed", "truncated", "oversized", "wrong head", "wrong contents", "wrong mode", "missing file", "extra file"} {
		t.Run(scenario, func(t *testing.T) {
			snapshot := original
			snapshot.Files = append([]workspace.File(nil), original.Files...)
			switch scenario {
			case "missing":
				snapshot.Repository = nil
			case "malformed":
				snapshot.Repository = []byte("not a bundle")
			case "truncated":
				snapshot.Repository = snapshot.Repository[:len(snapshot.Repository)-1]
			case "oversized":
				snapshot.Repository = make([]byte, workspace.MaxBytes+1)
			case "wrong mode":
				snapshot.Files[0].Mode = workspace.Executable
			case "wrong head":
				snapshot.Head = strings.Repeat("a", 40)
			case "wrong contents":
				snapshot.Files[0].Data = []byte("forged")
			case "missing file":
				snapshot.Files = nil
			case "extra file":
				snapshot.Files = append(snapshot.Files, workspace.File{Path: "extra", Mode: workspace.Regular})
			}
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := MaterializeRepository(context.Background(), root, snapshot); err == nil {
				t.Fatal("accepted invalid repository")
			}
		})
	}
}

func TestRepositoryOmitsLinksWithoutReportingDeletions(t *testing.T) {
	fixture := newFixture(t)
	if err := os.Symlink("../outside", filepath.Join(fixture.source, "link")); err != nil {
		t.Fatal(err)
	}
	fixture.commit("refs/heads/main", "omitted link")
	checkout := fixture.clone()
	snapshot, err := checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.Chmod(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Materialize(source, snapshot.Files); err != nil {
		t.Fatal(err)
	}
	repo, err := MaterializeRepository(context.Background(), source, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	index := &OfflineIndex{dir: filepath.Join(source, ".git"), source: source, binary: binary}
	out, err := index.run(context.Background(), 4096, nil, "--no-optional-locks", "status", "--porcelain")
	if err != nil || len(out) != 0 {
		t.Fatalf("omitted link appeared as a working change: %s %v", out, err)
	}
	if _, err := os.Lstat(filepath.Join(source, "link")); !os.IsNotExist(err) {
		t.Fatalf("link materialized: %v", err)
	}
	out, err = index.run(context.Background(), 4096, nil, "show", "HEAD:link")
	if err != nil || string(out) != "../outside" {
		t.Fatalf("historical link object missing: %q %v", out, err)
	}
	if err := repo.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryRejectsIncrementalBundle(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.source, "commit", "--allow-empty", "-m", "new commit")
	bundle, _, err := (&Checkout{path: fixture.source}).boundedGit(context.Background(), nil, workspace.MaxBytes, false, "bundle", "create", "-", fixture.base+"..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Repository = bundle
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeRepository(context.Background(), root, snapshot); err == nil || !strings.Contains(err.Error(), "prerequisite") {
		t.Fatalf("incremental bundle accepted or failed for unrelated reason: %v", err)
	}
}
