package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if err := CheckUnchanged(context.Background(), root, want); err != nil {
		t.Fatalf("frozen candidate differs from final changes: %v", err)
	}
}

func TestMaterializeChangesRefusesFilesystemAliases(t *testing.T) {
	for _, names := range [][2]string{{"ß", "ss"}, {"é", "e\u0301"}} {
		t.Run(names[0], func(t *testing.T) {
			root := privateRoot(t)
			base := []File{{Path: names[0], Mode: Regular, Data: []byte("baseline")}}
			changes := []Change{{Path: names[1], Mode: Regular, Data: []byte("candidate")}}
			err := MaterializeChanges(root, base, changes)
			if err != nil && !errors.Is(err, os.ErrExist) {
				t.Fatal(err)
			}
			if err != nil {
				entries, readErr := os.ReadDir(root)
				if readErr != nil || len(entries) != 1 {
					t.Fatalf("alias refusal left unexpected files: %v, %v", entries, readErr)
				}
				want := "baseline"
				if entries[0].Name() == names[1] {
					want = "candidate"
				} else if entries[0].Name() != names[0] {
					t.Fatalf("alias refusal left unexpected spelling: %q", entries[0].Name())
				}
				data, readErr := os.ReadFile(filepath.Join(root, entries[0].Name()))
				if readErr != nil || string(data) != want {
					t.Fatalf("alias overwrote first exclusive file: %q, %v", data, readErr)
				}
				return
			}
			data, readErr := os.ReadFile(filepath.Join(root, names[0]))
			if readErr != nil || string(data) != "baseline" {
				t.Fatalf("alias overwrote baseline: %q, %v (materialize: %v)", data, readErr, err)
			}
			data, readErr = os.ReadFile(filepath.Join(root, names[1]))
			if readErr != nil || string(data) != "candidate" {
				t.Fatalf("distinct candidate file=%q, %v", data, readErr)
			}
		})
	}
}

func TestMaterializeChangesPreservesSeparateTransferLimits(t *testing.T) {
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
	// Stop before writing 65 MiB and 10,100 files, after all candidate validation.
	root := privateRoot(t)
	marker := filepath.Join(root, "marker")
	if err := os.WriteFile(marker, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeChanges(root, base, changes); err == nil || err.Error() != "workspace root is not empty" {
		t.Fatalf("valid source plus bounded growth was rejected before materialization: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "unchanged" {
		t.Fatalf("refused root was modified: %q, %v", got, err)
	}
}

func TestMaterializeChangesValidatesBeforeWriting(t *testing.T) {
	base := []File{{Path: "Dir/stay", Mode: Regular}}
	for i, changes := range [][]Change{
		{{Path: "missing", Delete: true}},
		{{Path: "dir/new", Mode: Regular}},
		{{Path: ".git./config", Mode: Regular}},
		{{Path: "new", Mode: Regular, Data: make([]byte, MaxChangeBytes+1)}},
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
	for _, name := range []string{"literal\\backslash", ".git./config"} {
		root := privateRoot(t)
		if err := MaterializeChanges(root, []File{{Path: name, Mode: Regular}}, nil); err == nil || !strings.Contains(err.Error(), "unsafe fix source path") {
			t.Fatalf("unsafe candidate baseline %q accepted: %v", name, err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("invalid baseline wrote files: %v, %v", entries, err)
		}
	}
}

func TestSnapshotSingleListByteLimit(t *testing.T) {
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
	changes, err := ReadChanges(context.Background(), root, selected)
	if err != nil || len(changes) != 2 || len(changes[0].Data)+len(changes[1].Data) != MaxChangeBytes {
		t.Fatalf("exact cumulative byte limit: count=%d, err=%v", len(changes), err)
	}
	if err := os.WriteFile(second, data, 0600); err != nil {
		t.Fatal(err)
	}
	changes, err = ReadChanges(context.Background(), root, selected)
	if changes != nil || err == nil || !strings.Contains(err.Error(), `changed path "second"`) || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("cumulative changed-file overflow accepted: count=%d, err=%v", len(changes), err)
	}
}
