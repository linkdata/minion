package gitwork

import (
	"bytes"
	"context"
	"encoding/hex"
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

func TestBoundedGitPreservesCancellation(t *testing.T) {
	fixture := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := fixture.checkout.boundedGit(ctx, nil, 1, false, "cat-file", "blob", fixture.base+":README.md"); !errors.Is(err, context.Canceled) || errors.Is(err, ErrCheckoutTooLarge) || strings.Count(err.Error(), context.Canceled.Error()) != 1 {
		t.Fatalf("canceled read=%v", err)
	}
}

func TestSnapshotUsesPinnedBlobs(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.checkout.path, "README.md"), "untrusted working tree")
	writeFile(t, filepath.Join(fixture.checkout.path, "untracked"), "not source")
	snapshot, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != 1 || string(snapshot.Files[0].Data) != "fixture\n" || snapshot.Head != fixture.base {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestSnapshotPreservesPRDeletedBinaryAndModeContext(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, ".gitattributes"), "* -diff\n")
	writeFile(t, filepath.Join(fixture.source, "text.txt"), "base text\n")
	writeFile(t, filepath.Join(fixture.source, "deleted.txt"), "deleted base content\n")
	writeFile(t, filepath.Join(fixture.source, "binary"), "\x00\xffbase")
	base := fixture.commit("refs/heads/main", "base files")
	if err := os.Remove(filepath.Join(fixture.source, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.source, "binary"), "\x00\xffhead")
	writeFile(t, filepath.Join(fixture.source, "text.txt"), "head text\n")
	if err := os.Chmod(filepath.Join(fixture.source, "binary"), 0755); err != nil {
		t.Fatal(err)
	}
	head := fixture.commit("refs/pull/7/head", "PR files")
	fixture.git(fixture.source, "checkout", "--detach", base)
	writeFile(t, filepath.Join(fixture.source, "base-only"), "new base branch content")
	baseTip := fixture.commit("refs/heads/main", "base advanced after fork")
	fixture.checkout = fixture.clonePullRequest(head)
	snapshot, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Head != head || snapshot.Base != baseTip || snapshot.MergeBase != base {
		t.Fatalf("snapshot commits=%+v", snapshot)
	}
	if !bytes.Contains(snapshot.Diff, []byte("Binary files")) || !bytes.Contains(snapshot.Diff, []byte("deleted.txt")) || !bytes.Contains(snapshot.Diff, []byte("100755")) {
		t.Fatalf("comparison diff=%s", snapshot.Diff)
	}
	if !bytes.Contains(snapshot.Diff, []byte("-base text\n+head text\n")) {
		t.Fatalf("repository diff attributes hid text changes: %s", snapshot.Diff)
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

func TestSnapshotOverridesNamedBinaryDiffDriver(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, ".gitattributes"), "text.txt diff=blocked\n")
	writeFile(t, filepath.Join(fixture.source, "text.txt"), "base text\n")
	base := fixture.commit("refs/heads/main", "named diff driver base")
	writeFile(t, filepath.Join(fixture.source, "text.txt"), "head text\n")
	head := fixture.commit("refs/pull/7/head", "named diff driver head")
	control := fixture.git(fixture.source, "-c", "diff.blocked.binary=true", "diff", "--no-ext-diff", "--no-textconv", base, head, "--", "text.txt")
	if !strings.Contains(control, "Binary files") || strings.Contains(control, "-base text") {
		t.Fatalf("named-driver control did not hide text: %s", control)
	}
	fixture.checkout = fixture.clonePullRequest(head)
	fixture.git(fixture.checkout.path, "config", "diff.blocked.binary", "true")
	if err := fixture.checkout.recordConfig(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.checkout.Snapshot(context.Background())
	if err != nil || !bytes.Contains(snapshot.Diff, []byte("-base text\n+head text\n")) || bytes.Contains(snapshot.Diff, []byte("Binary files")) {
		t.Fatalf("named diff driver hid snapshot text: %s error=%v", snapshot.Diff, err)
	}
}

func TestSnapshotDisablesComparisonRenameDetection(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "old.txt"), "unchanged content to recognize as a rename\n")
	fixture.commit("refs/heads/main", "rename base")
	if err := os.Rename(filepath.Join(fixture.source, "old.txt"), filepath.Join(fixture.source, "new.txt")); err != nil {
		t.Fatal(err)
	}
	head := fixture.commit("refs/pull/7/head", "rename file")
	fixture.checkout = fixture.clonePullRequest(head)
	fixture.git(fixture.checkout.path, "config", "diff.renames", "true")
	if err := fixture.checkout.recordConfig(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.checkout.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(snapshot.Diff, []byte("deleted file mode 100644")) || !bytes.Contains(snapshot.Diff, []byte("new file mode 100644")) || bytes.Contains(snapshot.Diff, []byte("rename from")) {
		t.Fatalf("comparison inferred a rename: %s", snapshot.Diff)
	}
}

func TestCloneRejectsShallowSource(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.remote, "shallow"), fixture.base+"\n")
	root := t.TempDir()
	checkout, err := cloneFromURL(context.Background(), root, "file://"+fixture.remote, "main", localTreeMetadata(fixture.source), true)
	if checkout != nil || err == nil || !strings.Contains(err.Error(), "source repository is shallow") {
		t.Fatalf("shallow source returned checkout=%v error=%v", checkout, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("shallow refusal retained checkout: entries=%v error=%v", entries, err)
	}
}

func TestSourceFilesBoundsDistinctOmittedUnion(t *testing.T) {
	entries := make([]sourceEntry, workspace.MaxFiles+1)
	for i := range entries {
		entries[i] = sourceEntry{file: workspace.File{Path: fmt.Sprintf("link-%d", i)}, omitted: true}
	}
	checkout := &Checkout{}
	if _, _, err := checkout.sourceFiles(context.Background(), entries); !errors.Is(err, ErrCheckoutTooLarge) {
		t.Fatalf("accepted oversized omitted union: %v", err)
	}
	entries[len(entries)-1] = entries[0]
	files, omitted, err := checkout.sourceFiles(context.Background(), entries)
	if err != nil || len(files) != 0 || len(omitted) != workspace.MaxFiles {
		t.Fatalf("exact distinct omitted union: files=%d omitted=%d error=%v", len(files), len(omitted), err)
	}
}

func TestSnapshotRejectsGitConfigChangedDuringPreparation(t *testing.T) {
	fixture := newFixture(t)
	lookup := fixture.checkout.lookupTree
	changed := false
	fixture.checkout.lookupTree = func(ctx context.Context, sha string) (github.GitTree, error) {
		tree, err := lookup(ctx, sha)
		fixture.git(fixture.checkout.path, "config", "minion.probe", "changed")
		changed = true
		return tree, err
	}
	_, err := fixture.checkout.Snapshot(context.Background())
	if !changed || err == nil || !strings.Contains(err.Error(), "Git config changed") {
		t.Fatalf("mid-preparation config change: changed=%v error=%v", changed, err)
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
	checkout := fixture.clone()
	data, err := os.ReadFile(filepath.Join(checkout.path, "payload"))
	if err != nil || string(data) != payload {
		t.Fatalf("transformed default source: %q,%v", data, err)
	}
}

func TestImportRejectsIdentityAndMetadataAndPreservesBytes(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	if err := fixture.checkout.Import(ctx, strings.Repeat("f", 40), []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("wrong source\n")}}); err == nil || !strings.Contains(err.Error(), "result source commit does not match checkout") {
		t.Fatalf("wrong-source import error=%v", err)
	}
	if data, err := os.ReadFile(filepath.Join(fixture.checkout.path, "README.md")); err != nil || string(data) != "fixture\n" {
		t.Fatalf("wrong-source import changed source: %q error=%v", data, err)
	}
	if count, err := fixture.checkout.stagedPathCount(ctx); err != nil || count != 0 {
		t.Fatalf("wrong-source import staged count=%d error=%v", count, err)
	}
	marker := filepath.Join(t.TempDir(), "executed")
	fixture.git(fixture.checkout.path, "config", "filter.returned.clean", "printf filter >> "+shellQuote(marker)+"; cat")
	fixture.git(fixture.checkout.path, "config", "core.hooksPath", filepath.Join(fixture.checkout.path, "hooks"))
	if err := fixture.checkout.recordConfig(); err != nil {
		t.Fatal(err)
	}
	const branch = "minion/fix-returned-control-files"
	if err := fixture.checkout.NewBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	changes := []workspace.Change{
		{Path: "README.md", Delete: true},
		{Path: "binary", Mode: workspace.Executable, Data: []byte{0, 255, 1}},
		{Path: ".gitattributes", Mode: workspace.Regular, Data: []byte("binary filter=returned\n")},
		{Path: "hooks/post-commit", Mode: workspace.Executable, Data: []byte("#!/bin/sh\nprintf hook >> " + shellQuote(marker) + "\n")},
		{Path: "hooks/pre-commit", Mode: workspace.Executable, Data: []byte("#!/bin/sh\nprintf pre-commit >> " + shellQuote(marker) + "\nexit 1\n")},
		{Path: "hooks/pre-push", Mode: workspace.Executable, Data: []byte("#!/bin/sh\nprintf pre-push >> " + shellQuote(marker) + "\nexit 1\n")},
	}
	if err := fixture.checkout.Import(ctx, fixture.base, changes); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(fixture.checkout.path, "binary"))
	if err != nil || !bytes.Equal(data, changes[1].Data) {
		t.Fatalf("binary=%v,%v", data, err)
	}
	staged := exec.Command("git", "-C", fixture.checkout.path, "show", ":binary")
	staged.Env = cleanGitEnvironment()
	data, err = staged.Output()
	if err != nil || !bytes.Equal(data, changes[1].Data) {
		t.Fatalf("staged binary=%v,%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.checkout.path, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("deleted source=%v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("returned control files executed during import: %v", err)
	}
	if err := fixture.checkout.commitAndPush(ctx, fixture.remote, branch, "bot", "reviewed data", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("returned control files executed during publication: %v", err)
	}
}

func TestSnapshotReusesValidatedTreesByCommit(t *testing.T) {
	fixture := newFixture(t)
	lookups := 0
	lookup := localTreeMetadata(fixture.source)
	checkout := fixture.keepCheckout(cloneFromURL(context.Background(), t.TempDir(), fixture.remote, "main", func(ctx context.Context, sha string) (github.GitTree, error) {
		lookups++
		return lookup(ctx, sha)
	}, true))
	for i := 0; i < 2; i++ {
		if _, err := checkout.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if lookups != 1 {
		t.Fatalf("default clone/snapshot metadata lookups=%d, want 1", lookups)
	}
	writeFile(t, filepath.Join(fixture.source, "README.md"), "PR source\n")
	head := fixture.commit("refs/pull/7/head", "PR source")
	if err := checkout.checkoutPullRequest(context.Background(), 7, "main", head); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		snapshot, err := checkout.Snapshot(context.Background())
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
	fixture.commit("refs/heads/main", "base symlink")
	checkout := fixture.clone()
	if err := os.Rename(filepath.Join(fixture.source, "Docs"), filepath.Join(fixture.source, "docs")); err != nil {
		t.Fatal(err)
	}
	head := fixture.commit("refs/pull/7/head", "rename symlink")
	if err := checkout.checkoutPullRequest(context.Background(), 7, "main", head); err != nil {
		t.Fatal(err)
	}
	snapshot, err := checkout.Snapshot(context.Background())
	if err != nil || len(snapshot.Omitted) != 2 || snapshot.Omitted[0] != "docs" || snapshot.Omitted[1] != "Docs" {
		t.Fatalf("omitted=%v error=%v", snapshot.Omitted, err)
	}
}

func TestPublishIgnoresDefaultGlobalExcludesAndAttributes(t *testing.T) {
	home := t.TempDir()
	gitConfig := filepath.Join(home, ".config", "git")
	writeFile(t, filepath.Join(gitConfig, "ignore"), "notes.local\n")
	writeFile(t, filepath.Join(gitConfig, "attributes"), "notes.local filter=probe -diff probe=global\n")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	fixture := newFixture(t)
	ctx := context.Background()
	attributes, err := runGit(ctx, fixture.checkout.path, "check-attr", "filter", "diff", "probe", "--", "notes.local")
	if err != nil || attributes != "notes.local: filter: unset\nnotes.local: diff: unspecified\nnotes.local: probe: unspecified" {
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
	if err := fixture.checkout.commitAndPush(ctx, fixture.remote, branch, "bot", "include new source", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "--git-dir="+fixture.remote, "show", "refs/heads/"+branch+":notes.local")
	cmd.Env = cleanGitEnvironment()
	actual, err := cmd.Output()
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("published bytes=%q error=%v", actual, err)
	}
}

func TestCloneIgnoresHostTemplateExclusions(t *testing.T) {
	fixture := newFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	template := t.TempDir()
	writeFile(t, filepath.Join(template, "info", "exclude"), "notes.local\n")
	control := filepath.Join(t.TempDir(), "control")
	if output, err := exec.Command(realGit, "-c", "init.templateDir="+template, "clone", "--no-checkout", "--branch", "main", fixture.remote, control).CombinedOutput(); err != nil {
		t.Fatalf("clone host-template control: %s,%v", output, err)
	}
	if data, err := os.ReadFile(filepath.Join(control, ".git", "info", "exclude")); err != nil || string(data) != "notes.local\n" {
		t.Fatalf("control did not inherit host template: %q,%v", data, err)
	}
	shim := "#!/bin/sh\nexec " + shellQuote(realGit) + " -c " + shellQuote("init.templateDir="+template) + " \"$@\"\n"
	fakeOnPath(t, "git", shim)
	checkout := fixture.clone()
	if _, err := os.Lstat(filepath.Join(checkout.path, ".git", "info", "exclude")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trusted clone inherited a template exclusion: %v", err)
	}
	if err := checkout.Import(context.Background(), fixture.base, []workspace.Change{{Path: "notes.local", Mode: workspace.Regular, Data: []byte("reviewed file")}}); err != nil {
		t.Fatalf("host template excluded reviewed fix: %v", err)
	}
}

func TestControlledGitRejectsPortableMetadataVariants(t *testing.T) {
	for _, name := range []string{".g\u200cit/hooks/post-checkout", "git~1/hooks/post-checkout"} {
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

func TestSnapshotPreservesGitLegalSourceNames(t *testing.T) {
	fixture := newFixture(t)
	names := []string{"include/xt_connmark.h", "include/xt_CONNMARK.h", "Docs/a.md", "docs/b.md", `back\slash`, ".g\u200cit\\x", "dir\\.g\u200cit/x"}
	for _, name := range names {
		writeFile(t, filepath.Join(fixture.source, name), name)
	}
	fixture.commit("refs/heads/main", "Git-legal source names")
	checkout := fixture.clone()
	snapshot, err := checkout.Snapshot(context.Background())
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
	tree, err := localTreeMetadata(root)(context.Background(), treeSHA)
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

func TestSourceEntriesRejectsAncestorCollisionWithoutDirectoryMetadata(t *testing.T) {
	fixture := newFixture(t)
	blob := fixture.git(fixture.source, "rev-parse", "HEAD:README.md")
	subtree := fixture.gitInput(fixture.source, "100644 blob "+blob+"\tb\n", "mktree")
	blobBytes, err := hex.DecodeString(blob)
	if err != nil {
		t.Fatal(err)
	}
	treeBytes, err := hex.DecodeString(subtree)
	if err != nil {
		t.Fatal(err)
	}
	// A malformed tree can name both a file and a directory "a". The recursive
	// metadata fixture, like a partial API response, lists only a and a/b.
	raw := "100644 a\x00" + string(blobBytes) + "40000 a\x00" + string(treeBytes)
	treeSHA := fixture.gitInput(fixture.source, raw, "hash-object", "--literally", "-w", "-t", "tree", "--stdin")
	tree, err := localTreeMetadata(fixture.source)(context.Background(), treeSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSourceTree(tree, treeSHA); err != nil {
		t.Fatalf("fixture failed before ancestor-layout validation: %v", err)
	}
	checkout := &Checkout{path: fixture.source}
	_, err = checkout.sourceEntries(context.Background(), treeSHA, tree, treeSHA)
	if !errors.Is(err, ErrUnsupportedSource) || !strings.Contains(err.Error(), "file/directory collision") {
		t.Fatalf("accepted incomplete directory metadata: %v", err)
	}
}

func TestSourceTreeListingOverflowIsMetadataMismatch(t *testing.T) {
	fixture := newFixture(t)
	treeSHA := fixture.git(fixture.checkout.path, "rev-parse", "HEAD^{tree}")
	tree, err := localTreeMetadata(fixture.checkout.path)(context.Background(), treeSHA)
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *' ls-tree '*) head -c %d /dev/zero;; *) exec %s \"$@\";; esac\n", maxPathListBytes+1, shellQuote(realGit))
	fakeOnPath(t, "git", script)
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
	fakeOnPath(t, "git", shim)
	return log
}

func TestSnapshotReadsHeadAndBaseInOneBatch(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.source, "README.md"), "\x00head\n\xff")
	fixture.git(fixture.source, "add", "README.md")
	fixture.git(fixture.source, "commit", "-m", "binary PR")
	head := fixture.git(fixture.source, "rev-parse", "HEAD")
	fixture.git(fixture.source, "push", fixture.remote, "HEAD:refs/pull/7/head")
	checkout := fixture.clonePullRequest(head)
	log := recordSnapshotContentCalls(t)
	snapshot, err := checkout.Snapshot(context.Background())
	if err != nil || len(snapshot.Files) != 1 || len(snapshot.BaseFiles) != 1 || string(snapshot.Files[0].Data) != "\x00head\n\xff" || string(snapshot.BaseFiles[0].Data) != "fixture\n" {
		t.Fatalf("batch snapshot=%+v error=%v", snapshot, err)
	}
	calls, err := os.ReadFile(log)
	if err != nil || string(calls) != "xd" {
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

func TestSnapshotPreflightsChangedPaths(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(fixture.checkout.path, "README.md"), "changed\n")
	fixture.git(fixture.checkout.path, "add", "README.md")
	fixture.git(fixture.checkout.path, "commit", "-m", "PR")
	head := fixture.git(fixture.checkout.path, "rev-parse", "HEAD")
	fixture.checkout.expectedHead = head
	fixture.checkout.comparisonBase = fixture.base
	blob := fixture.git(fixture.checkout.path, "rev-parse", "HEAD:README.md")
	log := recordSnapshotContentCalls(t)
	t.Run("changed path budget", func(t *testing.T) {
		tree := func(prefix string, count int) string {
			var input strings.Builder
			for i := 0; i < count; i++ {
				fmt.Fprintf(&input, "100644 blob %s\t%s-%05d\n", blob, prefix, i)
			}
			return fixture.gitInput(fixture.checkout.path, input.String(), "mktree")
		}
		for _, count := range []int{workspace.MaxFiles, workspace.MaxFiles + 1} {
			t.Run(fmt.Sprint(count), func(t *testing.T) {
				base := fixture.gitInput(fixture.checkout.path, "base\n", "commit-tree", tree("old", workspace.MaxFiles/2), "-p", fixture.base)
				head := fixture.gitInput(fixture.checkout.path, "head\n", "commit-tree", tree("new", count-workspace.MaxFiles/2), "-p", base)
				fixture.git(fixture.checkout.path, "update-ref", "HEAD", head)
				fixture.checkout.comparisonBase = base
				fixture.checkout.expectedHead = head
				fixture.checkout.sourceTrees = nil
				snapshot, err := fixture.checkout.Snapshot(ctx)
				if count == workspace.MaxFiles {
					if err != nil || len(snapshot.ChangedPaths) != count {
						t.Fatalf("exact changed-path budget: paths=%d error=%v", len(snapshot.ChangedPaths), err)
					}
					if err := os.Remove(log); err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, ErrCheckoutTooLarge) || errors.Is(err, ErrUnsupportedSource) {
					t.Fatalf("changed path budget error=%v", err)
				}
			})
		}
	})
	if data, err := os.ReadFile(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight requested content: %q error=%v", data, err)
	}
}
