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

func TestClonePullRequestSkipsUnusedBaseTip(t *testing.T) {
	for _, kind := range []string{"oversized", "unsupported"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newFixture(t)
			writeFile(t, filepath.Join(fixture.source, "README.md"), "PR head\n")
			fixture.git(fixture.source, "add", ".")
			fixture.git(fixture.source, "commit", "-m", "PR head")
			head := fixture.git(fixture.source, "rev-parse", "HEAD")
			fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
			fixture.git(fixture.source, "checkout", "--detach", fixture.base)
			unused := "base-only"
			want := ErrCheckoutTooLarge
			if kind == "unsupported" {
				unused, want = "git~1/hooks/test", ErrUnsupportedSource
			}
			path := filepath.Join(fixture.source, filepath.FromSlash(unused))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, path, "unused base-tip blob\n")
			fixture.git(fixture.source, "-c", "core.protectHFS=false", "-c", "core.protectNTFS=false", "add", ".")
			fixture.git(fixture.source, "commit", "-m", "advance base")
			base := fixture.git(fixture.source, "rev-parse", "HEAD")
			baseTree := fixture.git(fixture.source, "rev-parse", "HEAD^{tree}")
			unusedBlob := fixture.git(fixture.source, "rev-parse", "HEAD:"+unused)
			fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
			fixture.git(fixture.remote, "config", "uploadpack.allowFilter", "true")
			lookup := localTreeMetadata(t, fixture.source)
			baseLookups := 0
			metadata := func(ctx context.Context, sha string) (github.GitTree, error) {
				tree, err := lookup(ctx, sha)
				if sha == baseTree {
					baseLookups++
					if kind == "oversized" {
						for i := range tree.Tree {
							if tree.Tree[i].Path == unused {
								size := int64(workspace.MaxBytes + 1)
								tree.Tree[i].Size = &size
							}
						}
					}
				}
				return tree, err
			}
			url := "file://" + fixture.remote
			if checkout, err := cloneFromURL(context.Background(), t.TempDir(), url, "main", metadata); !errors.Is(err, want) {
				if checkout != nil {
					_ = checkout.Close()
				}
				t.Fatalf("default clone error=%v want=%v", err, want)
			}
			if baseLookups != 1 {
				t.Fatalf("default clone skipped base preflight: lookups=%d", baseLookups)
			}
			checkout, err := clonePullRequestFromURL(context.Background(), t.TempDir(), url, 7, "main", head, metadata)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = checkout.Close() })
			snapshot, err := checkout.Snapshot(context.Background(), false, true)
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
		})
	}
}
