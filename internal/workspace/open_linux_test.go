//go:build linux

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func bumpTestChangeTime(info *fileInfoWithStat) {
	info.stat.Ctim.Sec++
}

func TestCheckRootRefusesFilesystemRoot(t *testing.T) {
	err := CheckRoot("/")
	var pathErr *os.PathError
	if err == nil || !strings.Contains(err.Error(), "filesystem root is not a workspace") || !errors.As(err, &pathErr) || pathErr.Path != "/" {
		t.Fatalf("filesystem root refusal=%v", err)
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
	if err := CheckUnchanged(t.Context(), root, base); err != nil {
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
	changes, err := ReadChanges(t.Context(), root, []Change{{Path: "changed", Mode: Executable}, {Path: "deleted", Delete: true}})
	if err != nil || len(changes) != 2 || changes[0].Mode != Executable || string(changes[0].Data) != string([]byte{0, 255}) {
		t.Fatalf("selected changes=%+v,%v", changes, err)
	}
	for _, test := range []struct {
		selected []Change
		want     string
	}{
		{[]Change{{Path: "ignored/secret", Mode: Regular}}, `changed path "ignored/secret"`},
		{[]Change{{Path: "../outside", Mode: Regular}}, "unsafe"},
		{[]Change{{Path: "changed", Mode: Regular}}, `selected change mode differs from file at "changed"`},
		{[]Change{{Path: "changed"}}, "invalid changed mode"},
		{[]Change{{Path: "changed", Delete: true, Data: []byte("invalid")}}, "has content or mode"},
	} {
		if _, err := ReadChanges(t.Context(), root, test.selected); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("selection %+v validation=%v want%q", test.selected, err, test.want)
		}
	}
}

func TestReadRegularChecksActualReadLength(t *testing.T) {
	name := filepath.Join("/proc", strconv.Itoa(os.Getpid()), "status")
	if _, err := ReadRegular(name, 1<<20); err == nil || !strings.Contains(err.Error(), "while reading") {
		t.Fatalf("accepted proc content exceeding its stat size: %v", err)
	}
}

func TestOpenPathErrorsIdentifyPathAndPreserveCause(t *testing.T) {
	name := filepath.Join(t.TempDir(), "missing", "source")
	err := CheckRoot(name)
	var pathErr *os.PathError
	if !errors.Is(err, os.ErrNotExist) || !errors.As(err, &pathErr) || pathErr.Op != "openat" || pathErr.Path != name {
		t.Fatalf("open error lost path or cause: %v", err)
	}
}
