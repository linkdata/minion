package gitwork

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestPullSnapshotPreservesSpecialComparisonPaths(t *testing.T) {
	fixture := newFixture(t)
	names := []string{"-dash", " leading", "trailing ", "tab\tname", "line\nname", `quote"name`, `literal\backslash`, "café"}
	for _, name := range names {
		writeFile(t, filepath.Join(fixture.source, name), "merge-base contents\n")
	}
	base := fixture.commit("refs/heads/main", "comparison names")
	for i, name := range names {
		if i == 0 {
			if err := os.Remove(filepath.Join(fixture.source, name)); err != nil {
				t.Fatal(err)
			}
		} else {
			writeFile(t, filepath.Join(fixture.source, name), "head contents\n")
		}
	}
	head := fixture.commit("refs/pull/7/head", "change comparison names")
	checkout := fixture.keepCheckout(clonePullRequestFromURL(context.Background(), t.TempDir(), fixture.remote, 7, "main", head))
	snapshot, err := checkout.Snapshot(context.Background())
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
	small, err := checkout.Snapshot(context.Background())
	if err != nil || small.DiffIncomplete || len(small.Diff) == 0 {
		t.Fatalf("small diff control failed: bytes=%d incomplete=%t error=%v", len(small.Diff), small.DiffIncomplete, err)
	}
	padding := workspace.MaxContextBytes - len(small.Diff)
	for _, extra := range []int{0, 1} {
		writeFile(t, filepath.Join(checkout.path, name), strings.Repeat("x", 1+padding+extra)+"\n")
		fixture.git(checkout.path, "add", ".")
		fixture.git(checkout.path, "commit", "-m", "bounded comparison")
		checkout.expectedHead = fixture.git(checkout.path, "rev-parse", "HEAD")
		snapshot, err := checkout.Snapshot(context.Background())
		if err != nil || snapshot.DiffIncomplete != (extra == 1) {
			t.Fatalf("exact/+1 comparison boundary: extra=%d bytes=%d incomplete=%t error=%v", extra, len(snapshot.Diff), snapshot.DiffIncomplete, err)
		}
		if len(snapshot.ChangedPaths) != 1 || snapshot.ChangedPaths[0] != name || len(snapshot.Files) != 2 {
			t.Fatalf("comparison lost paths or source: extra=%d paths=%v files=%d", extra, snapshot.ChangedPaths, len(snapshot.Files))
		}
		found := false
		for _, file := range snapshot.Files {
			if file.Path == name {
				found = string(file.Data) == strings.Repeat("x", 1+padding+extra)+"\n"
			}
		}
		if !found {
			t.Fatalf("comparison lost complete current file at boundary: extra=%d", extra)
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
	base := fixture.commit("refs/heads/main", "half-budget merge-base file")
	for i := 0; i < len(data); i += 2 {
		data[i] = 'b'
	}
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	head := fixture.commit("refs/pull/7/head", "half-budget head file")
	checkout := fixture.keepCheckout(clonePullRequestFromURL(context.Background(), t.TempDir(), fixture.remote, 7, "main", head))
	snapshot, err := checkout.Snapshot(context.Background())
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
