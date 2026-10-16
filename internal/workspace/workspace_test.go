package workspace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMaterializeChangesAndApply(t *testing.T) {
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
	changes, err := Changes(context.Background(), root, base)
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
	got, err := Changes(context.Background(), copyRoot, base)
	if err != nil || !reflect.DeepEqual(changes, got) {
		t.Fatalf("applied changes=%+v, %v; want %+v", got, err, changes)
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
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRejectUnsafeFilesAndChanges(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../outside", "/absolute", "a/../b", "a//b", "a\\b", ".git/config", "a/.GiT/hooks/x", "nul\x00file", strings.Repeat("x", 256)} {
		t.Run(name, func(t *testing.T) {
			if err := Materialize(t.TempDir(), []File{{Path: name, Mode: Regular}}); err == nil {
				t.Fatalf("accepted path %q", name)
			}
			if err := ValidateChanges([]Change{{Path: name, Delete: true}}); err == nil {
				t.Fatalf("accepted deleted path %q", name)
			}
		})
	}
	for _, files := range [][]File{
		{{Path: "a", Mode: Regular}, {Path: "a", Mode: Regular}},
		{{Path: "a", Mode: Regular}, {Path: "A", Mode: Regular}},
		{{Path: "a", Mode: Regular}, {Path: "a/b", Mode: Regular}},
		{{Path: "link", Mode: 0120000}},
		{{Path: "permissions", Mode: 0644}},
	} {
		if err := Materialize(t.TempDir(), files); err == nil {
			t.Fatalf("accepted %+v", files)
		}
	}
	for _, changes := range [][]Change{
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

func TestWorkspaceRefusesLinksAndNonemptyMaterialization(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if _, err := Changes(context.Background(), root, nil); err == nil {
		t.Fatal("accepted generated symlink")
	}
	if err := Materialize(root, nil); err == nil {
		t.Fatal("accepted nonempty root")
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
	if _, err := Changes(context.Background(), root, nil); err == nil {
		t.Fatal("accepted generated Git metadata")
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
		func(s *Snapshot) { s.Diff = make([]byte, MaxContextBytes+1) },
		func(s *Snapshot) { s.History = make([]byte, MaxContextBytes+1) },
		func(s *Snapshot) { s.Files = make([]File, MaxFiles+1) },
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
		if err := ValidateSnapshot(bad); err == nil {
			t.Fatal("accepted invalid snapshot")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Changes(ctx, t.TempDir(), nil); err == nil {
		t.Fatal("ignored canceled context")
	}
}

func TestApplyCleansRootAndKeepsRootDirectory(t *testing.T) {
	root := privateRoot(t)
	if err := Materialize(root, []File{{Path: "nested/deleted", Mode: Regular}}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(root+string(os.PathSeparator), []Change{{Path: "nested/deleted", Delete: true}}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("root removed: %v,%v", info, err)
	}
	if err := Apply(root, []Change{{Path: "missing", Delete: true}}); err == nil {
		t.Fatal("missing source deletion accepted")
	}
}

func TestChangesDetectsSameSizeContentWithPreservedMtime(t *testing.T) {
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
	changes, err := Changes(context.Background(), root, base)
	if err != nil || len(changes) != 1 || string(changes[0].Data) != "after!" {
		t.Fatalf("hidden content mutation=%+v,%v", changes, err)
	}
}
