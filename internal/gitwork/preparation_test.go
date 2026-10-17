package gitwork

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestReadOnlyCloneSnapshotsWithoutMaterializedWorktree(t *testing.T) {
	for _, pull := range []bool{false, true} {
		t.Run(fmt.Sprintf("pull=%t", pull), func(t *testing.T) {
			fixture := newFixture(t)
			head := fixture.base
			if pull {
				writeFile(t, filepath.Join(fixture.source, "README.md"), "pull request source\n")
				fixture.git(fixture.source, "add", ".")
				fixture.git(fixture.source, "commit", "-m", "PR source")
				head = fixture.git(fixture.source, "rev-parse", "HEAD")
				fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
			}
			lookup := localTreeMetadata(fixture.source)
			var checkout *Checkout
			var err error
			if pull {
				checkout, err = clonePullRequestFromURL(context.Background(), t.TempDir(), fixture.remote, 7, "main", head, lookup)
			} else {
				checkout, err = cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", lookup, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := checkout.Close(); err != nil {
					t.Error(err)
				}
			})
			assertNoWorktree := func() {
				t.Helper()
				entries, err := os.ReadDir(checkout.path)
				if err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
					t.Fatalf("read-only preparation materialized source: entries=%v error=%v", entries, err)
				}
			}
			assertNoWorktree()
			snapshot, err := checkout.Snapshot(context.Background(), true)
			if err != nil || snapshot.Head != head || len(snapshot.Files) != 1 || snapshot.Files[0].Path != "README.md" || !snapshot.HistoryRequested || !snapshot.HistoryComplete {
				t.Fatalf("metadata-only snapshot failed: head=%s files=%d history=%t/%t error=%v", snapshot.Head, len(snapshot.Files), snapshot.HistoryRequested, snapshot.HistoryComplete, err)
			}
			if pull {
				if snapshot.Base != fixture.base || snapshot.MergeBase != fixture.base || len(snapshot.BaseFiles) != 1 || string(snapshot.BaseFiles[0].Data) != "fixture\n" || string(snapshot.Files[0].Data) != "pull request source\n" {
					t.Fatalf("metadata-only PR lost pinned comparison: base=%s merge-base=%s base-files=%d", snapshot.Base, snapshot.MergeBase, len(snapshot.BaseFiles))
				}
			} else if snapshot.Base != "" || snapshot.MergeBase != "" || len(snapshot.BaseFiles) != 0 || string(snapshot.Files[0].Data) != "fixture\n" {
				t.Fatalf("default snapshot unexpectedly compared another revision: base=%s merge-base=%s base-files=%d", snapshot.Base, snapshot.MergeBase, len(snapshot.BaseFiles))
			}
			if err := workspace.ValidateSnapshot(snapshot); err != nil {
				t.Fatalf("metadata-only snapshot violates transfer contract: %v", err)
			}
			assertNoWorktree()
			fixture.git(checkout.path, "config", "minion.test", "changed")
			if _, err := checkout.Snapshot(context.Background(), false); err == nil || !strings.Contains(err.Error(), "Git config changed") {
				t.Fatalf("read-only checkout stopped verifying its config: %v", err)
			}
		})
	}
}

func TestPullSnapshotPreservesSpecialComparisonPaths(t *testing.T) {
	fixture := newFixture(t)
	names := []string{"-dash", " leading", "trailing ", "tab\tname", "line\nname", `quote"name`, `literal\backslash`, "café"}
	for _, name := range names {
		writeFile(t, filepath.Join(fixture.source, name), "merge-base contents\n")
	}
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "comparison names")
	base := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	for i, name := range names {
		if i == 0 {
			if err := os.Remove(filepath.Join(fixture.source, name)); err != nil {
				t.Fatal(err)
			}
		} else {
			writeFile(t, filepath.Join(fixture.source, name), "head contents\n")
		}
	}
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "change comparison names")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	checkout, err := clonePullRequestFromURL(context.Background(), t.TempDir(), fixture.remote, 7, "main", head, localTreeMetadata(fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	})
	snapshot, err := checkout.Snapshot(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Sorted(slices.Values(names))
	if snapshot.Base != base || snapshot.MergeBase != base || !slices.Equal(snapshot.ChangedPaths, want) || len(snapshot.BaseFiles) != len(names) {
		t.Fatalf("comparison paths changed spelling: changed=%q base-files=%d base=%s merge-base=%s", snapshot.ChangedPaths, len(snapshot.BaseFiles), snapshot.Base, snapshot.MergeBase)
	}
	for i, file := range snapshot.BaseFiles {
		if file.Path != want[i] || string(file.Data) != "merge-base contents\n" {
			t.Fatalf("base path=%q contents=%q; want path=%q", file.Path, file.Data, want[i])
		}
	}
	if !bytes.Contains(snapshot.Diff, []byte("café")) || bytes.Contains(snapshot.Diff, []byte(`caf\303\251`)) || !bytes.Contains(snapshot.Diff, []byte(`line\nname`)) {
		t.Fatalf("comparison text did not preserve UTF-8 and quote controls: %q", snapshot.Diff)
	}
}

func TestSnapshotAcceptsExactDiffByteLimit(t *testing.T) {
	fixture := newFixture(t)
	checkout := fixture.checkout
	checkout.comparisonBase = fixture.base
	const name = "one-line.txt"
	writeFile(t, filepath.Join(checkout.path, name), "x\n")
	fixture.git(checkout.path, "add", ".")
	fixture.git(checkout.path, "commit", "-m", "measure diff envelope")
	checkout.expectedHead = fixture.git(checkout.path, "rev-parse", "HEAD")
	small, err := checkout.Snapshot(context.Background(), false)
	if err != nil || small.DiffIncomplete || len(small.Diff) == 0 {
		t.Fatalf("small diff control failed: bytes=%d incomplete=%t error=%v", len(small.Diff), small.DiffIncomplete, err)
	}
	padding := workspace.MaxContextBytes - len(small.Diff)
	for _, extra := range []int{0, 1} {
		writeFile(t, filepath.Join(checkout.path, name), strings.Repeat("x", 1+padding+extra)+"\n")
		fixture.git(checkout.path, "add", ".")
		fixture.git(checkout.path, "commit", "-m", "bounded comparison")
		checkout.expectedHead = fixture.git(checkout.path, "rev-parse", "HEAD")
		snapshot, err := checkout.Snapshot(context.Background(), false)
		if err != nil || snapshot.DiffIncomplete != (extra == 1) {
			t.Fatalf("exact/+1 comparison boundary: extra=%d bytes=%d incomplete=%t error=%v", extra, len(snapshot.Diff), snapshot.DiffIncomplete, err)
		}
		if extra == 0 && len(snapshot.Diff) != workspace.MaxContextBytes || extra == 1 && len(snapshot.Diff) != 0 {
			t.Fatalf("comparison boundary retained incorrect bytes: extra=%d bytes=%d", extra, len(snapshot.Diff))
		}
	}
}

func TestSnapshotAcceptsExactCombinedSourceByteLimit(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.source, "rm", "README.md")
	data := bytes.Repeat([]byte{'a', 0}, workspace.MaxBytes/4)
	filename := filepath.Join(fixture.source, "payload.bin")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "half-budget merge-base file")
	base := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	for i := 0; i < len(data); i += 2 {
		data[i] = 'b'
	}
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "half-budget head file")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	checkout, err := clonePullRequestFromURL(context.Background(), t.TempDir(), fixture.remote, 7, "main", head, localTreeMetadata(fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	})
	snapshot, err := checkout.Snapshot(context.Background(), false)
	if err != nil || snapshot.Base != base || len(snapshot.Files) != 1 || len(snapshot.BaseFiles) != 1 {
		t.Fatalf("exact combined transfer failed: head-files=%d base-files=%d base=%s error=%v", len(snapshot.Files), len(snapshot.BaseFiles), snapshot.Base, err)
	}
	if len(snapshot.Files[0].Data)+len(snapshot.BaseFiles[0].Data) != workspace.MaxBytes || !bytes.Equal(snapshot.Files[0].Data, data) {
		t.Fatalf("exact combined transfer lost head bytes: head=%d base=%d", len(snapshot.Files[0].Data), len(snapshot.BaseFiles[0].Data))
	}
	for i := 0; i < len(data); i += 2 {
		data[i] = 'a'
	}
	if !bytes.Equal(snapshot.BaseFiles[0].Data, data) {
		t.Fatalf("exact combined transfer lost merge-base bytes: bytes=%d", len(snapshot.BaseFiles[0].Data))
	}
	if err := workspace.ValidateSnapshot(snapshot); err != nil {
		t.Fatalf("64 MiB snapshot violates protocol boundary: %v", err)
	}
	if entries, err := os.ReadDir(checkout.path); err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("large read-only preparation materialized a second worktree: %v,%v", entries, err)
	}
}
