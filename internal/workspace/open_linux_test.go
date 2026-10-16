//go:build linux

package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestCollectionRefusesHardlinkedFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	const contents = "outside file contents"
	if err := os.WriteFile(outside, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "file")
	if err := os.Link(outside, name); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name string
		read func() (int, error)
	}{
		{"regular file", func() (int, error) { data, err := ReadRegular(name, MaxBytes); return len(data), err }},
		{"source scan", func() (int, error) {
			return 0, CheckUnchanged(context.Background(), root, []File{{Path: "file", Mode: Regular, Data: []byte(contents)}})
		}},
		{"selected changes", func() (int, error) {
			changes, err := ReadChanges(context.Background(), root, []Change{{Path: "file", Mode: Regular}})
			return len(changes), err
		}},
	} {
		t.Run(check.name, func(t *testing.T) {
			count, err := check.read()
			if count != 0 || err == nil || !strings.Contains(err.Error(), "exactly one hard link") {
				t.Fatalf("hardlink collection returned %d entries/bytes, error=%v", count, err)
			}
		})
	}
	if err := os.Remove(outside); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadRegular(name, MaxBytes); err != nil || string(data) != contents {
		t.Fatalf("single remaining link=%q,%v", data, err)
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
	if err := CheckUnchanged(context.Background(), root, base); err != nil {
		t.Fatalf("scan below execute-only parent=%v", err)
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
	changes, err := ReadChanges(context.Background(), root, []Change{{Path: "changed", Mode: Executable}, {Path: "deleted", Delete: true}})
	if err != nil || len(changes) != 2 || changes[0].Mode != Executable || string(changes[0].Data) != string([]byte{0, 255}) {
		t.Fatalf("selected changes=%+v,%v", changes, err)
	}
	for _, test := range []struct {
		selected []Change
		want     string
	}{
		{[]Change{{Path: "ignored/secret", Mode: Regular}}, "not a directory"},
		{[]Change{{Path: "../outside", Mode: Regular}}, "unsafe"},
		{[]Change{{Path: ".git/config", Mode: Regular}}, "unsafe"},
		{[]Change{{Path: "changed", Mode: Regular}}, "selected change mode differs"},
		{[]Change{{Path: "changed"}}, "invalid changed mode"},
		{[]Change{{Path: "changed", Delete: true, Data: []byte("invalid")}}, "selection contains file data"},
	} {
		if _, err := ReadChanges(context.Background(), root, test.selected); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("selection %+v validation=%v want%q", test.selected, err, test.want)
		}
	}
}

func TestCollectionRefusesRootSymlinksEvenForDeleteOnlySelections(t *testing.T) {
	root := filepath.Join(t.TempDir(), "link")
	outside := privateRoot(t)
	base := []File{{Path: "file", Mode: Regular, Data: []byte("outside")}}
	if err := Materialize(outside, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	for _, check := range []func() error{
		func() error { return CheckRoot(root) },
		func() error { return CheckUnchanged(context.Background(), root, base) },
		func() error { _, err := ReadChanges(context.Background(), root, nil); return err },
		func() error {
			_, err := ReadChanges(context.Background(), root, []Change{{Path: "file", Delete: true}})
			return err
		},
	} {
		if err := check(); !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, syscall.ELOOP) {
			t.Fatalf("root symlink was not refused by anchored open: %v", err)
		}
	}
	if err := Apply(root, []Change{{Path: "file", Delete: true}}); err == nil || !strings.Contains(err.Error(), "workspace root") {
		t.Fatalf("Apply accepted a symlinked workspace root: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(outside, "file")); err != nil || string(data) != "outside" {
		t.Fatalf("symlink-root deletion changed outside file=%q,%v", data, err)
	}
}

func TestReadRegularRefusesFIFOWithoutBlocking(t *testing.T) {
	name := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(name, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadRegular(name, MaxBytes)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not regular") {
			t.Fatalf("FIFO validation=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("opening FIFO blocked waiting for a writer")
	}
}

type fileInfoWithStat struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (info fileInfoWithStat) Sys() any { return &info.stat }

func TestReadStabilityIncludesChangeTime(t *testing.T) {
	name := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(name, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	before := fileInfoWithStat{FileInfo: info, stat: *info.Sys().(*syscall.Stat_t)}
	after := before
	if !sameChangeTime(before, after) {
		t.Fatal("unchanged metadata was refused")
	}
	after.stat.Ctim.Sec++
	if sameChangeTime(before, after) {
		t.Fatal("restored size, mode and mtime hid a changed ctime")
	}
}

func TestApplyRefusesDeletionThroughSymlinkedParent(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	name := filepath.Join(outside, "file")
	if err := os.WriteFile(name, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "parent")); err != nil {
		t.Fatal(err)
	}
	err := Apply(root, []Change{{Path: "parent/file", Delete: true}})
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("symlinked deletion parent validation=%v", err)
	}
	if data, err := os.ReadFile(name); err != nil || string(data) != "outside" {
		t.Fatalf("outside file changed=%q,%v", data, err)
	}
}
