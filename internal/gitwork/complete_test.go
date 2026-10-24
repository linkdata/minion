package gitwork

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func assertBlobPresent(fixture gitFixture, dir, sha string) {
	fixture.t.Helper()
	fixture.git(dir, "cat-file", "-e", sha)
}

func TestCloneRejectsOversizedLocalTree(t *testing.T) {
	fixture := newFixture(t)
	name := filepath.Join(fixture.source, "blob")
	writeSparse(t, name, workspace.MaxBytes+1)
	fixture.commit("refs/heads/main", "oversized local blob")
	root := t.TempDir()
	checkout, err := cloneFromURL(t.Context(), root, "file://"+fixture.remote, "main", true)
	if checkout != nil || !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("oversized source accepted: checkout=%v error=%v", checkout, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("failed clone retained checkout: %v,%v", entries, err)
	}
}

func TestSnapshotPreflightsMergeBaseMetadata(t *testing.T) {
	fixture := newFixture(t)
	name := filepath.Join(fixture.source, "old")
	writeSparse(t, name, workspace.MaxBytes+1)
	base := fixture.commit("refs/heads/main", "historical oversized blob")
	blob := fixture.git(fixture.source, "rev-parse", "HEAD:old")
	fixture.git(fixture.source, "rm", "old")
	fixture.commit("refs/heads/main", "remove old blob")
	checkout := fixture.keepCheckout(cloneFromURL(t.Context(), t.TempDir(), "file://"+fixture.remote, "main", true))
	assertBlobPresent(fixture, checkout.path, blob)
	checkout.comparisonBase = base
	log := recordSnapshotContentCalls(t)
	if _, err := checkout.Snapshot(t.Context()); !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("merge-base limit: %v", err)
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
			snapshot, err := fixture.checkout.Snapshot(t.Context())
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
