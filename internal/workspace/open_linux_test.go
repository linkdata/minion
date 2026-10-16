//go:build linux

package workspace

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectionRefusesSymlinkParents(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("outside contents"), 0600); err != nil {
		t.Fatal(err)
	}

	parent := filepath.Join(root, "parent")
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(parent, "secret")
	if data, err := ReadRegular(name, MaxBytes); err == nil {
		t.Fatalf("read outside source through parent: %q", data)
	}
	visited := false
	if err := walk(context.Background(), parent, func(string, fs.DirEntry) error { visited = true; return nil }); err == nil || visited {
		t.Fatalf("walk followed root symlink: visited=%v error=%v", visited, err)
	}
}

func TestWalkRefusesDirectoryReplacedAfterQueueing(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside contents"), 0600); err != nil {
		t.Fatal(err)
	}
	visitedOutside := false
	err := walk(context.Background(), root, func(name string, entry fs.DirEntry) error {
		if name == parent {
			if err := os.Rename(parent, filepath.Join(root, "original")); err != nil {
				return err
			}
			return os.Symlink(outside, parent)
		}
		if entry.Name() == "secret" {
			visitedOutside = true
		}
		return nil
	})
	if err == nil || visitedOutside {
		t.Fatalf("queued directory followed replacement symlink: visited=%v error=%v", visitedOutside, err)
	}
}

func TestCollectionTraversesExecuteOnlyParents(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "traverse")
	root := filepath.Join(parent, "source")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	base := []File{{Path: "file", Mode: Regular, Data: []byte("contents")}}
	if err := Materialize(root, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0700); err != nil {
			t.Error(err)
		}
	})
	data, err := ReadRegular(filepath.Join(root, "file"), MaxBytes)
	if err != nil || string(data) != "contents" {
		t.Fatalf("read through execute-only parent=%q,%v", data, err)
	}
	changes, err := Changes(context.Background(), root, base)
	if err != nil || len(changes) != 0 {
		t.Fatalf("scan below execute-only parent=%+v,%v", changes, err)
	}
}

func TestReadChangesUsesOnlySelectedRegularPaths(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "changed"), []byte{0, 255}, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "ignored")); err != nil {
		t.Fatal(err)
	}
	changes, err := ReadChanges(context.Background(), root, []Change{{Path: "changed"}, {Path: "deleted", Delete: true}})
	if err != nil || len(changes) != 2 || changes[0].Mode != Executable || string(changes[0].Data) != string([]byte{0, 255}) {
		t.Fatalf("selected changes=%+v,%v", changes, err)
	}
	for _, selected := range [][]Change{
		{{Path: "ignored/secret"}}, {{Path: "../outside"}}, {{Path: ".git/config"}},
		{{Path: "changed", Mode: Regular}}, {{Path: "changed", Delete: true, Data: []byte("invalid")}},
	} {
		if _, err := ReadChanges(context.Background(), root, selected); err == nil {
			t.Fatalf("accepted unsafe selection %+v", selected)
		}
	}
}
