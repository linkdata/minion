package workspace

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func windowsJunction(t *testing.T, name, target string) {
	t.Helper()
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", name, target).CombinedOutput(); err != nil {
		t.Fatalf("create junction %q -> %q: %v: %s", name, target, err, output)
	}
}

func TestWindowsCollectionOrdinaryTree(t *testing.T) {
	root := privateRoot(t)
	base := []File{{Path: "source/file", Mode: Regular, Data: []byte("original")}}
	if err := Materialize(root, base); err != nil {
		t.Fatal(err)
	}
	if err := CheckUnchanged(t.Context(), root, base); err != nil {
		t.Fatalf("ordinary source scan: %v", err)
	}
	name := filepath.Join(root, "source", "file")
	if data, err := ReadRegular(name, MaxBytes); err != nil || string(data) != "original" {
		t.Fatalf("ordinary anchored read=%q, %v", data, err)
	}
	changes, err := ReadChanges(t.Context(), root, []Change{{Path: "source/file", Mode: Regular}})
	if err != nil || len(changes) != 1 || string(changes[0].Data) != "original" {
		t.Fatalf("ordinary selected read=%+v, %v", changes, err)
	}
}

func TestWindowsCollectionRefusesLinkParents(t *testing.T) {
	for _, kind := range []string{"symlink", "junction"} {
		t.Run(kind, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			secret := filepath.Join(outside, "secret")
			if err := os.WriteFile(secret, []byte("outside contents"), 0600); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(root, "parent")
			if kind == "junction" {
				windowsJunction(t, parent, outside)
			} else if err := os.Symlink(outside, parent); err != nil {
				t.Fatalf("create directory symlink: %v", err)
			}
			name := filepath.Join(parent, "secret")
			if data, err := ReadRegular(name, MaxBytes); err == nil {
				t.Fatalf("read outside source through %s parent: %q", kind, data)
			}
			if changes, err := ReadChanges(t.Context(), root, []Change{{Path: "parent/secret", Mode: Regular}}); err == nil || len(changes) != 0 {
				t.Fatalf("selected import followed %s parent: changes=%+v error=%v", kind, changes, err)
			}
			matchingTarget := []File{{Path: "parent/secret", Mode: Regular, Data: []byte("outside contents")}}
			if err := CheckUnchanged(t.Context(), root, matchingTarget); err == nil {
				t.Fatalf("source scan accepted %s parent", kind)
			}
			visited := false
			if err := walk(t.Context(), parent, false, func(string, fs.DirEntry) error { visited = true; return nil }); err == nil || visited {
				t.Fatalf("walk followed %s root: visited=%t error=%v", kind, visited, err)
			}
			if data, err := os.ReadFile(secret); err != nil || string(data) != "outside contents" {
				t.Fatalf("outside source changed: %q, %v", data, err)
			}
		})
	}
}

func TestWindowsWalkRefusesDirectoryReplacedAfterListing(t *testing.T) {
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
			windowsJunction(t, parent, outside)
			replaced = true
		}
		if entry.Name() == "secret" {
			visitedOutside = true
		}
		return nil
	})
	if !replaced || visitedOutside || err == nil {
		t.Fatalf("listed directory followed replacement junction: replaced=%t visited=%t error=%v", replaced, visitedOutside, err)
	}
}

func TestWindowsCollectionRefusesReparseRootForDeleteOnlySelection(t *testing.T) {
	for _, kind := range []string{"symlink", "junction"} {
		t.Run(kind, func(t *testing.T) {
			outside := privateRoot(t)
			if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0600); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(t.TempDir(), "job")
			if kind == "junction" {
				windowsJunction(t, root, outside)
			} else if err := os.Symlink(outside, root); err != nil {
				t.Fatalf("create root directory symlink: %v", err)
			}
			matchingTarget := []File{{Path: "file", Mode: Regular, Data: []byte("outside")}}
			for _, check := range []func() error{
				func() error { return CheckRoot(root) },
				func() error { return CheckUnchanged(t.Context(), root, matchingTarget) },
				func() error { _, err := ReadChanges(t.Context(), root, nil); return err },
				func() error {
					_, err := ReadChanges(t.Context(), root, []Change{{Path: "file", Delete: true}})
					return err
				},
				func() error { return Apply(root, []Change{{Path: "file", Delete: true}}) },
			} {
				if err := check(); err == nil {
					t.Fatal("reparse root accepted")
				}
			}
			if data, err := os.ReadFile(filepath.Join(outside, "file")); err != nil || string(data) != "outside" {
				t.Fatalf("refused deletion changed outside file: %q, %v", data, err)
			}
		})
	}
}

func TestWindowsApplyRefusesHardlinkedTargetBeforeTruncation(t *testing.T) {
	root := privateRoot(t)
	outside := filepath.Join(t.TempDir(), "outside")
	const contents = "outside contents"
	if err := os.WriteFile(outside, []byte(contents), 0600); err != nil {
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
	change := []Change{{Path: "file", Mode: Regular, Data: []byte("updated")}}
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
			t.Fatalf("hardlinked identity or mode changed: %v, %v", info, err)
		}
	}
	if err := os.Remove(outside); err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, change); err != nil {
		t.Fatalf("single-link update was refused: %v", err)
	}
	if data, err := os.ReadFile(name); err != nil || string(data) != "updated" {
		t.Fatalf("single-link update=%q, %v", data, err)
	}
}

func TestWindowsApplyRefusesJunctionParent(t *testing.T) {
	root, outside := privateRoot(t), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "existing"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	windowsJunction(t, filepath.Join(root, "parent"), outside)
	for _, name := range []string{"existing", "new"} {
		if err := Apply(root, []Change{{Path: "parent/" + name, Mode: Regular, Data: []byte("changed")}}); err == nil {
			t.Fatalf("junction parent accepted for %s", name)
		}
	}
	if data, err := os.ReadFile(filepath.Join(outside, "existing")); err != nil || string(data) != "outside" {
		t.Fatalf("outside existing file changed: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatalf("outside new file created: %v", err)
	}
}

type windowsChangedInfo struct {
	os.FileInfo
	size     int64
	mode     fs.FileMode
	modified time.Time
}

func (info windowsChangedInfo) Size() int64        { return info.size }
func (info windowsChangedInfo) Mode() fs.FileMode  { return info.mode }
func (info windowsChangedInfo) ModTime() time.Time { return info.modified }

func TestWindowsReadStabilityRejectsChangedMetadata(t *testing.T) {
	name := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(name, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := openPath(name, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	before := windowsChangedInfo{FileInfo: info, size: info.Size(), mode: info.Mode(), modified: info.ModTime()}
	for _, test := range []struct {
		name   string
		change func(*windowsChangedInfo)
	}{
		{"size", func(after *windowsChangedInfo) { after.size++ }},
		{"mode", func(after *windowsChangedInfo) { after.mode &^= 0222 }},
		{"mtime", func(after *windowsChangedInfo) { after.modified = after.modified.Add(time.Second) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			after := before
			test.change(&after)
			if err := checkReadStability(before, after, int(before.Size()), file, true); err == nil || !strings.Contains(err.Error(), "file changed") {
				t.Fatalf("changed metadata accepted: %v", err)
			}
		})
	}
	t.Run("change time", func(t *testing.T) {
		beforeChange, err := changeTime(before, file)
		if err != nil {
			t.Fatal(err)
		}
		anchor, err := os.OpenRoot(filepath.Dir(name))
		if err != nil {
			t.Fatal(err)
		}
		defer anchor.Close()
		var after os.FileInfo
		var afterChange [2]int64
		deadline := time.Now().Add(time.Second)
		for {
			// Root.Chtimes opens a write-attributes handle sharing reads, writes, and deletes.
			if err := anchor.Chtimes(filepath.Base(name), time.Time{}, before.ModTime().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := anchor.Chtimes(filepath.Base(name), time.Time{}, before.ModTime()); err != nil {
				t.Fatal(err)
			}
			after, err = file.Stat()
			if err != nil || !after.ModTime().Equal(before.ModTime()) {
				t.Fatalf("mtime-restored fixture changed file: %v, %v", after, err)
			}
			afterChange, err = changeTime(after, file)
			if err != nil {
				t.Fatal(err)
			}
			if beforeChange != afterChange {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Windows change time did not observe metadata update: before=%v after=%v", beforeChange, afterChange)
			}
			time.Sleep(time.Millisecond)
		}
		if err := checkReadStability(before, after, int(before.Size()), file, beforeChange == afterChange); err == nil || !strings.Contains(err.Error(), "file changed") {
			t.Fatalf("changed file time accepted: %v", err)
		}
	})
	t.Run("hard link count", func(t *testing.T) {
		alias := filepath.Join(filepath.Dir(name), "alias")
		if err := os.Link(name, alias); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(alias)
		if err := checkReadStability(before, before, int(before.Size()), file, true); err == nil || !strings.Contains(err.Error(), "hard links changed") {
			t.Fatalf("new hard link was not detected: %v", err)
		}
	})
}
