package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"
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
	if err != nil || !sourceModeMatches(info, Executable) {
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
	for i, test := range []struct {
		name  string
		valid bool
	}{
		{name: "."}, {name: "../outside"}, {name: "a/.GiT/hooks/x"}, {name: "nul\x00file"},
		{name: strings.Repeat("x", 256)}, {name: strings.Repeat("x", 255), valid: true},
	} {
		t.Run("path-"+strconv.Itoa(i), func(t *testing.T) {
			err := Materialize(privateRoot(t), []File{{Path: test.name, Mode: Regular}})
			if (err == nil) != test.valid || err != nil && !strings.Contains(err.Error(), "invalid source file") {
				t.Fatalf("path %q validation=%v", test.name, err)
			}
			if err := ValidateChanges([]Change{{Path: test.name, Delete: true}}); (err == nil) != test.valid {
				t.Fatalf("deleted path %q validation=%v", test.name, err)
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

func TestDefaultSnapshotRejectsOmittedFileOverlap(t *testing.T) {
	for i, test := range []struct{ file, omitted string }{
		{"file", "file"}, {"dir/file", "dir"},
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
		{"Foo/a", "Foo/b"}, {"Foo/a", "foo/b"},
	} {
		if err := ValidateSnapshot(Snapshot{Head: strings.Repeat("a", 40), Files: []File{{Path: test.file, Mode: Regular}}, Omitted: []string{test.omitted}}); err != nil {
			t.Fatalf("rejected disjoint default omission: %+v,%v", test, err)
		}
	}
}

func TestSnapshotHeadOmittedRequiresCurrentOmission(t *testing.T) {
	snapshot := Snapshot{Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("b", 40), Files: []File{{Path: "source.txt", Mode: Regular}}, Omitted: []string{"head-link", "base-link"}, HeadOmitted: []string{"head-link"}}
	if err := ValidateSnapshot(snapshot); err != nil {
		t.Fatalf("valid head link rejected: %v", err)
	}
	snapshot.HeadOmitted = []string{"unknown-link"}
	if err := ValidateSnapshot(snapshot); err == nil {
		t.Fatal("head link absent from omitted union accepted")
	}
	snapshot.HeadOmitted = []string{"source.txt"}
	snapshot.Omitted = append(snapshot.Omitted, "source.txt")
	if err := ValidateSnapshot(snapshot); err == nil {
		t.Fatal("head link overlapping regular source accepted")
	}
}

func TestSourcePreservesGitLegalCaseAndBackslashPaths(t *testing.T) {
	if runtime.GOOS == "windows" || !caseSensitiveTempDir(t) {
		t.Skip("case-distinct and backslash path fixture requires a case-sensitive Unix filesystem")
	}
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

func caseSensitiveTempDir(t *testing.T) bool {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "case-probe"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(root, "CASE-PROBE"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return errors.Is(err, os.ErrNotExist)
}

func TestCaseOnlyRenameAppliesDeletionBeforeCreation(t *testing.T) {
	for _, addFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "deletion first", true: "creation first"}[addFirst], func(t *testing.T) {
			root := privateRoot(t)
			base := []File{{Path: "Foo/a", Mode: Regular, Data: []byte("before")}}
			if err := Materialize(root, base); err != nil {
				t.Fatal(err)
			}
			changes := []Change{{Path: "Foo/a", Delete: true}, {Path: "foo/a", Mode: Regular, Data: []byte("after")}}
			if addFirst {
				changes[0], changes[1] = changes[1], changes[0]
			}
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
		})
	}
	for _, invalid := range [][]Change{
		{{Path: "Foo/a", Delete: true}, {Path: "Foo/a", Mode: Regular}},
		{{Path: "Foo/a", Delete: true}, {Path: "foo/a", Delete: true}},
		{{Path: "Foo/a", Mode: Regular}, {Path: "foo/a", Mode: Regular}},
		{{Path: "Foo/a", Delete: true}, {Path: "foo/a", Mode: Regular}, {Path: "FOO/a", Delete: true}},
		{{Path: "Foo/a", Delete: true}, {Path: "foo/a", Mode: Regular}, {Path: "foo/a", Mode: Regular}},
		{{Path: "foo/a", Mode: Regular}, {Path: "Foo/a", Delete: true}, {Path: "Foo/a", Delete: true}},
	} {
		if err := ValidateChanges(invalid); err == nil {
			t.Fatalf("accepted duplicated rename operations: %+v", invalid)
		}
	}
}

func TestMaterializeRefusesNonemptyRoot(t *testing.T) {
	root := privateRoot(t)
	if err := os.WriteFile(filepath.Join(root, "source"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Materialize(root, nil); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("nonempty root validation=%v", err)
	}
}

func TestWorkspaceRefusesLinks(t *testing.T) {
	root, outside := privateRoot(t), t.TempDir()
	sentinel := filepath.Join(outside, "file")
	if err := os.WriteFile(sentinel, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"parent": outside, "target": sentinel} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("Windows symlink fixture is unavailable: %v", err)
			}
			t.Fatal(err)
		}
	}
	if err := CheckUnchanged(t.Context(), root, nil); err == nil || !strings.Contains(err.Error(), "workspace changed") {
		t.Fatalf("generated symlink validation=%v", err)
	}
	for _, tc := range []struct {
		name, path, want string
		remove           bool
	}{
		{"target write", "target", "not regular", false},
		{"target deletion", "target", "not regular", true},
		{"parent write", "parent/file", "not a directory", false},
		{"parent deletion", "parent/file", "not a directory", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			change := Change{Path: tc.path, Delete: tc.remove}
			if !tc.remove {
				change.Mode = Regular
				change.Data = []byte("changed")
			}
			if err := Apply(root, []Change{change}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("symlink change accepted: %v", err)
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside" {
				t.Fatalf("outside file changed=%q,%v", data, err)
			}
		})
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
		func(s *Snapshot) { s.Repository = make([]byte, MaxBytes+1) },
		func(s *Snapshot) {
			s.Files = make([]File, MaxFiles+1)
			for i := range s.Files {
				s.Files[i] = File{Path: fmt.Sprintf("file-%d", i), Mode: Regular}
			}
		},
		func(s *Snapshot) { s.ChangedPaths = []string{"a"} },
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
		if (len(bad.Diff) > MaxContextBytes || len(bad.Repository) > MaxBytes) && !strings.Contains(err.Error(), "exceeds limits") {
			t.Fatalf("context bounds did not reach limit validation: %v", err)
		}
		if len(bad.ChangedPaths) != 0 && !strings.Contains(err.Error(), "lacks source identity") {
			t.Fatalf("context identity refusal lost: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckUnchanged(ctx, t.TempDir(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context validation=%v", err)
	}
}

func TestSnapshotContextPathCountLimits(t *testing.T) {
	for _, field := range []string{"omitted", "changed"} {
		for _, count := range []int{MaxFiles, MaxFiles + 1} {
			t.Run(fmt.Sprintf("%s/%d", field, count), func(t *testing.T) {
				snapshot := Snapshot{Head: strings.Repeat("a", 40)}
				paths := make([]string, count)
				for i := range paths {
					paths[i] = fmt.Sprintf("path-%05d", i)
				}
				want := "snapshot context exceeds limits"
				if field == "omitted" {
					snapshot.Omitted = paths
				} else {
					snapshot.Base, snapshot.MergeBase = snapshot.Head, snapshot.Head
					snapshot.ChangedPaths = paths
					want = "snapshot changed paths or incomplete diff is invalid"
				}
				err := ValidateSnapshot(snapshot)
				if count == MaxFiles {
					if err != nil {
						t.Fatalf("exact path count refused: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("path count overflow: %v", err)
				}
			})
		}
	}
}

func TestSnapshotRefusesUnsafeOrRepeatedPathLists(t *testing.T) {
	for _, field := range []string{"omitted", "changed"} {
		for _, paths := range [][]string{{"../outside"}, {"repeat", "repeat"}} {
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

func TestMaterializeRefusesSymlinkRoot(t *testing.T) {
	target := privateRoot(t)
	root := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, root); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows symlink privilege unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := Materialize(root, []File{{Path: "source", Mode: Regular}}); err == nil {
		t.Fatal("materialized through symlink root")
	}
	if _, err := os.Stat(filepath.Join(target, "source")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused materialization wrote source: %v", err)
	}
}

func TestApplyRootErrorDoesNotRequirePrivacy(t *testing.T) {
	root := privateRoot(t)
	name := filepath.Join(root, "file")
	if err := os.WriteFile(name, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	changes := []Change{{Path: "new", Mode: Regular, Data: []byte("imported")}}
	if err := Apply(name, changes); err == nil || runtime.GOOS == "windows" && !strings.Contains(err.Error(), "not a plain directory") || runtime.GOOS != "windows" && !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("incorrect root requirement: %v", err)
	}
	if data, err := os.ReadFile(name); err != nil || string(data) != "unchanged" {
		t.Fatalf("invalid root changed its contents: %q, %v", data, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 || entries[0].Name() != "file" {
		t.Fatalf("invalid root caused writes: %v, %v", entries, err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, changes); err != nil {
		t.Fatalf("Apply imposed a private-root requirement: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "new")); err != nil || string(data) != "imported" {
		t.Fatalf("public directory import failed: %q, %v", data, err)
	}
	if info, err := os.Stat(root); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0755 {
		t.Fatalf("Apply changed root privacy: %v, %v", info, err)
	}
}

func TestCheckUnchangedNamesFirstDeletedPath(t *testing.T) {
	root := privateRoot(t)
	base := []File{{Path: "z-last", Mode: Regular}, {Path: "a-first", Mode: Regular}}
	for range 5 {
		if err := CheckUnchanged(context.Background(), root, base); err == nil || err.Error() != `workspace changed: deleted "a-first"` {
			t.Fatalf("deletion diagnostic lacks stable path: %v", err)
		}
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
	for _, size := range []int64{int64(len(base[0].Data)) + 1, MaxBytes + 1} {
		if err := os.Truncate(name, size); err != nil {
			t.Fatal(err)
		}
		if err := CheckUnchanged(context.Background(), root, base); err == nil || !strings.Contains(err.Error(), "byte limit") || !strings.Contains(err.Error(), "workspace changed") {
			t.Fatalf("grown source size=%d read=%v", size, err)
		}
	}
}

func TestWalkBoundsEntries(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= MaxEntries; i++ {
		if err := os.WriteFile(filepath.Join(root, strconv.Itoa(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(repository, overflow bool) {
		t.Helper()
		visited := 0
		err := walk(context.Background(), root, repository, func(string, os.DirEntry) error {
			visited++
			return nil
		})
		if visited != MaxEntries || (err != nil) != overflow || (err != nil && !strings.Contains(err.Error(), "entry limit")) {
			t.Fatalf("entry-bound walk: repository=%t overflow=%t visited=%d error=%v", repository, overflow, visited, err)
		}
	}
	check(false, true)
	extra := filepath.Join(root, "0")
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	check(false, false)
	metadata := filepath.Join(root, ".git")
	if err := os.Mkdir(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadata, "config"), []byte("separately verified metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	check(true, false)
	if err := os.WriteFile(extra, nil, 0600); err != nil {
		t.Fatal(err)
	}
	check(true, true)
}

func TestValidSHA(t *testing.T) {
	for _, value := range []string{strings.Repeat("a", 40), strings.Repeat("f", 40)} {
		if !ValidSHA(value) {
			t.Fatalf("rejected valid SHA %q", value)
		}
	}
	for _, value := range []string{"bad", strings.Repeat("f", 64), strings.Repeat("g", 40), strings.Repeat("g", 64), strings.Repeat("A", 40), strings.Repeat("F", 64), strings.Repeat("a", 39) + "F"} {
		if ValidSHA(value) {
			t.Fatalf("accepted invalid SHA %q", value)
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
	if err := CheckUnchanged(context.Background(), root, base); err == nil || !strings.Contains(err.Error(), "workspace changed") {
		t.Fatalf("hidden content mutation validation=%v", err)
	}
}

func TestUnicodeFoldCollisionsInFixLayouts(t *testing.T) {
	pair := [2]string{"ſ", "s"}
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
	if len(name) != MaxPathBytes-1 || !validSourcePath(name) || !ValidPath(name) {
		t.Fatalf("rejected path below byte boundary: %d", len(name))
	}
	if validSourcePath(name+"b") || ValidPath(name+"b") {
		t.Fatal("accepted relative path at the transfer byte limit")
	}
}

func TestCollectionUsesOwnerExecuteBitForGitMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the private Git index for executable mode")
	}
	for _, mode := range []os.FileMode{0644, 0645, 0755} {
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
			if err := CheckUnchanged(context.Background(), root, base); (err == nil) != (mode == 0644) || (err != nil && !strings.Contains(err.Error(), "workspace changed")) {
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

func TestFixLayoutIncludesOmittedFilesAndAncestorAliases(t *testing.T) {
	for _, names := range [][2]string{{"link/a", "Link"}, {"Docs/a", "docs/b"}, {"File", "file"}} {
		files := []File{{Path: names[0], Mode: Regular}}
		if err := ValidateFixPaths(files, []string{names[1]}); err == nil || (!strings.Contains(err.Error(), "collision") && !strings.Contains(err.Error(), "duplicate source path")) {
			t.Fatalf("accepted folded file/omission layout %q: %v", names, err)
		}
	}
	if err := ValidateFixPaths([]File{{Path: "folder/a", Mode: Regular}}, []string{"folder/link"}); err != nil {
		t.Fatalf("refused disjoint omitted path in the same directory: %v", err)
	}
	if err := ValidateFixPaths(nil, []string{"Link", "link"}); err == nil || !strings.Contains(err.Error(), "duplicate source path") {
		t.Fatalf("accepted folded omission pair: %v", err)
	}
}

func TestCheckUnchangedRefusesUnsafeWorkerNames(t *testing.T) {
	for _, test := range []struct {
		name      string
		directory bool
	}{{string([]byte{'x', 0xff}), false}, {"new\\output", false}, {".gIt", true}} {
		label := strings.ToValidUTF8(test.name, "invalid")
		if test.directory {
			label += "-directory"
		}
		t.Run(label, func(t *testing.T) {
			if (runtime.GOOS == "windows" && strings.Contains(test.name, "\\")) ||
				((runtime.GOOS == "windows" || runtime.GOOS == "darwin") && !utf8.ValidString(test.name)) {
				t.Skip("host cannot create this literal filename")
			}
			root := t.TempDir()
			name := filepath.Join(root, test.name)
			if test.directory {
				if err := os.MkdirAll(name, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(name, []byte("worker output"), 0600); err != nil {
				t.Fatal(err)
			}
			want := "unsafe"
			if test.name == "new\\output" {
				want = "workspace changed"
			}
			if err := CheckUnchanged(t.Context(), root, nil); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("unsafe worker name %q validation=%v", test.name, err)
			}
		})
	}
}
