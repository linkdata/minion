package gitwork

import (
	"bytes"
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

func TestReadBatchBlobsValidatesProtocol(t *testing.T) {
	sha := strings.Repeat("a", 40)
	entries := []sourceEntry{{file: workspace.File{Path: "binary", Mode: workspace.Executable}, sha: sha, size: 3}, {file: workspace.File{Path: "empty", Mode: workspace.Regular}, sha: sha, size: 0}}
	valid := []byte(sha + " blob 3\n\x00\n\xff\n" + sha + " blob 0\n\n")
	files, err := readBatchBlobs(valid, entries)
	if err != nil || len(files) != 2 || !bytes.Equal(files[0].Data, []byte{0, '\n', 255}) || files[0].Mode != workspace.Executable || len(files[1].Data) != 0 {
		t.Fatalf("batch files=%+v error=%v", files, err)
	}
	for name, data := range map[string][]byte{
		"missing object": []byte(sha + " missing\n"),
		"wrong object":   bytes.Replace(valid, []byte(sha), []byte(strings.Repeat("b", 40)), 1),
		"wrong type":     bytes.Replace(valid, []byte(" blob "), []byte(" tree "), 1),
		"larger size":    bytes.Replace(valid, []byte(" blob 3"), []byte(" blob 4"), 1),
		"smaller size":   bytes.Replace(valid, []byte(" blob 3"), []byte(" blob 2"), 1),
		"negative size":  bytes.Replace(valid, []byte(" blob 3"), []byte(" blob -1"), 1),
		"missing suffix": valid[:len(valid)-1],
		"extra output":   append(append([]byte{}, valid...), 'x'),
		"long header":    []byte(strings.Repeat("a", 129) + "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readBatchBlobs(data, entries); !errors.Is(err, ErrUnsupportedSource) {
				t.Fatalf("malformed batch accepted or misclassified: %v", err)
			}
		})
	}
}

func recordBatchCalls(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	log := filepath.Join(root, "batch-calls")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	shim := "#!/bin/sh\nfor arg do\nif [ \"$arg\" = --batch ]; then printf x >> " + quote(log) + "; fi\ndone\nexec " + quote(git) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(root, "git"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestSnapshotReadsHeadAndBaseInOneBatch(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "README.md"), "\x00head\n\xff")
	fixture.git(fixture.source, "add", "README.md")
	fixture.git(fixture.source, "commit", "-m", "binary PR")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	if err := fixture.checkout.CheckoutPullRequest(context.Background(), 7, "main", head); err != nil {
		t.Fatal(err)
	}
	log := recordBatchCalls(t)
	snapshot, err := fixture.checkout.Snapshot(context.Background(), false, true)
	if err != nil || len(snapshot.Files) != 1 || len(snapshot.BaseFiles) != 1 || string(snapshot.Files[0].Data) != "\x00head\n\xff" || string(snapshot.BaseFiles[0].Data) != "fixture\n" {
		t.Fatalf("batch snapshot=%+v error=%v", snapshot, err)
	}
	calls, err := os.ReadFile(log)
	if err != nil || string(calls) != "x" {
		t.Fatalf("cat-file batch invocations=%q error=%v", calls, err)
	}
}

func TestSourceFilesClassifiesMetadataMismatchAndCancellation(t *testing.T) {
	fixture := newFixture(t)
	sha := fixture.gitInput(fixture.checkout.path, strings.Repeat("x", 512), "hash-object", "-w", "--stdin")
	for _, size := range []int64{1, 1024} {
		_, _, err := fixture.checkout.sourceFiles(context.Background(), []sourceEntry{{file: workspace.File{Path: "data", Mode: workspace.Regular}, sha: sha, size: size}})
		if !errors.Is(err, ErrUnsupportedSource) || errors.Is(err, ErrCheckoutTooLarge) {
			t.Fatalf("declared size=%d mismatch error=%v", size, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := fixture.checkout.sourceFiles(ctx, []sourceEntry{{file: workspace.File{Path: "data", Mode: workspace.Regular}, sha: sha, size: 512}}); !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("cancelled batch error=%v", err)
	}
}

func TestImportRejectsGitMetadataAliasesAtStaging(t *testing.T) {
	fixture := newFixture(t)
	for _, name := range []string{"git~1/config", ".git./config", ".git:alternate/config", ".g\u200cit/config"} {
		t.Run(name, func(t *testing.T) {
			err := fixture.checkout.Import(context.Background(), fixture.base, []workspace.Change{{Path: name, Mode: workspace.Regular, Data: []byte("invalid metadata alias")}})
			if !errors.Is(err, ErrUnsupportedSource) || !strings.Contains(err.Error(), "invalid path") {
				t.Fatalf("alias %q import error=%v", name, err)
			}
			paths, err := fixture.checkout.stagedPaths(context.Background())
			if err != nil || len(paths) != 0 {
				t.Fatalf("rejected alias staged=%v error=%v", paths, err)
			}
			if err := os.RemoveAll(filepath.Join(fixture.checkout.path, strings.Split(name, "/")[0])); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshotPreflightsCombinedBytesAndChangedPaths(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(fixture.checkout.path, "README.md"), "changed\n")
	fixture.git(fixture.checkout.path, "add", "README.md")
	fixture.git(fixture.checkout.path, "commit", "-m", "PR")
	head := fixture.git(fixture.checkout.path, "rev-parse", "HEAD")
	fixture.checkout.expectedHead = head
	fixture.git(fixture.checkout.path, "update-ref", "refs/remotes/minion/base", fixture.base)
	blob := fixture.git(fixture.checkout.path, "rev-parse", "HEAD:README.md")
	log := recordBatchCalls(t)
	t.Run("combined byte budget", func(t *testing.T) {
		fixture.checkout.sourceTrees = map[string][]sourceEntry{}
		for _, ref := range []string{fixture.base, head} {
			fixture.checkout.sourceTrees[ref] = []sourceEntry{{file: workspace.File{Path: "README.md", Mode: workspace.Regular}, sha: blob, size: workspace.MaxBytes/2 + 1}}
		}
		if _, err := fixture.checkout.Snapshot(ctx, false, true); !errors.Is(err, ErrCheckoutTooLarge) || !strings.Contains(err.Error(), "combined") {
			t.Fatalf("combined budget error=%v", err)
		}
	})
	t.Run("changed path budget", func(t *testing.T) {
		tree := func(prefix string) string {
			var input strings.Builder
			for i := 0; i < workspace.MaxFiles/2+1; i++ {
				fmt.Fprintf(&input, "100644 blob %s\t%s-%05d\n", blob, prefix, i)
			}
			return fixture.gitInput(fixture.checkout.path, input.String(), "mktree")
		}
		base := fixture.gitInput(fixture.checkout.path, "base\n", "commit-tree", tree("old"), "-p", fixture.base)
		head := fixture.gitInput(fixture.checkout.path, "head\n", "commit-tree", tree("new"), "-p", base)
		fixture.git(fixture.checkout.path, "update-ref", "HEAD", head)
		fixture.git(fixture.checkout.path, "update-ref", "refs/remotes/minion/base", base)
		fixture.checkout.expectedHead = head
		fixture.checkout.sourceTrees = nil
		if _, err := fixture.checkout.Snapshot(ctx, false, true); !errors.Is(err, ErrCheckoutTooLarge) {
			t.Fatalf("changed path budget error=%v", err)
		}
	})
	if data, err := os.ReadFile(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight requested blob batch: %q error=%v", data, err)
	}
}

func TestSourcePreparationBoundsIncludeOmittedLinks(t *testing.T) {
	sha := strings.Repeat("a", 40)
	size := int64(workspace.MaxBytes + 1)
	if err := checkSourceTree(completeTree(sha, []github.GitTreeEntry{{Path: "link", Type: "blob", Mode: "120000", Size: &size}}), sha); !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("oversized omitted symlink error=%v", err)
	}
	entries := make([]github.GitTreeEntry, workspace.MaxFiles+1)
	for i := range entries {
		entries[i] = github.GitTreeEntry{Path: fmt.Sprintf("sub-%d", i), Type: "commit", Mode: "160000"}
	}
	if err := checkSourceTree(completeTree(sha, entries), sha); !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("oversized gitlink list error=%v", err)
	}
}
