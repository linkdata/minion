package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestMaterializeChangesCreatesOnlyFinalFiles(t *testing.T) {
	base := []File{
		{Path: "Foo/old", Mode: Regular},
		{Path: "to-directory", Mode: Regular},
		{Path: "to-file/child", Mode: Regular},
		{Path: "deleted", Mode: Regular},
		{Path: "updated", Mode: Regular, Data: []byte("before")},
		{Path: "unchanged", Mode: Regular, Data: []byte("same")},
	}
	changes := []Change{
		{Path: "foo/new", Mode: Regular, Data: []byte("renamed directory")},
		{Path: "Foo/old", Delete: true},
		{Path: "to-directory/child", Mode: Regular, Data: []byte("new child")},
		{Path: "to-directory", Delete: true},
		{Path: "to-file", Mode: Regular, Data: []byte("new file")},
		{Path: "to-file/child", Delete: true},
		{Path: "deleted", Delete: true},
		{Path: "updated", Mode: Executable, Data: []byte{0, 255}},
	}
	root := privateRoot(t)
	if err := MaterializeChanges(root, base, changes); err != nil {
		t.Fatal(err)
	}
	want := []File{
		{Path: "foo/new", Mode: Regular, Data: []byte("renamed directory")},
		{Path: "to-directory/child", Mode: Regular, Data: []byte("new child")},
		{Path: "to-file", Mode: Regular, Data: []byte("new file")},
		{Path: "updated", Mode: Executable, Data: []byte{0, 255}},
		{Path: "unchanged", Mode: Regular, Data: []byte("same")},
	}
	if err := CheckUnchanged(t.Context(), root, want); err != nil {
		t.Fatalf("frozen candidate differs from final changes: %v", err)
	}
	if err := CheckChangesUnchanged(t.Context(), root, base, changes); err != nil {
		t.Fatalf("candidate differs from separate transfers: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "updated"), []byte("tampered candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckChangesUnchanged(t.Context(), root, base, changes); err == nil || !strings.Contains(err.Error(), "workspace changed") {
		t.Fatalf("candidate mutation went undetected: %v", err)
	}
}

func TestMaterializeChangesHandlesUnicodeAliasesOnHostFilesystem(t *testing.T) {
	root := privateRoot(t)
	base := []File{{Path: "é", Mode: Regular, Data: []byte("baseline")}}
	changes := []Change{{Path: "e\u0301", Mode: Regular, Data: []byte("candidate")}}
	err := MaterializeChanges(root, base, changes)
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err == nil {
		if len(entries) != 2 {
			t.Fatalf("aliased names accepted without exclusive refusal: %v", entries)
		}
		return // This host represents the exact names as distinct files.
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("alias refusal left unexpected files: %v", entries)
	}
	// The sorted candidate spelling is written first; the exclusive baseline write must not truncate it.
	if data, err := os.ReadFile(filepath.Join(root, entries[0].Name())); err != nil || string(data) != "candidate" {
		t.Fatalf("alias overwrote first exclusive file: %q, %v", data, err)
	}
}

func TestChangedFilesPreservesSeparateTransferLimits(t *testing.T) {
	data := make([]byte, 1<<20)
	base := make([]File, MaxFiles)
	for i := range base {
		base[i] = File{Path: fmt.Sprintf("base-%05d", i), Mode: Regular}
		if i < MaxBytes/len(data) {
			base[i].Data = data
		}
	}
	changes := make([]Change, MaxChanges)
	for i := range changes {
		changes[i] = Change{Path: fmt.Sprintf("added-%03d", i), Mode: Regular}
	}
	changes[0].Data = data
	files, err := changedFiles(base, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != MaxFiles+MaxChanges {
		t.Fatalf("separate transfers lost files: got %d, want %d", len(files), MaxFiles+MaxChanges)
	}
	total := 0
	for _, file := range files {
		total += len(file.Data)
	}
	if total != MaxBytes+len(data) {
		t.Fatalf("separate transfers lost bytes: got %d, want %d", total, MaxBytes+len(data))
	}
	byteOverflow := append(slices.Clone(base[:MaxBytes/len(data)]), File{Path: changes[0].Path, Mode: Regular, Data: data})
	if err := validateFiles(byteOverflow); err == nil || !strings.Contains(err.Error(), "source exceeds byte limit") {
		t.Fatalf("candidate canary widened source byte limit: %v", err)
	}
}

func TestCheckChangesUnchangedSkipsOnlyTopLevelGitDirectory(t *testing.T) {
	base := []File{{Path: "source", Mode: Regular, Data: []byte("before")}}
	changes := []Change{{Path: "source", Mode: Regular, Data: []byte("after")}}
	final := []File{{Path: "source", Mode: Regular, Data: []byte("after")}}
	for _, test := range []struct {
		name, path, kind string
		allowed          bool
	}{
		{"top-level directory", ".git", "directory", true},
		{"nested directory", "sub/.git", "directory", false},
		{"metadata file", ".git", "file", false},
		{"case variant", ".GIT", "directory", false},
		{"metadata symlink", ".git", "symlink", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := privateRoot(t)
			if err := MaterializeChanges(root, base, changes); err != nil {
				t.Fatal(err)
			}
			if err := CheckChangesUnchanged(t.Context(), root, base, changes); err != nil {
				t.Fatalf("invalid candidate control: %v", err)
			}
			name := filepath.Join(root, test.path)
			if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
				t.Fatal(err)
			}
			switch test.kind {
			case "directory":
				if err := os.Mkdir(name, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(name, "config"), []byte("metadata"), 0600); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(name, []byte("gitdir: elsewhere"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				symlinkOrSkip(t, privateRoot(t), name)
			}
			err := CheckChangesUnchanged(t.Context(), root, base, changes)
			want := fmt.Sprintf("unsafe workspace path %q", test.path)
			if (err == nil) != test.allowed || (err != nil && !strings.Contains(err.Error(), want)) {
				t.Fatalf("candidate metadata %q: %v, want allowed=%t", test.path, err, test.allowed)
			}
			if err := CheckUnchanged(t.Context(), root, final); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("strict unchanged check skipped metadata %q: %v", test.path, err)
			}
		})
	}
}

func TestMaterializeChangesValidatesBeforeWriting(t *testing.T) {
	base := []File{{Path: "Dir/stay", Mode: Regular}}
	for i, changes := range [][]Change{
		{{Path: "missing", Delete: true}},
		{{Path: "dir/new", Mode: Regular}},
		{{Path: ".git./config", Mode: Regular}},
	} {
		root := privateRoot(t)
		if err := MaterializeChanges(root, base, changes); err == nil {
			t.Fatalf("invalid final candidate %d accepted", i)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("invalid candidate wrote files: %v, %v", entries, err)
		}
	}
	const name = "literal\\backslash"
	root := privateRoot(t)
	want := "unsafe fix source path"
	if runtime.GOOS == "windows" {
		want = "invalid source file"
	}
	if err := MaterializeChanges(root, []File{{Path: name, Mode: Regular}}, nil); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("unsafe candidate baseline %q accepted: %v", name, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid baseline wrote files: %v, %v", entries, err)
	}
}

func TestSnapshotSingleAndCombinedByteLimits(t *testing.T) {
	data := make([]byte, 1<<20)
	files := make([]File, MaxBytes/len(data))
	for i := range files {
		files[i] = File{Path: fmt.Sprintf("file-%d", i), Mode: Regular, Data: data}
	}
	snapshot := Snapshot{Head: strings.Repeat("a", 40), Files: files}
	if err := ValidateSnapshot(snapshot); err != nil {
		t.Fatalf("source at exact byte limit: %v", err)
	}
	snapshot.Files = append(snapshot.Files, File{Path: "overflow", Mode: Regular, Data: data[:1]})
	if err := ValidateSnapshot(snapshot); err == nil || err.Error() != "source exceeds byte limit" {
		t.Fatalf("single-list source overflow: %v", err)
	}
	half := files[:len(files)/2]
	snapshot = Snapshot{Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("b", 40), Files: half, BaseFiles: half}
	if err := ValidateSnapshot(snapshot); err != nil {
		t.Fatalf("source at exact combined byte limit: %v", err)
	}
	snapshot.BaseFiles = append(slices.Clone(half), File{Path: "overflow", Mode: Regular, Data: data[:1]})
	if err := ValidateSnapshot(snapshot); err == nil || err.Error() != "snapshot files exceed byte limit" {
		t.Fatalf("combined source overflow: %v", err)
	}
}

func TestReadChangesCumulativeByteLimit(t *testing.T) {
	root := privateRoot(t)
	data := bytes.Repeat([]byte{'a'}, 600<<10)
	if err := os.WriteFile(filepath.Join(root, "first"), data, 0600); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(root, "second")
	if err := os.WriteFile(second, data[:MaxChangeBytes-len(data)], 0600); err != nil {
		t.Fatal(err)
	}
	selected := []Change{{Path: "first", Mode: Regular}, {Path: "second", Mode: Regular}}
	changes, err := ReadChanges(t.Context(), root, selected)
	if err != nil || len(changes) != 2 || len(changes[0].Data)+len(changes[1].Data) != MaxChangeBytes {
		t.Fatalf("exact cumulative byte limit: count=%d, err=%v", len(changes), err)
	}
	if err := os.WriteFile(second, data, 0600); err != nil {
		t.Fatal(err)
	}
	changes, err = ReadChanges(t.Context(), root, selected)
	if changes != nil || err == nil || !strings.Contains(err.Error(), `changed path "second"`) || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("cumulative changed-file overflow accepted: count=%d, err=%v", len(changes), err)
	}
}
