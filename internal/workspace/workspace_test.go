package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMaterializeReadChangesAndApply(t *testing.T) {
	base := []File{
		{Path: "binary", Mode: Regular, Data: []byte{0, 0xff, '\n'}},
		{Path: "deleted", Mode: Regular, Data: []byte("old")},
		{Path: "to-directory", Mode: Regular, Data: []byte("file")},
		{Path: "to-file/child", Mode: Regular, Data: []byte("child")},
	}
	root := privateRoot(t)
	if err := Materialize(root, base); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"deleted", "to-directory", "to-file/child"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(root, "to-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "to-directory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "to-directory/child"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "to-file"), []byte("new file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new-empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	changes, err := ReadChanges(context.Background(), root, []Change{
		{Path: "binary", Mode: Executable}, {Path: "deleted", Delete: true},
		{Path: "to-directory", Delete: true}, {Path: "to-directory/child", Mode: Regular},
		{Path: "to-file", Mode: Regular}, {Path: "to-file/child", Delete: true}, {Path: "new-empty", Mode: Regular},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 7 {
		t.Fatalf("changes=%+v", changes)
	}
	copyRoot := privateRoot(t)
	if err := Materialize(copyRoot, base); err != nil {
		t.Fatal(err)
	}
	if err := Apply(copyRoot, changes); err != nil {
		t.Fatal(err)
	}
	final := []File{
		{Path: "binary", Mode: Executable, Data: base[0].Data},
		{Path: "to-directory/child", Mode: Regular, Data: []byte("new")},
		{Path: "to-file", Mode: Regular, Data: []byte("new file")}, {Path: "new-empty", Mode: Regular},
	}
	if err := CheckUnchanged(context.Background(), copyRoot, final); err != nil {
		t.Fatalf("applied files differ from expected final source: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(copyRoot, "binary"))
	if err != nil || !bytes.Equal(data, base[0].Data) {
		t.Fatalf("binary=%v,%v", data, err)
	}
	info, err := os.Stat(filepath.Join(copyRoot, "binary"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("executable mode=%v,%v", info, err)
	}
}

func privateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// TempDir creates its returned numbered child with 0777, subject to umask.
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRejectUnsafeFilesAndChanges(t *testing.T) {
	for i, name := range []string{"", ".", "..", "../outside", "/absolute", "a/../b", "a//b", ".git/config", "a/.GiT/hooks/x", "nul\x00file", strings.Repeat("x", 256)} {
		t.Run("path-"+strconv.Itoa(i), func(t *testing.T) {
			if err := Materialize(t.TempDir(), []File{{Path: name, Mode: Regular}}); err == nil || !strings.Contains(err.Error(), "invalid source file") {
				t.Fatalf("path %q validation=%v", name, err)
			}
			if err := ValidateChanges([]Change{{Path: name, Delete: true}}); err == nil {
				t.Fatalf("accepted deleted path %q", name)
			}
		})
	}
	for _, test := range []struct {
		files []File
		want  string
	}{
		{[]File{{Path: "a", Mode: Regular}, {Path: "a", Mode: Regular}}, "duplicate source path"},
		{[]File{{Path: "a", Mode: Regular}, {Path: "a/b", Mode: Regular}}, "file/directory collision"},
		{[]File{{Path: "link", Mode: 0120000}}, "invalid source file"},
		{[]File{{Path: "permissions", Mode: 0644}}, "invalid source file"},
	} {
		if err := Materialize(t.TempDir(), test.files); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("files=%+v validation=%v want%q", test.files, err, test.want)
		}
	}
	for _, changes := range [][]Change{
		{{Path: "a\\b", Mode: Regular}},
		{{Path: "a", Mode: Regular}, {Path: "A", Mode: Regular}},
		{{Path: "deleted", Mode: Regular, Delete: true}},
		{{Path: "deleted", Data: []byte("content"), Delete: true}},
		{{Path: "a", Mode: Regular}, {Path: "a/b", Mode: Regular}},
		{{Path: "large", Mode: Regular, Data: make([]byte, MaxChangeBytes+1)}},
	} {
		if err := ValidateChanges(changes); err == nil {
			t.Fatalf("accepted changes with %d entries", len(changes))
		}
	}
}

func TestSourceAllowsDirectorySpellingsButChangesRejectCollisions(t *testing.T) {
	for i, paths := range [][]string{
		{"Foo/a", "foo/b"},
		{"Foo/Bar/a", "Foo/bar/b"},
		{"Foo/Bar/a", "foo/Bar/b"},
	} {
		t.Run("case-"+strconv.Itoa(i), func(t *testing.T) {
			files := []File{{Path: paths[0], Mode: Regular}, {Path: paths[1], Mode: Regular}}
			if err := ValidateSnapshot(Snapshot{Head: strings.Repeat("a", 40), Files: files}); err != nil {
				t.Fatalf("rejected case-distinct source directories: %v,%v", paths, err)
			}
			changes := []Change{{Path: paths[0], Mode: Regular}, {Path: paths[1], Mode: Regular}}
			if err := ValidateChanges(changes); err == nil {
				t.Fatalf("accepted changed directory spelling collision: %v", paths)
			}
		})
	}
	if err := ValidateChanges([]Change{{Path: "Foo/a", Mode: Regular}, {Path: "Foo/b", Mode: Regular}}); err != nil {
		t.Fatalf("rejected shared directory spelling: %v", err)
	}
	if err := ValidateChanges([]Change{{Path: "Foo/a", Delete: true}, {Path: "foo/b", Mode: Regular}}); err != nil {
		t.Fatalf("rejected replacement directory spelling: %v", err)
	}
}

func TestChangedPathsChecksFinalSourceLayout(t *testing.T) {
	if err := ValidateChangedPaths([]string{"Foo/stay"}, []Change{{Path: "foo/new", Mode: Regular}}); err == nil {
		t.Fatal("accepted added path with different source directory spelling")
	}
	if err := ValidateChangedPaths(nil, []Change{{Path: "missing", Delete: true}}); err == nil {
		t.Fatal("accepted deletion absent from source")
	}
	for _, test := range []struct {
		base    []string
		changes []Change
	}{
		{[]string{"Foo/a"}, []Change{{Path: "Foo/a", Delete: true}, {Path: "foo/a", Mode: Regular}}},
		{[]string{"entry"}, []Change{{Path: "entry", Delete: true}, {Path: "entry/child", Mode: Regular}}},
		{[]string{"entry/child"}, []Change{{Path: "entry/child", Delete: true}, {Path: "entry", Mode: Regular}}},
	} {
		if err := ValidateChangedPaths(test.base, test.changes); err != nil {
			t.Fatalf("rejected valid final layout: %+v,%v", test, err)
		}
	}
}

func TestDefaultSnapshotRejectsOmittedFileOverlap(t *testing.T) {
	for i, test := range []struct{ file, omitted string }{
		{"file", "file"}, {"dir/file", "dir"}, {"dir", "dir/file"},
	} {
		t.Run("overlap-"+strconv.Itoa(i), func(t *testing.T) {
			snapshot := Snapshot{Head: strings.Repeat("a", 40), Files: []File{{Path: test.file, Mode: Regular}}, Omitted: []string{test.omitted}}
			if err := ValidateSnapshot(snapshot); err == nil {
				t.Fatalf("accepted default source/omission overlap: %+v", test)
			}
			snapshot.Base, snapshot.MergeBase = strings.Repeat("b", 40), strings.Repeat("b", 40)
			if err := ValidateSnapshot(snapshot); err != nil {
				t.Fatalf("rejected possible base-side omission: %+v,%v", test, err)
			}
		})
	}
	for _, test := range []struct{ file, omitted string }{
		{"Foo/a", "Foo/b"}, {"File", "file"}, {"Foo/a", "foo/b"}, {"back\\slash", "Back\\Slash"},
	} {
		if err := ValidateSnapshot(Snapshot{Head: strings.Repeat("a", 40), Files: []File{{Path: test.file, Mode: Regular}}, Omitted: []string{test.omitted}}); err != nil {
			t.Fatalf("rejected disjoint default omission: %+v,%v", test, err)
		}
	}
}

func TestSourcePreservesGitLegalCaseAndBackslashPaths(t *testing.T) {
	files := []File{
		{Path: "include/xt_connmark.h", Mode: Regular, Data: []byte("lowercase")},
		{Path: "include/xt_CONNMARK.h", Mode: Regular, Data: []byte("uppercase")},
		{Path: "Docs/a.md", Mode: Regular, Data: []byte("upper directory")},
		{Path: "docs/b.md", Mode: Regular, Data: []byte("lower directory")},
		{Path: "literal\\backslash", Mode: Regular, Data: []byte("backslash")},
	}
	snapshot := Snapshot{
		Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("b", 40),
		Files: files, ChangedPaths: []string{"Docs/a.md", "docs/b.md", "literal\\backslash"},
		Omitted: []string{"Link", "link", "literal\\omission"},
	}
	if err := ValidateSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	root := privateRoot(t)
	if err := Materialize(root, files); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := ReadRegular(filepath.Join(root, file.Path), MaxBytes)
		if err != nil || !bytes.Equal(data, file.Data) {
			t.Fatalf("source path %q=%q,%v", file.Path, data, err)
		}
	}
	if err := CheckUnchanged(context.Background(), root, files); err != nil {
		t.Fatalf("unchanged Git-legal source=%v", err)
	}
}

func TestCaseOnlyRenameAppliesDeletionBeforeCreation(t *testing.T) {
	root := privateRoot(t)
	base := []File{{Path: "Foo/a", Mode: Regular, Data: []byte("before")}}
	if err := Materialize(root, base); err != nil {
		t.Fatal(err)
	}
	changes := []Change{{Path: "Foo/a", Delete: true}, {Path: "foo/a", Mode: Regular, Data: []byte("after")}}
	if err := ValidateChanges(changes); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChangedPaths([]string{"Foo/a"}, changes); err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, changes); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "foo" {
		t.Fatalf("renamed root entries=%v,%v", entries, err)
	}
	if err := CheckUnchanged(context.Background(), root, []File{{Path: "foo/a", Mode: Regular, Data: []byte("after")}}); err != nil {
		t.Fatalf("renamed source=%v", err)
	}
	for _, invalid := range [][]Change{
		{{Path: "Foo/a", Delete: true}, {Path: "Foo/a", Mode: Regular}},
		{{Path: "Foo/a", Delete: true}, {Path: "foo/a", Delete: true}},
		{{Path: "Foo/a", Mode: Regular}, {Path: "foo/a", Mode: Regular}},
		{{Path: "Foo/a", Delete: true}, {Path: "foo/a", Mode: Regular}, {Path: "FOO/a", Delete: true}},
	} {
		if err := ValidateChanges(invalid); err == nil {
			t.Fatalf("accepted duplicated rename operations: %+v", invalid)
		}
	}
}

func TestWorkspaceRefusesLinksAndNonemptyMaterialization(t *testing.T) {
	root := privateRoot(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := CheckUnchanged(context.Background(), root, nil); err == nil || !strings.Contains(err.Error(), "modified supplied source") {
		t.Fatalf("generated symlink validation=%v", err)
	}
	if err := Materialize(root, nil); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("nonempty root validation=%v", err)
	}
	if err := Apply(root, []Change{{Path: "outside/exfiltrate", Mode: Regular}}); err == nil {
		t.Fatal("followed symlink parent")
	}
	if err := Apply(root, []Change{{Path: "outside", Mode: Regular}}); err == nil {
		t.Fatal("overwrote symlink")
	}
	if err := Apply(root, []Change{{Path: "outside", Delete: true}}); err == nil {
		t.Fatal("deleted symlink as regular source")
	}
	if err := os.Remove(filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".GIT"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := CheckUnchanged(context.Background(), root, nil); err == nil || !strings.Contains(err.Error(), "unsafe workspace path") {
		t.Fatalf("generated Git metadata validation=%v", err)
	}
}

func TestSnapshotBoundsAndIdentity(t *testing.T) {
	good := Snapshot{Head: strings.Repeat("a", 40), Files: []File{{Path: "a", Mode: Regular}}}
	if err := ValidateSnapshot(good); err != nil {
		t.Fatal(err)
	}
	for _, modify := range []func(*Snapshot){
		func(s *Snapshot) { s.Head = "main" },
		func(s *Snapshot) { s.Base = strings.Repeat("b", 40) },
		func(s *Snapshot) { s.Base, s.MergeBase = s.Head, s.Head; s.Diff = make([]byte, MaxContextBytes+1) },
		func(s *Snapshot) { s.HistoryRequested = true; s.History = make([]byte, MaxContextBytes+1) },
		func(s *Snapshot) {
			s.Files = make([]File, MaxFiles+1)
			for i := range s.Files {
				s.Files[i] = File{Path: fmt.Sprintf("file-%d", i), Mode: Regular}
			}
		},
		func(s *Snapshot) { s.Omitted = []string{"../outside"} },
		func(s *Snapshot) { s.History = []byte("unrequested") },
		func(s *Snapshot) { s.Base, s.MergeBase = s.Head, s.Head; s.ChangedPaths = []string{"../outside"} },
		func(s *Snapshot) {
			s.Base, s.MergeBase = s.Head, s.Head
			s.DiffIncomplete = true
			s.Diff = []byte("partial")
		},
	} {
		bad := good
		modify(&bad)
		err := ValidateSnapshot(bad)
		if err == nil {
			t.Fatal("accepted invalid snapshot")
		}
		if (len(bad.Diff) > MaxContextBytes || len(bad.History) > MaxContextBytes) && !strings.Contains(err.Error(), "exceeds limits") {
			t.Fatalf("context bounds did not reach limit validation: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckUnchanged(ctx, t.TempDir(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context validation=%v", err)
	}
}

func TestSnapshotRefusesUnsafeOrRepeatedPathLists(t *testing.T) {
	for _, field := range []string{"omitted", "changed"} {
		for _, paths := range [][]string{{""}, {"../outside"}, {"/absolute"}, {".git/config"}, {"repeat", "repeat"}} {
			t.Run(field+fmt.Sprint(paths), func(t *testing.T) {
				snapshot := Snapshot{Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("b", 40)}
				want := "snapshot changed path is unsafe or duplicated"
				if field == "omitted" {
					snapshot.Omitted = paths
					want = fmt.Sprintf("invalid omitted source path %q", paths[len(paths)-1])
				} else {
					snapshot.ChangedPaths = paths
				}
				if err := ValidateSnapshot(snapshot); err == nil || err.Error() != want {
					t.Fatalf("invalid %s paths %q error=%v want=%q", field, paths, err, want)
				}
			})
		}
	}
}

func TestApplyCleansRootAndKeepsRootDirectory(t *testing.T) {
	root := privateRoot(t)
	if err := Materialize(root, []File{{Path: "nested/shallow", Mode: Regular}, {Path: "nested/deep/deleted", Mode: Regular}}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(root+string(os.PathSeparator), []Change{{Path: "nested/shallow", Delete: true}, {Path: "nested/deep/deleted", Delete: true}}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("root removed: %v,%v", info, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("different-depth deletion left entries=%v,%v", entries, err)
	}
	if err := Apply(root, []Change{{Path: "missing", Delete: true}}); err == nil {
		t.Fatal("missing source deletion accepted")
	}
}

func TestMaterializeRefusesNonprivateRoot(t *testing.T) {
	root := privateRoot(t)
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := Materialize(root, []File{{Path: "source", Mode: Regular}}); err == nil || !strings.Contains(err.Error(), "private directory") {
		t.Fatalf("public root validation=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "source")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused materialization wrote source: %v", err)
	}
}

func TestApplyRootErrorDoesNotRequirePrivacy(t *testing.T) {
	root := privateRoot(t)
	name := filepath.Join(root, "file")
	if err := os.WriteFile(name, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Apply(name, nil); err == nil || !strings.Contains(err.Error(), "real directory") || strings.Contains(err.Error(), "private") {
		t.Fatalf("incorrect root requirement: %v", err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, []Change{{Path: "new", Mode: Regular}}); err != nil {
		t.Fatalf("Apply imposed a private-root requirement: %v", err)
	}
}

func TestCheckUnchangedNamesFirstDeletedPath(t *testing.T) {
	root := privateRoot(t)
	base := []File{{Path: "z-last", Mode: Regular}, {Path: "a-first", Mode: Regular}}
	for range 5 {
		if err := CheckUnchanged(context.Background(), root, base); err == nil || err.Error() != `read-only action modified supplied source: deleted "a-first"` {
			t.Fatalf("deletion diagnostic lacks stable path: %v", err)
		}
	}
}

func TestCheckUnchangedRefusesAdditionsAndDeletions(t *testing.T) {
	for _, mutation := range []string{"add", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			root := privateRoot(t)
			base := []File{{Path: "source", Mode: Regular, Data: []byte("before")}}
			if err := Materialize(root, base); err != nil {
				t.Fatal(err)
			}
			var err error
			if mutation == "add" {
				err = os.WriteFile(filepath.Join(root, "new"), nil, 0600)
			} else {
				err = os.Remove(filepath.Join(root, "source"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckUnchanged(context.Background(), root, base); err == nil || !strings.Contains(err.Error(), "modified supplied source") {
				t.Fatalf("%s validation=%v", mutation, err)
			}
		})
	}
}

func TestCollectionBoundsFileCountsAndBytes(t *testing.T) {
	files := make([]File, MaxFiles+1)
	for i := range files {
		files[i] = File{Path: fmt.Sprintf("source-%d", i), Mode: Regular}
	}
	if err := CheckUnchanged(context.Background(), filepath.Join(t.TempDir(), "absent"), files); err == nil || !strings.Contains(err.Error(), "source exceeds file limit") {
		t.Fatalf("oversized baseline validation=%v", err)
	}
	root := privateRoot(t)
	base := []File{{Path: "source", Mode: Regular, Data: []byte("source")}}
	if err := Materialize(root, base); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "source")
	if data, err := ReadRegular(name, 3); len(data) != 0 || err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("bounded read=%q,%v", data, err)
	}
	if err := os.Truncate(name, MaxBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := CheckUnchanged(context.Background(), root, base); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized source read=%v", err)
	}
}

func TestWalkBoundsEntries(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= MaxEntries; i++ {
		if err := os.WriteFile(filepath.Join(root, strconv.Itoa(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	visited := 0
	err := walk(context.Background(), root, func(string, os.DirEntry) error { visited++; return nil })
	if err == nil || !strings.Contains(err.Error(), "entry limit") || visited != MaxEntries {
		t.Fatalf("entry-bound walk: visited=%d error=%v", visited, err)
	}
}

func TestValidSHA(t *testing.T) {
	for _, value := range []string{strings.Repeat("a", 40), strings.Repeat("f", 64)} {
		if !ValidSHA(value) {
			t.Fatalf("rejected valid SHA %q", value)
		}
	}
	for _, value := range []string{"bad", strings.Repeat("g", 40), strings.Repeat("g", 64), strings.Repeat("A", 40), strings.Repeat("F", 64), strings.Repeat("a", 39) + "F"} {
		if ValidSHA(value) {
			t.Fatalf("accepted invalid SHA %q", value)
		}
	}
}

func TestValidPathUsesTransportGrammar(t *testing.T) {
	for _, name := range []string{"CON", "C:relative", "folder/file"} {
		if !ValidPath(name) {
			t.Fatalf("host path rules rejected relative transport path %q", name)
		}
	}
}

func TestChangedLayoutCollisionDiagnosticIsStable(t *testing.T) {
	base := []string{"Dir/keep", "Foo/keep"}
	changes := []Change{{Path: "foo/new", Mode: Regular}, {Path: "dir/new", Mode: Regular}}
	if err := ValidateChangedPaths(base, changes); err == nil || err.Error() != `source directory spelling collision at "dir/new"` {
		t.Fatalf("unstable layout diagnostic=%v", err)
	}
}

func TestCheckUnchangedDetectsSameSizeContentWithPreservedMtime(t *testing.T) {
	root := privateRoot(t)
	base := []File{{Path: "file", Mode: Regular, Data: []byte("before")}}
	if err := Materialize(root, base); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "file")
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("after!"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := CheckUnchanged(context.Background(), root, base); err == nil || !strings.Contains(err.Error(), "modified supplied source") {
		t.Fatalf("hidden content mutation validation=%v", err)
	}
}

func TestUnicodeFoldCollisionsInFixLayouts(t *testing.T) {
	for _, pair := range [][2]string{{"ſ", "s"}, {"µ", "μ"}, {"ς", "σ"}, {"ϐ", "β"}, {"K", "k"}} {
		t.Run(pair[0]+pair[1], func(t *testing.T) {
			if FoldPath(pair[0]) != FoldPath(pair[1]) {
				t.Fatalf("case-fold equivalents have different keys: %q,%q", pair[0], pair[1])
			}
			for _, names := range [][2]string{pair, {pair[0] + "/a", pair[1] + "/b"}} {
				files := []File{{Path: names[0], Mode: Regular}, {Path: names[1], Mode: Regular}}
				if err := ValidateSnapshot(Snapshot{Head: strings.Repeat("a", 40), Files: files}); err != nil {
					t.Fatalf("rejected exact source names: %v", err)
				}
				if err := ValidateFixPaths(files, nil); err == nil {
					t.Fatalf("accepted folded fix source collision: %q", names)
				}
				changes := []Change{{Path: names[0], Mode: Regular}, {Path: names[1], Mode: Regular}}
				if err := ValidateChanges(changes); err == nil {
					t.Fatalf("accepted folded result collision: %q", names)
				}
				if err := ValidateChangedPaths([]string{names[0]}, changes[1:]); err == nil {
					t.Fatalf("accepted folded unchanged-source collision: %q", names)
				}
			}
		})
	}
	if FoldPath("I") == FoldPath("ı") || FoldPath("I") == FoldPath("İ") {
		t.Fatal("folding conflated distinct dotted/dotless-I classes")
	}
}

func TestValidateFixPathsUsesSourceLimits(t *testing.T) {
	files := make([]File, MaxChanges+1)
	for i := range files {
		files[i] = File{Path: fmt.Sprintf("source/%d", i), Mode: Regular}
	}
	if err := ValidateFixPaths(files, nil); err != nil {
		t.Fatalf("applied change-count limit to complete fix source: %v", err)
	}
	if err := ValidateFixPaths([]File{{Path: "literal\\backslash", Mode: Regular}}, nil); err == nil || !strings.Contains(err.Error(), "unsafe fix source path") {
		t.Fatalf("accepted non-portable fix source: %v", err)
	}
}

func TestRelativePathByteLimit(t *testing.T) {
	name := strings.Repeat("a/", MaxPathBytes/2-1) + "a"
	if len(name) != MaxPathBytes-1 || !ValidSourcePath(name) || !ValidPath(name) {
		t.Fatalf("rejected path below byte boundary: %d", len(name))
	}
	if ValidSourcePath(name+"b") || ValidPath(name+"b") {
		t.Fatal("accepted relative path at the transfer byte limit")
	}
}

func TestSnapshotCombinedHeadAndBaseByteLimit(t *testing.T) {
	data := make([]byte, 1<<20)
	snapshot := Snapshot{
		Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("b", 40),
	}
	for i := 0; i < MaxBytes/len(data)/2; i++ {
		file := File{Path: fmt.Sprintf("file-%d", i), Mode: Regular, Data: data}
		snapshot.Files = append(snapshot.Files, file)
		snapshot.BaseFiles = append(snapshot.BaseFiles, file)
	}
	snapshot.BaseFiles = append(snapshot.BaseFiles, File{Path: "overflow", Mode: Regular, Data: data[:1]})
	if err := ValidateSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), "snapshot files exceed byte limit") {
		t.Fatalf("combined source overflow validation=%v", err)
	}
	snapshot.BaseFiles = snapshot.BaseFiles[:len(snapshot.BaseFiles)-1]
	if err := ValidateSnapshot(snapshot); err != nil {
		t.Fatalf("rejected source exactly at combined limit: %v", err)
	}
}

func TestCollectionSeparatesGitModesAndReadOnlyPermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0644, 0654, 0645, 0744, 0755} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			root := privateRoot(t)
			base := []File{{Path: "file", Mode: Regular, Data: []byte("before")}}
			if err := Materialize(root, base); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(root, "file"), mode); err != nil {
				t.Fatal(err)
			}
			wantMode := Regular
			if mode&0100 != 0 {
				wantMode = Executable
			}
			if err := CheckUnchanged(context.Background(), root, base); (err == nil) != (mode == 0644) || (err != nil && !strings.Contains(err.Error(), "modified supplied source")) {
				t.Fatalf("mode-only validation=%v, want mode=%o", err, wantMode)
			}
			if err := os.WriteFile(filepath.Join(root, "file"), []byte("after"), mode); err != nil {
				t.Fatal(err)
			}
			changes, err := ReadChanges(context.Background(), root, []Change{{Path: "file", Mode: wantMode}})
			if err != nil || len(changes) != 1 || changes[0].Mode != wantMode || string(changes[0].Data) != "after" {
				t.Fatalf("edited file=%+v,%v, want mode=%o", changes, err, wantMode)
			}
		})
	}
}
