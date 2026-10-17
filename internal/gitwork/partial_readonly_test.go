package gitwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/workspace"
)

func partialReadOnlyFixture(t *testing.T, pull, link bool) (gitFixture, *Checkout, string, string) {
	t.Helper()
	fixture := newFixture(t)
	fixture.git(fixture.remote, "config", "uploadpack.allowFilter", "true")
	for _, group := range []struct {
		name  string
		count int
	}{{"unchanged", 20}, {"changed", 3}, {"deleted", 2}} {
		for i := 0; i < group.count; i++ {
			name := fmt.Sprintf("%s-%02d", group.name, i)
			writeFile(t, filepath.Join(fixture.source, name), "base "+name+"\n")
		}
	}
	if link {
		if err := os.Symlink("base-target", filepath.Join(fixture.source, "omitted-link")); err != nil {
			t.Fatal(err)
		}
	}
	base := fixture.commit("refs/heads/main", "base files")
	head := base
	if pull {
		for i := 0; i < 3; i++ {
			name := fmt.Sprintf("changed-%02d", i)
			writeFile(t, filepath.Join(fixture.source, name), "head "+name+"\n")
		}
		for i := 0; i < 2; i++ {
			fixture.git(fixture.source, "rm", fmt.Sprintf("deleted-%02d", i))
		}
		if link {
			if err := os.Remove(filepath.Join(fixture.source, "omitted-link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("head-target", filepath.Join(fixture.source, "omitted-link")); err != nil {
				t.Fatal(err)
			}
		}
		head = fixture.commit("refs/pull/7/head", "PR changes")
	}
	var checkout *Checkout
	var err error
	// file:// is necessary: a local path clone ignores --filter and hides this regression.
	url := "file://" + fixture.remote
	if pull {
		checkout, err = clonePullRequestFromURL(context.Background(), t.TempDir(), url, 7, "main", head, localTreeMetadata(fixture.source))
	} else {
		checkout, err = cloneFromURL(context.Background(), t.TempDir(), url, "main", localTreeMetadata(fixture.source), false)
	}
	checkout = fixture.keepCheckout(checkout, err)
	if got := fixture.git(checkout.path, "config", "remote.origin.promisor"); got != "true" {
		t.Fatalf("fixture is not partial: promisor=%q", got)
	}
	if got := fixture.git(checkout.path, "config", "remote.origin.partialclonefilter"); got != "blob:none" {
		t.Fatalf("fixture lost blob filter: %q", got)
	}
	return fixture, checkout, base, head
}

func partialFetchSessions(t *testing.T, fixture gitFixture, checkout *Checkout) func() int {
	t.Helper()
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
	return func() int {
		data, err := os.ReadFile(log)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, bytes.Repeat([]byte("x"), len(data))) {
			t.Fatal("invalid upload-pack session log")
		}
		return len(data)
	}
}

func assertPartialReadOnlyState(t *testing.T, checkout *Checkout, config []byte) {
	t.Helper()
	entries, err := os.ReadDir(checkout.path)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("snapshot materialized a worktree: %v error=%v", entries, err)
	}
	got, err := os.ReadFile(filepath.Join(checkout.path, ".git", "config"))
	if err != nil || !bytes.Equal(got, config) {
		t.Fatalf("snapshot fetch mutated trusted config: error=%v", err)
	}
	if err := checkout.ready(); err != nil {
		t.Fatalf("config assurance failed after snapshot: %v", err)
	}
}

func TestPartialReadOnlySnapshotBulkFetchesBeforeDiff(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pull, link bool
		sessions   int
	}{
		{"default", false, false, 1}, {"default omitted link", false, true, 1},
		{"PR regular files", true, false, 1}, {"PR omitted link", true, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, checkout, base, head := partialReadOnlyFixture(t, tc.pull, tc.link)
			// Distinct unchanged content defeats object deduplication; all selected
			// old versions are absent too, including deleted paths.
			for _, spec := range []string{head + ":README.md", head + ":unchanged-00", head + ":unchanged-19", base + ":changed-00", base + ":deleted-00"} {
				sha := fixture.git(fixture.source, "rev-parse", spec)
				if !blobMissing(t, checkout.path, sha) {
					t.Fatalf("partial fixture already fetched %s", spec)
				}
			}
			sessions := partialFetchSessions(t, fixture, checkout)
			config, err := os.ReadFile(filepath.Join(checkout.path, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			assertPartialReadOnlyState(t, checkout, config)
			snapshot, err := checkout.Snapshot(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			if got := sessions(); got != tc.sessions {
				t.Fatalf("Snapshot used %d upload-pack sessions, want %d", got, tc.sessions)
			}
			wantHead, wantBase := 26, 0
			if tc.pull {
				wantHead, wantBase = 24, 5
			}
			if snapshot.Head != head || len(snapshot.Files) != wantHead || len(snapshot.BaseFiles) != wantBase {
				t.Fatalf("snapshot head=%s regular=%d base=%d", snapshot.Head, len(snapshot.Files), len(snapshot.BaseFiles))
			}
			if tc.pull && (snapshot.Base != base || snapshot.MergeBase != base || len(snapshot.ChangedPaths) != 5+len(snapshot.Omitted)) {
				t.Fatalf("PR comparison lost: base=%s merge-base=%s changed=%q", snapshot.Base, snapshot.MergeBase, snapshot.ChangedPaths)
			}
			if !tc.pull && (snapshot.Base != "" || snapshot.MergeBase != "") {
				t.Fatalf("default snapshot unexpectedly compared another revision: base=%s merge-base=%s", snapshot.Base, snapshot.MergeBase)
			}
			if tc.link && (len(snapshot.Omitted) != 1 || snapshot.Omitted[0] != "omitted-link") {
				t.Fatalf("omitted links=%q", snapshot.Omitted)
			}
			for _, file := range snapshot.Files {
				want := "base " + file.Path + "\n"
				if file.Path == "README.md" {
					want = "fixture\n"
				} else if tc.pull && strings.HasPrefix(file.Path, "changed-") {
					want = "head " + file.Path + "\n"
				}
				if string(file.Data) != want {
					t.Fatalf("wrong head contents at %s", file.Path)
				}
			}
			for _, file := range snapshot.BaseFiles {
				if string(file.Data) != "base "+file.Path+"\n" {
					t.Fatalf("wrong selected base contents at %s", file.Path)
				}
			}
			if err := workspace.ValidateSnapshot(snapshot); err != nil {
				t.Fatal(err)
			}
			assertPartialReadOnlyState(t, checkout, config)
			second, err := checkout.Snapshot(context.Background(), true)
			if err != nil || !second.HistoryRequested || !second.HistoryComplete || len(second.History) == 0 || len(second.History) > workspace.MaxContextBytes {
				t.Fatalf("history snapshot failed: requested=%t complete=%t bytes=%d error=%v", second.HistoryRequested, second.HistoryComplete, len(second.History), err)
			}
			second.History, second.HistoryRequested, second.HistoryComplete = nil, false, false
			if !reflect.DeepEqual(second, snapshot) || sessions() != tc.sessions {
				t.Fatalf("history changed source/comparison or contacted origin: sessions=%d", sessions())
			}
			assertPartialReadOnlyState(t, checkout, config)
		})
	}
}

func TestPartialReadOnlySnapshotRefusesCombinedBytesBeforeBulkFetch(t *testing.T) {
	fixture, checkout, base, head := partialReadOnlyFixture(t, true, false)
	sessions := partialFetchSessions(t, fixture, checkout)
	config, err := os.ReadFile(filepath.Join(checkout.path, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	lookup := localTreeMetadata(fixture.source)
	checkout.sourceTrees = nil
	checkout.lookupTree = func(ctx context.Context, sha string) (github.GitTree, error) {
		tree, err := lookup(ctx, sha)
		if err == nil {
			for i := range tree.Tree {
				if tree.Tree[i].Path == "changed-00" {
					size := int64(workspace.MaxBytes/2 + 1)
					tree.Tree[i].Size = &size
				}
			}
		}
		return tree, err
	}
	log := recordSnapshotContentCalls(t)
	_, err = checkout.Snapshot(context.Background(), false)
	if !errors.Is(err, ErrCheckoutTooLarge) || !strings.Contains(err.Error(), "combined") || sessions() != 0 {
		t.Fatalf("refusal fetched blob data: sessions=%d error=%v", sessions(), err)
	}
	for _, spec := range []string{head + ":unchanged-00", head + ":changed-00", base + ":changed-00", base + ":deleted-00"} {
		if !blobMissing(t, checkout.path, fixture.git(fixture.source, "rev-parse", spec)) {
			t.Fatalf("refused snapshot fetched %s", spec)
		}
	}
	assertPartialReadOnlyState(t, checkout, config)
	if data, err := os.ReadFile(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata preflight requested blob contents or rendered diff: %q error=%v", data, err)
	}
}

func TestLocalSnapshotBulkFetchKeepsNonpartialConfig(t *testing.T) {
	fixture := newFixture(t)
	sessions := partialFetchSessions(t, fixture, fixture.checkout)
	config, err := os.ReadFile(filepath.Join(fixture.checkout.path, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.checkout.Snapshot(context.Background(), false)
	if err != nil || len(snapshot.Files) != 1 || string(snapshot.Files[0].Data) != "fixture\n" || sessions() != 0 {
		t.Fatalf("local snapshot contacted origin or failed: sessions=%d files=%d error=%v", sessions(), len(snapshot.Files), err)
	}
	after, err := os.ReadFile(filepath.Join(fixture.checkout.path, ".git", "config"))
	if err != nil || !bytes.Equal(after, config) {
		t.Fatalf("local snapshot changed nonpartial config: %v", err)
	}
	if err := fixture.checkout.ready(); err != nil {
		t.Fatal(err)
	}
}
