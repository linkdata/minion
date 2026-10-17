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

func TestBoundedGitClassifiesLimitsAndCancellation(t *testing.T) {
	fixture := newFixture(t)
	if _, _, err := fixture.checkout.boundedGit(context.Background(), nil, 1, false, "cat-file", "blob", fixture.base+":README.md"); !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("limit error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := fixture.checkout.boundedGit(ctx, nil, 1, false, "cat-file", "blob", fixture.base+":README.md"); !errors.Is(err, context.Canceled) || errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("canceled read=%v", err)
	}
}

func TestSnapshotUsesPinnedBlobsAndBoundedHistory(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.checkout.path, "README.md"), "untrusted working tree")
	writeFile(t, filepath.Join(fixture.checkout.path, "untracked"), "not source")
	snapshot, err := fixture.checkout.Snapshot(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != 1 || string(snapshot.Files[0].Data) != "fixture\n" || snapshot.Head != fixture.base || len(snapshot.History) != 0 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	withHistory, err := fixture.checkout.Snapshot(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !withHistory.HistoryComplete || !bytes.Contains(withHistory.History, []byte("initial")) {
		t.Fatalf("history=%q complete=%v", withHistory.History, withHistory.HistoryComplete)
	}
	for i := 0; i < 101; i++ {
		fixture.git(fixture.checkout.path, "commit", "--allow-empty", "-m", "more history")
		if i == 98 {
			fixture.checkout.expectedHead = fixture.git(fixture.checkout.path, "rev-parse", "HEAD")
			exact, err := fixture.checkout.Snapshot(context.Background(), true, false)
			if err != nil || !exact.HistoryComplete || !bytes.Contains(exact.History, []byte("initial")) {
				t.Fatalf("exact 100-commit history incomplete: %v", err)
			}
		}
	}
	fixture.checkout.expectedHead = fixture.git(fixture.checkout.path, "rev-parse", "HEAD")
	withHistory, err = fixture.checkout.Snapshot(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if withHistory.HistoryComplete || bytes.Contains(withHistory.History, []byte("initial")) {
		t.Fatal("bounded history claimed complete coverage")
	}
}

func TestSnapshotPreservesPRDeletedBinaryAndModeContext(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "deleted.txt"), "deleted base content\n")
	writeFile(t, filepath.Join(fixture.source, "binary"), "\x00\xffbase")
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "base files")
	base := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	if err := os.Remove(filepath.Join(fixture.source, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.source, "binary"), "\x00\xffhead")
	if err := os.Chmod(filepath.Join(fixture.source, "binary"), 0755); err != nil {
		t.Fatal(err)
	}
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "PR files")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	fixture.git(fixture.source, "checkout", "--detach", base)
	writeFile(t, filepath.Join(fixture.source, "base-only"), "new base branch content")
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "base advanced after fork")
	baseTip := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	if err := fixture.checkout.checkoutPullRequest(context.Background(), 7, "main", head); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.checkout.Snapshot(context.Background(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Head != head || snapshot.Base != baseTip || snapshot.MergeBase != base {
		t.Fatalf("snapshot commits=%+v", snapshot)
	}
	if !bytes.Contains(snapshot.Diff, []byte("Binary files")) || !bytes.Contains(snapshot.Diff, []byte("deleted.txt")) || !bytes.Contains(snapshot.Diff, []byte("100755")) {
		t.Fatalf("comparison diff=%s", snapshot.Diff)
	}
	old := map[string]workspace.File{}
	for _, file := range snapshot.BaseFiles {
		old[file.Path] = file
	}
	if string(old["deleted.txt"].Data) != "deleted base content\n" || string(old["binary"].Data) != "\x00\xffbase" {
		t.Fatalf("base context=%+v", snapshot.BaseFiles)
	}
	for _, file := range snapshot.Files {
		if file.Path == "binary" && (file.Mode != workspace.Executable || string(file.Data) != "\x00\xffhead") {
			t.Fatalf("binary source=%+v", file)
		}
	}
}

func TestSnapshotReportsOmittedGitlinksAndTruncatedHistory(t *testing.T) {
	fixture := newFixture(t)
	fixture.git(fixture.checkout.path, "update-index", "--add", "--cacheinfo", "160000,"+fixture.base+",dependency")
	writeFile(t, filepath.Join(fixture.checkout.path, "large"), "small source")
	fixture.git(fixture.checkout.path, "add", "large")
	fixture.git(fixture.checkout.path, "commit", "-m", "bounded context")
	fixture.gitInput(fixture.checkout.path, strings.Repeat("m", workspace.MaxContextBytes+256), "commit", "--allow-empty", "-F", "-")
	fixture.checkout.expectedHead = fixture.git(fixture.checkout.path, "rev-parse", "HEAD")
	snapshot, err := fixture.checkout.Snapshot(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Omitted) != 1 || snapshot.Omitted[0] != "dependency" {
		t.Fatalf("omitted=%v", snapshot.Omitted)
	}
	if snapshot.HistoryComplete || len(snapshot.History) != workspace.MaxContextBytes {
		t.Fatalf("history complete=%v bytes=%d", snapshot.HistoryComplete, len(snapshot.History))
	}
}

func TestCloneDefaultBranchDisablesRepositoryTransforms(t *testing.T) {
	fixture := newFixture(t)
	payload := strings.Repeat("$Id$\n", 128)
	writeFile(t, filepath.Join(fixture.source, "payload"), payload)
	fixture.git(fixture.source, "add", "payload")
	writeFile(t, filepath.Join(fixture.source, ".gitattributes"), "payload ident text eol=crlf working-tree-encoding=UTF-16LE\n")
	fixture.git(fixture.source, "add", ".gitattributes")
	fixture.git(fixture.source, "commit", "-m", "checkout transforms")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	checkout, err := cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", localTreeMetadata(t, fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	data, err := os.ReadFile(filepath.Join(checkout.path, "payload"))
	if err != nil || string(data) != payload {
		t.Fatalf("transformed default source: %q,%v", data, err)
	}
}

func TestImportRejectsIdentityAndMetadataAndPreservesBytes(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	if err := fixture.checkout.Import(ctx, strings.Repeat("f", 40), nil); err == nil {
		t.Fatal("accepted wrong source")
	}
	if err := fixture.checkout.Import(ctx, fixture.base, []workspace.Change{{Path: ".git/config", Mode: workspace.Regular}}); err == nil {
		t.Fatal("accepted Git metadata")
	}
	changes := []workspace.Change{{Path: "README.md", Delete: true}, {Path: "binary", Mode: workspace.Executable, Data: []byte{0, 255, 1}}}
	if err := fixture.checkout.Import(ctx, fixture.base, changes); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(fixture.checkout.path, "binary"))
	if err != nil || !bytes.Equal(data, changes[1].Data) {
		t.Fatalf("binary=%v,%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.checkout.path, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("deleted source=%v", err)
	}
}

func TestCloneOmitsDefaultBranchSymlinkFromSnapshot(t *testing.T) {
	fixture := newFixture(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(fixture.source, "escape")); err != nil {
		t.Fatal(err)
	}
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "symlink source")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	checkout, err := cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", localTreeMetadata(t, fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := checkout.Snapshot(context.Background(), false, false)
	if err != nil || len(snapshot.Omitted) != 1 || snapshot.Omitted[0] != "escape" {
		t.Fatalf("snapshot omitted=%v error=%v", snapshot.Omitted, err)
	}
	for _, file := range snapshot.Files {
		if file.Path == "escape" {
			t.Fatal("symlink content exported")
		}
	}
}

func TestSnapshotReusesValidatedTreesByCommit(t *testing.T) {
	fixture := newFixture(t)
	lookups := 0
	lookup := localTreeMetadata(t, fixture.source)
	checkout, err := cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", func(ctx context.Context, sha string) (github.GitTree, error) {
		lookups++
		return lookup(ctx, sha)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 2; i++ {
		if _, err := checkout.Snapshot(context.Background(), false, false); err != nil {
			t.Fatal(err)
		}
	}
	if lookups != 1 {
		t.Fatalf("default clone/snapshot metadata lookups=%d, want 1", lookups)
	}
	writeFile(t, filepath.Join(fixture.source, "README.md"), "PR source\n")
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "PR source")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	if err := checkout.checkoutPullRequest(context.Background(), 7, "main", head); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		snapshot, err := checkout.Snapshot(context.Background(), false, true)
		if err != nil || snapshot.Head != head || string(snapshot.Files[0].Data) != "PR source\n" {
			t.Fatalf("snapshot=%+v error=%v", snapshot, err)
		}
	}
	if lookups != 2 {
		t.Fatalf("PR checkout/snapshot metadata lookups=%d, want 2 total", lookups)
	}
}

func TestSnapshotPreservesOmittedPathSpellingsAcrossRevisions(t *testing.T) {
	fixture := newFixture(t)
	if err := os.Symlink("/outside", filepath.Join(fixture.source, "Docs")); err != nil {
		t.Fatal(err)
	}
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "base symlink")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/heads/main")
	checkout, err := cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", localTreeMetadata(t, fixture.source))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := os.Rename(filepath.Join(fixture.source, "Docs"), filepath.Join(fixture.source, "docs")); err != nil {
		t.Fatal(err)
	}
	fixture.git(fixture.source, "add", ".")
	fixture.git(fixture.source, "commit", "-m", "rename symlink")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	if err := checkout.checkoutPullRequest(context.Background(), 7, "main", head); err != nil {
		t.Fatal(err)
	}
	snapshot, err := checkout.Snapshot(context.Background(), false, true)
	if err != nil || len(snapshot.Omitted) != 2 || snapshot.Omitted[0] != "docs" || snapshot.Omitted[1] != "Docs" {
		t.Fatalf("omitted=%v error=%v", snapshot.Omitted, err)
	}
}

func TestPublishIgnoresDefaultGlobalExcludesAndAttributes(t *testing.T) {
	home := t.TempDir()
	gitConfig := filepath.Join(home, ".config", "git")
	if err := os.MkdirAll(gitConfig, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(gitConfig, "ignore"), "notes.local\n")
	writeFile(t, filepath.Join(gitConfig, "attributes"), "notes.local filter=probe -diff\n")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	fixture := newFixture(t)
	ctx := context.Background()
	attributes, err := runGit(ctx, fixture.checkout.path, "check-attr", "filter", "diff", "--", "notes.local")
	if err != nil || attributes != "notes.local: filter: unset\nnotes.local: diff: unspecified" {
		t.Fatalf("global attributes replaced controlled attributes: %q error=%v", attributes, err)
	}
	const branch = "minion/fix-global-excludes"
	if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	data := []byte("exact source\r\n")
	if err := fixture.checkout.Import(ctx, fixture.base, []workspace.Change{{Path: "notes.local", Mode: workspace.Regular, Data: data}}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.checkout.CommitAndPush(ctx, branch, "bot", "include new source", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "--git-dir="+fixture.remote, "show", "refs/heads/"+branch+":notes.local")
	actual, err := cmd.Output()
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("published bytes=%q error=%v", actual, err)
	}
}

func TestControlledGitRejectsPortableMetadataVariants(t *testing.T) {
	for _, name := range []string{".g\u200cit/hooks/post-checkout", "git~1/hooks/post-checkout", ".git:alternate/hooks/post-checkout"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			ctx := context.Background()
			if _, err := runGit(ctx, root, "init"); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"core.protectHFS", "core.protectNTFS"} {
				if _, err := runGit(ctx, root, "config", key, "false"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(root, name), "unsafe metadata alias")
			if _, err := runGit(ctx, root, "add", "--all", "--", "."); err == nil || !strings.Contains(err.Error(), "invalid path") {
				t.Fatalf("metadata alias %q staging error=%v", name, err)
			}
			output, err := runGit(ctx, root, "diff", "--cached", "--name-only")
			if err != nil || output != "" {
				t.Fatalf("rejected metadata was staged: %q, %v", output, err)
			}
		})
	}
}

func TestSnapshotHistoryIgnoresUntrustedMailmap(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.checkout.path, ".mailmap"), "Forged <forged@example.invalid> Test <test@example.invalid>\n")
	fixture.git(fixture.checkout.path, "add", ".mailmap")
	fixture.git(fixture.checkout.path, "commit", "-m", "untrusted author mapping")
	fixture.git(fixture.checkout.path, "config", "log.mailmap", "true")
	fixture.git(fixture.checkout.path, "notes", "add", "-m", "PRIVATE_NOTE_NOT_EXPORTED")
	fixture.checkout.expectedHead = fixture.git(fixture.checkout.path, "rev-parse", "HEAD")
	if err := fixture.checkout.recordConfig(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.checkout.Snapshot(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(snapshot.History, []byte("PRIVATE_NOTE_NOT_EXPORTED")) || bytes.Contains(snapshot.History, []byte("Forged")) || !bytes.Contains(snapshot.History, []byte("Test <test@example.invalid>")) {
		t.Fatalf("mailmap rewrote history authors: %s", snapshot.History)
	}
}

func TestSnapshotPreservesGitLegalSourceNames(t *testing.T) {
	fixture := newFixture(t)
	names := []string{"include/xt_connmark.h", "include/xt_CONNMARK.h", "Docs/a.md", "docs/b.md", `back\slash`, ".g\u200cit\\x", "dir\\.g\u200cit/x"}
	for _, name := range names {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(fixture.checkout.path, name)), 0700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(fixture.checkout.path, name), name)
	}
	fixture.git(fixture.checkout.path, "add", ".")
	fixture.git(fixture.checkout.path, "commit", "-m", "Git-legal source names")
	fixture.checkout.expectedHead = fixture.git(fixture.checkout.path, "rev-parse", "HEAD")
	snapshot, err := fixture.checkout.Snapshot(context.Background(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Materialize(root, snapshot.Files); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(data) != name {
			t.Fatalf("source %q data=%q, error=%v", name, data, err)
		}
	}
	if err := workspace.CheckUnchanged(context.Background(), root, snapshot.Files); err != nil {
		t.Fatalf("unchanged source: %v", err)
	}
}

func TestSourceEntriesCrossChecksGitMetadata(t *testing.T) {
	fixture := newFixture(t)
	root := fixture.checkout.path
	if err := os.Symlink("README.md", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	fixture.git(root, "add", "link")
	fixture.git(root, "commit", "-m", "symlink metadata fixture")
	head := fixture.git(root, "rev-parse", "HEAD")
	treeSHA := fixture.git(root, "rev-parse", "HEAD^{tree}")
	tree, err := localTreeMetadata(t, root)(context.Background(), treeSHA)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*github.GitTree)
	}{
		{"valid", func(*github.GitTree) {}},
		{"regular mode", func(tree *github.GitTree) { tree.Tree[0].Mode = "100755" }},
		{"symlink as regular", func(tree *github.GitTree) { tree.Tree[1].Mode = "100644" }},
		{"missing", func(tree *github.GitTree) { tree.Tree = tree.Tree[:1] }},
		{"unmatched", func(tree *github.GitTree) {
			size := int64(0)
			tree.Tree = append(tree.Tree, github.GitTreeEntry{Path: "extra", Mode: "100644", Type: "blob", Size: &size})
		}},
		{"duplicate", func(tree *github.GitTree) { tree.Tree = append(tree.Tree, tree.Tree[0]) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := tree
			changed.Tree = append([]github.GitTreeEntry(nil), tree.Tree...)
			test.mutate(&changed)
			_, err := fixture.checkout.sourceEntries(context.Background(), head, changed, treeSHA)
			if test.name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUnsupportedSource) {
				t.Fatalf("metadata mismatch classification: %v", err)
			}
		})
	}
}

func TestSourceTreeAcceptsExactTransferLimits(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, test := range []struct {
		name    string
		entries []github.GitTreeEntry
	}{
		{"files", func() []github.GitTreeEntry {
			entries := make([]github.GitTreeEntry, workspace.MaxFiles)
			for i := range entries {
				entries[i] = github.GitTreeEntry{Path: fmt.Sprintf("f%d", i), Type: "commit", Mode: "160000"}
			}
			return entries
		}()},
		{"entries", func() []github.GitTreeEntry {
			entries := make([]github.GitTreeEntry, workspace.MaxEntries)
			for i := range entries {
				entries[i] = github.GitTreeEntry{Path: fmt.Sprintf("d%d", i), Type: "tree", Mode: "040000"}
			}
			return entries
		}()},
		{"bytes", func() []github.GitTreeEntry {
			size := int64(workspace.MaxBytes)
			return []github.GitTreeEntry{{Path: "full", Type: "blob", Mode: "100644", Size: &size}}
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := checkSourceTree(completeTree(sha, test.entries), sha); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSourceTreeListingOverflowIsMetadataMismatch(t *testing.T) {
	fixture := newFixture(t)
	treeSHA := fixture.git(fixture.checkout.path, "rev-parse", "HEAD^{tree}")
	tree, err := localTreeMetadata(t, fixture.checkout.path)(context.Background(), treeSHA)
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *' ls-tree '*) head -c %d /dev/zero;; *) exec %s \"$@\";; esac\n", maxPathListBytes+1, shellQuote(realGit))
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = fixture.checkout.sourceEntries(context.Background(), fixture.base, tree, treeSHA)
	if !errors.Is(err, ErrUnsupportedSource) || errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("listing mismatch error=%v", err)
	}
}

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

func recordSnapshotContentCalls(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	log := filepath.Join(root, "content-calls")
	shim := "#!/bin/sh\nfor arg do\ncase \"$arg\" in --batch) printf x >> " + shellQuote(log) + ";; --no-color) printf d >> " + shellQuote(log) + ";; esac\ndone\nexec " + shellQuote(git) + " \"$@\"\n"
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
	if err := fixture.checkout.checkoutPullRequest(context.Background(), 7, "main", head); err != nil {
		t.Fatal(err)
	}
	log := recordSnapshotContentCalls(t)
	snapshot, err := fixture.checkout.Snapshot(context.Background(), false, true)
	if err != nil || len(snapshot.Files) != 1 || len(snapshot.BaseFiles) != 1 || string(snapshot.Files[0].Data) != "\x00head\n\xff" || string(snapshot.BaseFiles[0].Data) != "fixture\n" {
		t.Fatalf("batch snapshot=%+v error=%v", snapshot, err)
	}
	calls, err := os.ReadFile(log)
	if err != nil || string(calls) != "dx" {
		t.Fatalf("ordered content invocations=%q error=%v", calls, err)
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
	log := recordSnapshotContentCalls(t)
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
		t.Fatalf("preflight requested content: %q error=%v", data, err)
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
