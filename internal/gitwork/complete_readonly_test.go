package gitwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func readOnlyFixture(t *testing.T, pull, link bool) (gitFixture, *Checkout, string, string) {
	t.Helper()
	fixture := newFixture(t)
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
	url := "file://" + fixture.remote
	if pull {
		checkout, err = clonePullRequestFromURL(context.Background(), t.TempDir(), url, 7, "main", head)
	} else {
		checkout, err = cloneFromURL(context.Background(), t.TempDir(), url, "main", false)
	}
	checkout = fixture.keepCheckout(checkout, err)
	config, err := os.ReadFile(filepath.Join(checkout.path, ".git", "config"))
	if err != nil || bytes.Contains(config, []byte("promisor")) || bytes.Contains(config, []byte("partialclonefilter")) {
		t.Fatalf("fixture is not a complete clone: config=%s error=%v", config, err)
	}
	return fixture, checkout, base, head
}

func assertReadOnlyState(t *testing.T, checkout *Checkout, config []byte) {
	t.Helper()
	entries, err := os.ReadDir(checkout.path)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("snapshot materialized a worktree: %v error=%v", entries, err)
	}
	got, err := os.ReadFile(filepath.Join(checkout.path, ".git", "config"))
	if err != nil || !bytes.Equal(got, config) {
		t.Fatalf("snapshot mutated trusted config: error=%v", err)
	}
	if err := checkout.ready(); err != nil {
		t.Fatalf("config assurance failed after snapshot: %v", err)
	}
}

func TestReadOnlySnapshotUsesLocalObjectsWithoutOrigin(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pull, link bool
	}{
		{"default", false, false}, {"default omitted link", false, true},
		{"PR regular files", true, false}, {"PR omitted link", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, checkout, base, head := readOnlyFixture(t, tc.pull, tc.link)
			if err := os.Rename(fixture.remote, fixture.remote+".offline"); err != nil {
				t.Fatal(err)
			}
			config, err := os.ReadFile(filepath.Join(checkout.path, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			assertReadOnlyState(t, checkout, config)
			snapshot, err := checkout.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
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
			assertReadOnlyState(t, checkout, config)
			second, err := checkout.Snapshot(context.Background())
			if err != nil || len(second.Repository) == 0 {
				t.Fatalf("second snapshot failed: repository bytes=%d error=%v", len(second.Repository), err)
			}
			second.Repository, snapshot.Repository = nil, nil
			if !reflect.DeepEqual(second, snapshot) {
				t.Fatal("repeated snapshot changed source or comparison")
			}
			assertReadOnlyState(t, checkout, config)
		})
	}
}

func TestReadOnlySnapshotPreflightsCombinedBytesBeforeContent(t *testing.T) {
	fixture, checkout, base, head := readOnlyFixture(t, true, false)
	if err := os.Rename(fixture.remote, fixture.remote+".offline"); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(checkout.path, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{head, base} {
		entries, err := checkout.sourceTree(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for i := range entries {
			if entries[i].file.Path == "changed-00" {
				entries[i].size = workspace.MaxBytes/2 + 1
				found = true
			}
		}
		if !found {
			t.Fatalf("missing comparison entry at %s", ref)
		}
		checkout.sourceTrees[ref] = entries
	}
	log := recordSnapshotContentCalls(t)
	_, err = checkout.Snapshot(context.Background())
	if !errors.Is(err, ErrCheckoutTooLarge) || !strings.Contains(err.Error(), "combined") {
		t.Fatalf("combined metadata preflight failed for an unrelated reason: %v", err)
	}
	for _, spec := range []string{head + ":unchanged-00", head + ":changed-00", base + ":changed-00", base + ":deleted-00"} {
		assertBlobPresent(fixture, checkout.path, fixture.git(fixture.source, "rev-parse", spec))
	}
	assertReadOnlyState(t, checkout, config)
	if data, err := os.ReadFile(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata preflight requested blob contents or rendered diff: %q error=%v", data, err)
	}
}
