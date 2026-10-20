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
		for _, dir := range []string{".", "untracked", "ignored-tracked"} {
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
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := index.Changes(ctx)
	if err == nil || !strings.Contains(err.Error(), `untracked nested repository output "newdir/"`) || ctx.Err() != nil {
		t.Fatalf("nested Git FIFO refused only after blocking: %v; deadline=%v", err, ctx.Err())
	}
}

func TestFixIndexRejectsIgnoredTrackedDirectoryWithGitMetadata(t *testing.T) {
	for _, kind := range []string{"directory with FIFO HEAD", "gitfile"} {
		t.Run(kind, func(t *testing.T) {
			index, source := offlineIndexFixture(t, []workspace.File{
				{Path: ".gitignore", Mode: workspace.Regular, Data: []byte("*.log\n")},
				{Path: "tracked.log", Mode: workspace.Regular, Data: []byte("before")},
			})
			name := filepath.Join(source, "tracked.log")
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(name, 0700); err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(name, ".git")
			if kind == "gitfile" {
				if err := os.WriteFile(metadata, []byte("gitdir: /tmp/untrusted\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(metadata, 0700); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(filepath.Join(metadata, "HEAD"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(t.TempDir(), "add-ran")
			index.binary = filepath.Join(fakeOnPath(t, "git-proxy", "#!/bin/sh\nfor argument do\n if [ \"$argument\" = add ]; then touch "+shellQuote(marker)+"; exit 1; fi\ndone\nexec "+shellQuote(index.binary)+" \"$@\"\n"), "git-proxy")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := index.Changes(ctx)
			if err == nil || !strings.Contains(err.Error(), `tracked fix path "tracked.log" contains nested Git metadata`) || ctx.Err() != nil {
				t.Fatalf("ignored tracked directory reached Git metadata: %v; deadline=%v", err, ctx.Err())
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Git add ran before metadata refusal: %v", err)
			}
		})
	}
}

func TestFixIndexSkipsPopulatedSourceGitMetadata(t *testing.T) {
	files := []workspace.File{{Path: "source", Mode: workspace.Regular, Data: []byte("before")}}
	source := repositorySource(t, files)
	metadata := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(source, ".git", "HEAD"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	index, err := NewOfflineIndex(ctx, metadata, source, files)
	if err != nil || ctx.Err() != nil {
		t.Fatalf("source Git metadata affected index creation: %v; deadline=%v", err, ctx.Err())
	}
	writeIndexFixture(t, source, "source", []byte("after"))
	changes, err := index.Changes(ctx)
	if err != nil || len(changes) != 1 || changes[0].Path != "source" || string(changes[0].Data) != "after" || ctx.Err() != nil {
		t.Fatalf("source Git metadata affected selected contents: %+v, %v; deadline=%v", changes, err, ctx.Err())
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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
		for _, child := range []string{"objects", "refs"} {
			if err := os.MkdirAll(filepath.Join(source, dir, ".git", child), 0700); err != nil {
				t.Fatal(err)
			}
		}
		for _, control := range []string{".gitignore", ".gitattributes", ".git/HEAD"} {
			if err := syscall.Mkfifo(filepath.Join(source, dir, control), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeIndexFixture(t, source, "source", []byte("after"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			changes, err := index.Changes(ctx)
			want := "can only add regular files"
			if kind == "symlink" {
				want = `unsupported Git index change at "file"`
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

func TestFixIndexPrunesCaseVariantMetadataBeforeOpeningFIFOs(t *testing.T) {
	for _, name := range []string{".GIT/.gitignore", "sub/.GIT/.git/HEAD"} {
		t.Run(name, func(t *testing.T) {
			index, source := offlineIndexFixture(t, nil)
			file := filepath.Join(source, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(file, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			changes, err := index.Changes(ctx)
			if err != nil || len(changes) != 0 || ctx.Err() != nil {
				t.Fatalf("excluded metadata FIFO affected Git: %+v,%v; deadline=%v", changes, err, ctx.Err())
			}
		})
	}
}

func TestFixIndexIgnoredSymlinkParentDoesNotBlockOnAttributeFIFO(t *testing.T) {
	for _, test := range []struct {
		name, ignore, tracked, target, link string
	}{
		{"nested", "build/\n", "build/pkg/file", "build/cache", "cache"},
		{"top level", "parent\ncache/\n", "parent/file", "cache", "cache"},
	} {
		t.Run(test.name, func(t *testing.T) {
			index, source := offlineIndexFixture(t, []workspace.File{
				{Path: ".gitignore", Mode: workspace.Regular, Data: []byte(test.ignore)},
				{Path: test.tracked, Mode: workspace.Regular, Data: []byte("before")},
			})
			if err := os.MkdirAll(filepath.Join(source, test.target), 0700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(source, test.target, ".gitattributes"), 0600); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(source, filepath.Dir(test.tracked))
			if err := os.RemoveAll(parent); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(test.link, parent); err != nil {
				t.Fatal(err)
			}
			if info, err := os.Lstat(filepath.Join(parent, ".gitattributes")); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
				t.Fatalf("symlink does not lead to attribute FIFO: %v, %v", info, err)
			}
			tracked, err := index.run(context.Background(), 1024, nil, "ls-tree", "-r", "--name-only", index.baseline, "--", test.tracked)
			if err != nil || string(tracked) != test.tracked+"\n" {
				t.Fatalf("baseline tracked path=%q error=%v", tracked, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			changes, err := index.Changes(ctx)
			if err != nil || len(changes) != 1 || changes[0].Path != test.tracked || !changes[0].Delete || ctx.Err() != nil {
				t.Fatalf("ignored symlink parent affected deletion: %+v, %v; deadline=%v", changes, err, ctx.Err())
			}
		})
	}
}

func TestFixIndexHandlesGitFIFOInsideTrackedDirectory(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: "tracked/file", Mode: workspace.Regular, Data: []byte("before")},
	})
	if err := syscall.Mkfifo(filepath.Join(source, "tracked", ".git"), 0600); err != nil {
		t.Fatal(err)
	}
	writeIndexFixture(t, source, "tracked/file", []byte("after"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	changes, err := index.Changes(ctx)
	if err != nil || ctx.Err() != nil || len(changes) != 1 || changes[0].Path != "tracked/file" || string(changes[0].Data) != "after" {
		t.Fatalf("tracked directory FIFO changed selection or blocked Git: %+v, %v; deadline=%v", changes, err, ctx.Err())
	}
}

func TestCaseOnlyRenamePassesChangesAndImport(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: "README.md", Mode: workspace.Regular, Data: []byte("fixture\n")},
	})
	if err := os.Rename(filepath.Join(source, "README.md"), filepath.Join(source, "readme.md")); err != nil {
		t.Fatal(err)
	}
	changes, err := index.Changes(context.Background())
	if err != nil || len(changes) != 2 {
		t.Fatalf("case-only rename changes=%+v error=%v", changes, err)
	}
	found := map[string]bool{}
	for _, change := range changes {
		switch change.Path {
		case "README.md":
			found[change.Path] = change.Delete
		case "readme.md":
			found[change.Path] = !change.Delete && change.Mode == workspace.Regular && string(change.Data) == "fixture\n"
		}
	}
	if !found["README.md"] || !found["readme.md"] {
		t.Fatalf("case-only rename lost deletion or creation: %+v", changes)
	}
	fixture := newFixture(t)
	if err := fixture.checkout.Import(context.Background(), fixture.base, changes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.checkout.path, "README.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old spelling remains: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(fixture.checkout.path, "readme.md")); err != nil || string(data) != "fixture\n" {
		t.Fatalf("new spelling data=%q error=%v", data, err)
	}
	if got := fixture.git(fixture.checkout.path, "diff", "--cached", "--name-status", "--no-renames"); got != "D\tREADME.md\nA\treadme.md" {
		t.Fatalf("case-only rename was not staged as exact paths: %q", got)
	}
}
