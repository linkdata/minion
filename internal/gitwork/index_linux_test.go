//go:build linux

package gitwork

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/workspace"
)

func TestFixIndexRejectsControlFIFOsBeforeGitOpensThem(t *testing.T) {
	for _, control := range []string{".gitignore", ".gitattributes"} {
		for _, dir := range []string{".", "tracked", "untracked", "ignored-tracked"} {
			t.Run(dir+"/"+control, func(t *testing.T) {
				index, source := offlineIndexFixture(t, []workspace.File{
					{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("ignored-tracked/\n")},
					{Path: "tracked/file", Mode: workspace.Regular},
					{Path: "ignored-tracked/file", Mode: workspace.Regular},
				})
				name := filepath.Join(source, dir, control)
				if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(name, 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, err := index.Changes(ctx)
				if err == nil || !strings.Contains(err.Error(), "fix control file") || !strings.Contains(err.Error(), control) || !strings.Contains(err.Error(), "must be regular") || ctx.Err() != nil {
					t.Fatalf("FIFO refused only after Git blocked: %v; deadline=%v", err, ctx.Err())
				}
			})
		}
	}
}

func TestFixIndexRejectsNestedGitFIFOBeforeGitOpensIt(t *testing.T) {
	index, source := offlineIndexFixture(t, nil)
	metadata := filepath.Join(source, "newdir", ".git")
	for _, dir := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(metadata, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(metadata, "HEAD"), 0600); err != nil {
		t.Fatal(err)
	}
	writeIndexFixture(t, source, "newdir/file", []byte("new output"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := index.Changes(ctx)
	if err == nil || !strings.Contains(err.Error(), `untracked nested repository output "newdir/"`) || ctx.Err() != nil {
		t.Fatalf("nested Git FIFO refused only after blocking: %v; deadline=%v", err, ctx.Err())
	}
}

func TestFixIndexTreatsColonDirectoriesAsLiteralNames(t *testing.T) {
	for _, dir := range []string{":build", ":(top)build", ":!build", ":(icase)build"} {
		t.Run(dir, func(t *testing.T) {
			index, source := offlineIndexFixture(t, []workspace.File{{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("build/\n")}})
			if err := os.Mkdir(filepath.Join(source, dir), 0700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(source, dir, ".gitignore"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := index.Changes(ctx)
			if err == nil || !strings.Contains(err.Error(), "fix control file") || !strings.Contains(err.Error(), "must be regular") || ctx.Err() != nil {
				t.Fatalf("literal directory path was interpreted as a pathspec: %v; deadline=%v", err, ctx.Err())
			}
			if err := os.Remove(filepath.Join(source, dir, ".gitignore")); err != nil {
				t.Fatal(err)
			}
			writeIndexFixture(t, source, dir+"/file", []byte("legal source"))
			changes, err := index.Changes(ctx)
			if err != nil || len(changes) != 1 || changes[0].Path != dir+"/file" {
				t.Fatalf("legal colon directory was refused: %+v,%v", changes, err)
			}
		})
	}
}

func TestFixIndexDoesNotInspectIgnoredControlFiles(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: ".gitignore", Mode: workspace.Regular, Data: []byte(".venv/\ntarget/\n")},
		{Path: "source", Mode: workspace.Regular, Data: []byte("before")},
	})
	for _, dir := range []string{".venv", "target"} {
		if err := os.Mkdir(filepath.Join(source, dir), 0700); err != nil {
			t.Fatal(err)
		}
		for _, control := range []string{".gitignore", ".gitattributes"} {
			if err := syscall.Mkfifo(filepath.Join(source, dir, control), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("/outside/virtualenv", filepath.Join(source, dir, "packages")); err != nil {
			t.Fatal(err)
		}
	}
	writeIndexFixture(t, source, "source", []byte("after"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	changes, err := index.Changes(ctx)
	if err != nil || len(changes) != 1 || changes[0].Path != "source" || string(changes[0].Data) != "after" || ctx.Err() != nil {
		t.Fatalf("ignored build control files affected selection: %+v, %v; deadline=%v", changes, err, ctx.Err())
	}
}

func TestFixIndexRejectsTrackedFIFOAndSymlink(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			index, source := offlineIndexFixture(t, []workspace.File{{Path: "file", Mode: workspace.Regular, Data: []byte("before")}})
			name := filepath.Join(source, "file")
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "fifo" {
				err = syscall.Mkfifo(name, 0600)
			} else {
				err = os.Symlink("/outside/source", name)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			changes, err := index.Changes(ctx)
			want := "can only add regular files"
			if kind == "symlink" {
				want = `unsupported offline git change at "file"`
			}
			if err == nil || !strings.Contains(err.Error(), want) || len(changes) != 0 || ctx.Err() != nil {
				t.Fatalf("tracked %s returned %+v, %v; deadline=%v", kind, changes, err, ctx.Err())
			}
		})
	}
}

func TestFixIndexControlScanHonorsNestedIgnoreNegation(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: "sub/.gitignore", Mode: workspace.Regular, Data: []byte("*.cache/\n!keep.cache/\n")},
	})
	if err := os.MkdirAll(filepath.Join(source, "sub", "ignored.cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(source, "sub", "ignored.cache", ".gitattributes"), 0600); err != nil {
		t.Fatal(err)
	}
	writeIndexFixture(t, source, "sub/keep.cache/added", []byte("included by negation"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	changes, err := index.Changes(ctx)
	if err != nil || len(changes) != 1 || changes[0].Path != "sub/keep.cache/added" || ctx.Err() != nil {
		t.Fatalf("nested ignore negation changed selection: %+v, %v; deadline=%v", changes, err, ctx.Err())
	}
}

func TestFixIndexGrowthCheckDoesNotStatThroughSymlinkedParents(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		name := "unignored"
		ignore := ""
		if ignored {
			name, ignore = "ignored", "parent\n"
		}
		t.Run(name, func(t *testing.T) {
			index, source := offlineIndexFixture(t, []workspace.File{
				{Path: ".gitignore", Mode: workspace.Regular, Data: []byte(ignore)},
				{Path: "parent/file", Mode: workspace.Regular, Data: []byte("before")},
			})
			outside := t.TempDir()
			outsideFile := filepath.Join(outside, "file")
			if err := os.WriteFile(outsideFile, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(outsideFile, 1<<40); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(source, "parent")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(source, "parent")); err != nil {
				t.Fatal(err)
			}
			changes, err := index.Changes(context.Background())
			if ignored {
				if err != nil || len(changes) != 1 || changes[0].Path != "parent/file" || !changes[0].Delete {
					t.Fatalf("ignored replacement did not export only deletion: %+v, %v", changes, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), `fix output "parent"`) || strings.Contains(err.Error(), "tracked fix path") {
				t.Fatalf("symlink was accepted or outside target reached growth check: %+v, %v", changes, err)
			}
			info, statErr := os.Stat(outsideFile)
			if statErr != nil || info.Size() != 1<<40 {
				t.Fatalf("outside target changed: %v, %v", info, statErr)
			}
		})
	}
}
