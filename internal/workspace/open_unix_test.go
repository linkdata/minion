//go:build linux || darwin

package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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
	if err := walk(t.Context(), parent, false, func(string, fs.DirEntry) error { visited = true; return nil }); err == nil || visited {
		t.Fatalf("walk followed root symlink: visited=%v error=%v", visited, err)
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
		writer, err := os.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			t.Errorf("release FIFO reader: %v", err)
		} else {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("FIFO reader did not stop")
			}
			if err := writer.Close(); err != nil {
				t.Error(err)
			}
		}
		t.Fatal("opening FIFO blocked waiting for a writer")
	}
}

func TestReadRegularKeepsEmptyAndExactLimitFiles(t *testing.T) {
	for _, contents := range []string{"", "x", "bounded contents"} {
		t.Run(strconv.Itoa(len(contents)), func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "source")
			if err := os.WriteFile(name, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			data, err := ReadRegular(name, int64(len(contents)))
			if err != nil || string(data) != contents {
				t.Fatalf("exact-limit contents=%q error=%v", data, err)
			}
		})
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
			return 0, CheckUnchanged(t.Context(), root, []File{{Path: "file", Mode: Regular, Data: []byte(contents)}})
		}},
		{"selected changes", func() (int, error) {
			changes, err := ReadChanges(t.Context(), root, []Change{{Path: "file", Mode: Regular}})
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

func TestApplyRefusesHardlinkedTargetsBeforeTruncation(t *testing.T) {
	root := privateRoot(t)
	outside := filepath.Join(t.TempDir(), "outside")
	const contents = "outside contents"
	if err := os.WriteFile(outside, []byte(contents), 0640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, "file")
	if err := os.Link(outside, name); err != nil {
		t.Fatal(err)
	}
	change := []Change{{Path: "file", Mode: Executable, Data: []byte("updated")}}
	if err := Apply(root, change); err == nil || !strings.Contains(err.Error(), "exactly one hard link") {
		t.Fatalf("hardlinked target was accepted: %v", err)
	}
	for _, path := range []string{outside, name} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != contents {
			t.Fatalf("hardlinked contents changed: %q, %v", data, err)
		}
		info, err := os.Stat(path)
		if err != nil || !os.SameFile(before, info) || info.Mode() != before.Mode() {
			t.Fatalf("hardlinked inode or mode changed: %v, %v", info, err)
		}
	}
	if err := os.Remove(outside); err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, change); err != nil {
		t.Fatalf("single-link update was refused: %v", err)
	}
	data, err := os.ReadFile(name)
	if err != nil || string(data) != "updated" {
		t.Fatalf("single-link update=%q, %v", data, err)
	}
}

func TestWalkRefusesDirectoryReplacedAfterListing(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside contents"), 0600); err != nil {
		t.Fatal(err)
	}
	visitedOutside, replaced := false, false
	err := walk(t.Context(), root, false, func(name string, entry fs.DirEntry) error {
		if name == parent {
			if err := os.Rename(parent, filepath.Join(root, "original")); err != nil {
				return err
			}
			if err := os.Symlink(outside, parent); err != nil {
				return err
			}
			replaced = true
		}
		if entry.Name() == "secret" {
			visitedOutside = true
		}
		return nil
	})
	if !replaced || visitedOutside || !errors.Is(err, syscall.ELOOP) && !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("directory replacement check failed: replaced=%v visitedOutside=%v error=%v", replaced, visitedOutside, err)
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
		func() error { return CheckUnchanged(t.Context(), root, base) },
		func() error { _, err := ReadChanges(t.Context(), root, nil); return err },
		func() error {
			_, err := ReadChanges(t.Context(), root, []Change{{Path: "file", Delete: true}})
			return err
		},
	} {
		if err := check(); !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, syscall.ELOOP) {
			t.Fatalf("root symlink was not refused by anchored open: %v", err)
		}
	}
	if err := Apply(root, []Change{{Path: "file", Delete: true}}); !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("Apply did not refuse the symlinked root through its anchored open: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(outside, "file")); err != nil || string(data) != "outside" {
		t.Fatalf("symlink-root deletion changed outside file=%q,%v", data, err)
	}
}

type fileInfoWithStat struct {
	os.FileInfo
	stat     syscall.Stat_t
	size     int64
	mode     fs.FileMode
	modified time.Time
}

func (info fileInfoWithStat) Sys() any           { return &info.stat }
func (info fileInfoWithStat) Size() int64        { return info.size }
func (info fileInfoWithStat) Mode() fs.FileMode  { return info.mode }
func (info fileInfoWithStat) ModTime() time.Time { return info.modified }

func TestReadStabilityRejectsChangedMetadata(t *testing.T) {
	name := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(name, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	before := fileInfoWithStat{FileInfo: info, stat: *info.Sys().(*syscall.Stat_t), size: info.Size(), mode: info.Mode(), modified: info.ModTime()}
	beforeChange, err := changeTime(before, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*fileInfoWithStat)
		want   string
	}{
		{"size", func(after *fileInfoWithStat) { after.size++ }, "file changed"},
		{"mode", func(after *fileInfoWithStat) { after.mode ^= 0100 }, "file changed"},
		{"mtime", func(after *fileInfoWithStat) { after.modified = after.modified.Add(time.Second) }, "file changed"},
		{"ctime", bumpTestChangeTime, "file changed"},
		{"hard link", func(after *fileInfoWithStat) { after.stat.Nlink++ }, "hard links changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			after := before
			test.change(&after)
			afterChange, err := changeTime(after, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = checkReadStability(before, after, int(before.Size()), nil, beforeChange == afterChange)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("stability check=%v, want %q", err, test.want)
			}
		})
	}
}
