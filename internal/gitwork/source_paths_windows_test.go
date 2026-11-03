package gitwork

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/workspace"
)

// windowsRawTree writes Git object names without asking Windows to create them.
func windowsRawTree(f gitFixture, records ...string) string {
	return f.gitInput(f.source, strings.Join(records, "\x00")+"\x00", "mktree", "-z")
}

func windowsTreeCommit(f gitFixture, tree string) string {
	return f.gitInput(f.source, "native path fixture\n", "commit-tree", tree, "-p", f.base)
}

func windowsRecordGitCommands(t *testing.T) string {
	t.Helper()
	realGit, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	proxyDir := t.TempDir()
	testsupport.LinkTestExecutable(t, filepath.Join(proxyDir, "git.exe"))
	log := filepath.Join(t.TempDir(), "git-commands")
	t.Setenv("PATH", proxyDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MINION_TEST_GIT_PROXY", realGit)
	t.Setenv("MINION_TEST_GIT_COMMANDS", log)
	return log
}

func windowsAssertNoCheckout(t *testing.T, log string) {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil || !bytes.Contains(data, []byte("clone\n")) || !bytes.Contains(data, []byte("ls-tree\n")) || bytes.Contains(data, []byte("checkout\n")) || bytes.Contains(data, []byte("update-ref\n")) {
		t.Fatalf("source preflight Git calls=%q error=%v", data, err)
	}
}

func TestWindowsHostileGitTreePathsFailBeforeCheckout(t *testing.T) {
	for _, test := range []struct {
		name               string
		paths              []string
		directoryCollision bool
		linkCollision      bool
		reason             string
	}{
		{name: "device", paths: []string{"CON"}},
		{name: "numbered device", paths: []string{"COM1"}},
		{name: "console device", paths: []string{"CONIN$"}},
		{name: "trailing dot", paths: []string{"report."}},
		{name: "trailing space", paths: []string{"report "}},
		{name: "alternate data stream", paths: []string{"report:secret"}},
		{name: "Git metadata alias", paths: []string{"git~1"}},
		{name: "drive component", paths: []string{"C:"}},
		{name: "UNC spelling", paths: []string{`\\server`}},
		{name: "file case collision", paths: []string{"Readme", "README"}, reason: "duplicate source path"},
		{name: "directory case collision", paths: []string{"Docs/file", "docs/file"}, directoryCollision: true, reason: "source directory spelling collision"},
		{name: "omitted link case collision", paths: []string{"Link", "link"}, linkCollision: true, reason: "duplicate source path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			blob := fixture.gitInput(fixture.source, "hostile\n", "hash-object", "-w", "--stdin")
			var records []string
			if test.directoryCollision {
				child := windowsRawTree(fixture, fmt.Sprintf("100644 blob %s\tfile", blob))
				records = []string{
					fmt.Sprintf("040000 tree %s\tDocs", child),
					fmt.Sprintf("040000 tree %s\tdocs", child),
				}
			} else if test.linkCollision {
				records = []string{
					fmt.Sprintf("100644 blob %s\tLink", blob),
					fmt.Sprintf("120000 blob %s\tlink", blob),
				}
			} else {
				for _, name := range test.paths {
					records = append(records, fmt.Sprintf("100644 blob %s\t%s", blob, name))
				}
			}
			tree := windowsRawTree(fixture, records...)
			head := windowsTreeCommit(fixture, tree)
			fixture.git(fixture.source, "push", fixture.remote, head+":refs/heads/main")
			listing := fixture.git(fixture.source, "ls-tree", "-r", "--name-only", "-z", head)
			for _, name := range test.paths {
				if !strings.Contains(listing, name+"\x00") {
					t.Fatalf("Git tree lost exact path %q: %q", name, listing)
				}
			}
			log := windowsRecordGitCommands(t)
			workRoot := t.TempDir()
			checkout, err := cloneFromURL(t.Context(), workRoot, fixture.remote, "main", true)
			if checkout != nil {
				t.Cleanup(func() { _ = checkout.Close() })
			}
			var exit *exec.ExitError
			reason := test.reason
			if reason == "" {
				reason = "unsafe source path"
			}
			if checkout != nil || !errors.Is(err, ErrUnsupportedSource) || errors.As(err, &exit) || !strings.Contains(err.Error(), reason) {
				t.Fatalf("hostile Git tree %q crossed source preflight: checkout=%v error=%v", test.paths, checkout, err)
			}
			windowsAssertNoCheckout(t, log)
			if entries, err := os.ReadDir(workRoot); err != nil || len(entries) != 0 {
				t.Fatalf("rejected tree materialized checkout: entries=%v error=%v", entries, err)
			}
		})
	}
}

func TestWindowsHostilePullRequestTreeFailsBeforeCheckout(t *testing.T) {
	fixture := newFixture(t)
	blob := fixture.gitInput(fixture.source, "unsafe\n", "hash-object", "-w", "--stdin")
	tree := windowsRawTree(fixture, fmt.Sprintf("100644 blob %s\tgit~1", blob))
	head := windowsTreeCommit(fixture, tree)
	fixture.git(fixture.source, "push", fixture.remote, head+":refs/pull/7/head")
	log := windowsRecordGitCommands(t)
	workRoot := t.TempDir()
	checkout, err := clonePullRequestFromURL(t.Context(), workRoot, fixture.remote, 7, "main", head)
	if checkout != nil {
		t.Cleanup(func() { _ = checkout.Close() })
	}
	var exit *exec.ExitError
	if checkout != nil || !errors.Is(err, ErrUnsupportedSource) || errors.As(err, &exit) || !strings.Contains(err.Error(), "unsafe source path") {
		t.Fatalf("hostile PR tree crossed source preflight: checkout=%v error=%v", checkout, err)
	}
	windowsAssertNoCheckout(t, log)
	if entries, err := os.ReadDir(workRoot); err != nil || len(entries) != 0 {
		t.Fatalf("rejected PR tree materialized checkout: entries=%v error=%v", entries, err)
	}
}

func TestWindowsPullRequestUsesSafeHeadAndSelectedBaseFiles(t *testing.T) {
	fixture := newFixture(t)
	blob := fixture.gitInput(fixture.source, "safe\n", "hash-object", "-w", "--stdin")
	baseTree := windowsRawTree(fixture,
		fmt.Sprintf("100644 blob %s\tReadme", blob),
		fmt.Sprintf("100644 blob %s\tREADME", blob))
	base := windowsTreeCommit(fixture, baseTree)
	headTree := windowsRawTree(fixture, fmt.Sprintf("100644 blob %s\tReadme", blob))
	head := fixture.gitInput(fixture.source, "safe head\n", "commit-tree", headTree, "-p", base)
	fixture.git(fixture.source, "push", fixture.remote, base+":refs/heads/main")
	fixture.git(fixture.source, "push", fixture.remote, head+":refs/pull/7/head")
	checkout, err := clonePullRequestFromURL(t.Context(), t.TempDir(), fixture.remote, 7, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	snapshot, err := checkout.Snapshot(t.Context())
	if err != nil || snapshot.Head != head || snapshot.MergeBase != base || len(snapshot.Files) != 1 || snapshot.Files[0].Path != "Readme" || len(snapshot.BaseFiles) != 1 || snapshot.BaseFiles[0].Path != "README" {
		t.Fatalf("safe PR comparison snapshot=%+v error=%v", snapshot, err)
	}
	if err := workspace.ValidateSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	for _, files := range [][]workspace.File{snapshot.Files, snapshot.BaseFiles} {
		if err := workspace.Materialize(t.TempDir(), files); err != nil {
			t.Fatalf("safe comparison files could not materialize: %v", err)
		}
	}
}

func TestWindowsImportRejectsHostileResultPathsBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		paths  []string
		reason string
	}{
		{name: "device", paths: []string{"NUL"}, reason: "unsafe or duplicate"},
		{name: "numbered device", paths: []string{"LPT1"}, reason: "unsafe or duplicate"},
		{name: "trailing dot", paths: []string{"output."}, reason: "unsafe or duplicate"},
		{name: "trailing space", paths: []string{"output "}, reason: "unsafe or duplicate"},
		{name: "alternate data stream", paths: []string{"output:secret"}, reason: "unsafe or duplicate"},
		{name: "metadata alias", paths: []string{"git~1/config"}, reason: "unsafe or duplicate"},
		{name: "drive relative", paths: []string{"C:relative"}, reason: "unsafe or duplicate"},
		{name: "drive absolute", paths: []string{"C:/outside"}, reason: "unsafe or duplicate"},
		{name: "UNC backslashes", paths: []string{`\\server\share\outside`}, reason: "unsafe or duplicate"},
		{name: "UNC slashes", paths: []string{"//server/share/outside"}, reason: "unsafe or duplicate"},
		{name: "file case collision", paths: []string{"Output", "output"}, reason: "unsafe or duplicate"},
		{name: "directory case collision", paths: []string{"Docs/a", "docs/b"}, reason: "source directory spelling collision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			changes := []workspace.Change{{Path: "README.md", Mode: workspace.Regular, Data: []byte("changed before rejection\n")}}
			for _, name := range test.paths {
				changes = append(changes, workspace.Change{Path: name, Mode: workspace.Regular, Data: []byte("unreviewed")})
			}
			if err := fixture.checkout.Import(t.Context(), fixture.base, changes); err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("hostile import %q was not rejected before mutation: %v", test.paths, err)
			}
			if data, err := os.ReadFile(filepath.Join(fixture.checkout.path, "README.md")); err != nil || string(data) != "fixture\n" {
				t.Fatalf("rejected import changed source: %q, %v", data, err)
			}
			if entries, err := os.ReadDir(fixture.checkout.path); err != nil || len(entries) != 2 || entries[0].Name() != ".git" || entries[1].Name() != "README.md" {
				t.Fatalf("rejected import changed checkout entries: %v, %v", entries, err)
			}
			if count, err := fixture.checkout.stagedPathCount(t.Context()); err != nil || count != 0 {
				t.Fatalf("rejected import staged paths=%d, %v", count, err)
			}
		})
	}
}

func TestWindowsGitTreeLinkCannotBecomeImportParent(t *testing.T) {
	fixture := newFixture(t)
	linkBlob := fixture.gitInput(fixture.source, "../outside", "hash-object", "-w", "--stdin")
	tree := windowsRawTree(fixture, fmt.Sprintf("120000 blob %s\tlinked", linkBlob))
	head := windowsTreeCommit(fixture, tree)
	fixture.git(fixture.source, "push", fixture.remote, head+":refs/heads/main")
	workRoot := t.TempDir()
	outside := filepath.Join(workRoot, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	checkout, err := cloneFromURL(t.Context(), workRoot, fixture.remote, "main", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	snapshot, err := checkout.Snapshot(t.Context())
	if err != nil || len(snapshot.Files) != 0 || len(snapshot.Omitted) != 1 || snapshot.Omitted[0] != "linked" {
		t.Fatalf("Git symlink source snapshot files=%v omitted=%v error=%v", snapshot.Files, snapshot.Omitted, err)
	}
	if err := checkout.Import(t.Context(), head, []workspace.Change{{Path: "linked/keep", Mode: workspace.Regular, Data: []byte("changed")}}); err == nil {
		t.Fatal("Git link became an import parent")
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "outside" {
		t.Fatalf("outside target changed: %q, %v", data, err)
	}
	if count, err := checkout.stagedPathCount(t.Context()); err != nil || count != 0 {
		t.Fatalf("refused link import staged paths=%d, %v", count, err)
	}
}

func TestWindowsSupportedGitPathsImportAndPublishWithExactSpelling(t *testing.T) {
	fixture := newFixture(t)
	for path, data := range map[string]string{
		"Docs/Guide.TXT":       "guide\n",
		"notes with space.txt": "notes\n",
		"caf\u00e9.txt":        "unicode\n",
	} {
		writeFile(t, filepath.Join(fixture.source, filepath.FromSlash(path)), data)
	}
	base := fixture.commit("refs/heads/main", "supported Windows paths")
	checkout := fixture.clone()
	snapshot, err := checkout.Snapshot(t.Context())
	if err != nil || snapshot.Head != base || len(snapshot.Files) != 4 {
		t.Fatalf("supported source snapshot head=%q files=%v error=%v", snapshot.Head, snapshot.Files, err)
	}
	seen := make(map[string]string, len(snapshot.Files))
	for _, file := range snapshot.Files {
		seen[file.Path] = string(file.Data)
	}
	if seen["Docs/Guide.TXT"] != "guide\n" || seen["notes with space.txt"] != "notes\n" || seen["caf\u00e9.txt"] != "unicode\n" {
		t.Fatalf("source spellings or bytes changed: %v", seen)
	}
	const branch = "minion/fix-windows-paths"
	if err := checkout.NewBranch(t.Context(), branch); err != nil {
		t.Fatal(err)
	}
	changes := []workspace.Change{
		{Path: "Docs/Guide.TXT", Mode: workspace.Regular, Data: []byte("reviewed guide\n")},
		{Path: "new file.txt", Mode: workspace.Regular, Data: []byte("new\n")},
	}
	if err := workspace.ValidateChangedPaths([]string{"Docs/Guide.TXT", "README.md", "caf\u00e9.txt", "notes with space.txt"}, changes); err != nil {
		t.Fatal(err)
	}
	if err := checkout.Import(t.Context(), base, changes); err != nil {
		t.Fatal(err)
	}
	var recorded string
	if err := checkout.commitAndPush(t.Context(), fixture.remote, branch, "bot", "reviewed Windows paths", testCommitDate, func(sha string) error { recorded = sha; return nil }); err != nil {
		t.Fatal(err)
	}
	if published := fixture.git(fixture.source, "--git-dir="+fixture.remote, "rev-parse", "refs/heads/"+branch); published != recorded || published == base {
		t.Fatalf("published SHA=%q recorded=%q base=%q", published, recorded, base)
	}
	listing := fixture.git(fixture.source, "--git-dir="+fixture.remote, "ls-tree", "-r", "--name-only", "-z", branch)
	for _, name := range []string{"Docs/Guide.TXT", "README.md", "caf\u00e9.txt", "notes with space.txt", "new file.txt"} {
		if !strings.Contains(listing, name+"\x00") {
			t.Fatalf("published tree lost exact path %q: %q", name, listing)
		}
	}
	for _, change := range changes {
		var output bytes.Buffer
		if err := runGitInputTo(t.Context(), fixture.source, nil, &output, "--git-dir="+fixture.remote, "show", "refs/heads/"+branch+":"+change.Path); err != nil || !bytes.Equal(output.Bytes(), change.Data) {
			t.Fatalf("published content at %q=%q, error=%v; want %q", change.Path, output.Bytes(), err, change.Data)
		}
	}
}
