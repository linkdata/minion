package action

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

func offlineIndexFixture(t *testing.T, files []workspace.File) (*fixIndex, string) {
	t.Helper()
	root := t.TempDir()
	source, err := os.MkdirTemp(root, "source-")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.MkdirTemp(root, "index-")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.Materialize(source, files); err != nil {
		t.Fatal(err)
	}
	index, err := newFixIndex(context.Background(), metadata, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(source, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source received Git metadata: %v", err)
	}
	return index, source
}

func writeIndexFixture(t *testing.T, source, name string, data []byte) {
	t.Helper()
	path := filepath.Join(source, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFixIndexPreservesGitignoreAndTrackedIgnoredFiles(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: ".gitignore", Mode: workspace.Regular, Data: []byte(".venv/\ntarget/\n__pycache__/\n*.log\ntracked.log\n")},
		{Path: "source.txt", Mode: workspace.Regular, Data: []byte("source")},
		{Path: "tracked.log", Mode: workspace.Regular, Data: []byte("before")},
	})
	writeIndexFixture(t, source, "tracked.log", []byte("after"))
	writeIndexFixture(t, source, "runtime.log", make([]byte, workspace.MaxChangeBytes+1))
	writeIndexFixture(t, source, "target/cache/.git/config", []byte("untrusted nested metadata"))
	for i := 0; i < workspace.MaxChanges+10; i++ {
		writeIndexFixture(t, source, fmt.Sprintf("__pycache__/%d.pyc", i), []byte("cache"))
	}
	if err := os.MkdirAll(filepath.Join(source, ".venv", "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/outside/python", filepath.Join(source, ".venv", "bin", "python")); err != nil {
		t.Fatal(err)
	}
	changes, err := index.changes(context.Background())
	want := []workspace.Change{{Path: "tracked.log", Mode: workspace.Regular, Data: []byte("after")}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %+v, %v", changes, err)
	}
	writeIndexFixture(t, source, "tracked.log", []byte("before"))
	changes, err = index.changes(context.Background())
	if err != nil || len(changes) != 0 {
		t.Fatalf("ignored-only output changed result: %+v, %v", changes, err)
	}
}

func TestFixIndexDisablesAttributesAndOperatorConfiguration(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[filter \"escape\"]\n clean = touch "+marker+"\n required = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "filter.escape.clean")
	t.Setenv("GIT_CONFIG_VALUE_0", "touch "+marker)
	t.Setenv("GH_TOKEN", "must-not-be-inherited")
	index, source := offlineIndexFixture(t, []workspace.File{
		{Path: ".gitattributes", Mode: workspace.Regular, Data: []byte("* filter=escape diff=escape working-tree-encoding=UTF-16\n")},
		{Path: "source.txt", Mode: workspace.Regular, Data: []byte("unencoded text")},
	})
	writeIndexFixture(t, source, "source.txt", []byte("changed unencoded text"))
	changes, err := index.changes(context.Background())
	if err != nil || len(changes) != 1 || string(changes[0].Data) != "changed unencoded text" {
		t.Fatalf("attributes changed result: %+v, %v", changes, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository filter ran: %v", err)
	}
	for _, entry := range index.environment() {
		if strings.HasPrefix(entry, "GH_TOKEN=") || strings.Contains(entry, global) || strings.HasPrefix(entry, "GIT_CONFIG_COUNT=") {
			t.Fatalf("unsafe offline Git environment: %q", entry)
		}
	}
}

func TestFixIndexRejectsNonIgnoredUnsafeOutput(t *testing.T) {
	for _, kind := range []string{"symlink", "nested repository", "oversized", "too many"} {
		t.Run(kind, func(t *testing.T) {
			index, source := offlineIndexFixture(t, nil)
			switch kind {
			case "symlink":
				if err := os.Symlink("/outside/secret", filepath.Join(source, "link")); err != nil {
					t.Fatal(err)
				}
			case "nested repository":
				writeIndexFixture(t, source, "nested/file", []byte("nested source"))
				nested := filepath.Join(source, "nested")
				metadata := filepath.Join(nested, ".git")
				if err := os.Mkdir(metadata, 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := newFixIndex(context.Background(), metadata, nested); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				writeIndexFixture(t, source, "huge", make([]byte, workspace.MaxChangeBytes+1))
			case "too many":
				for i := 0; i <= workspace.MaxChanges; i++ {
					writeIndexFixture(t, source, fmt.Sprintf("file-%d", i), nil)
				}
			}
			if changes, err := index.changes(context.Background()); err == nil {
				t.Fatalf("accepted %s output: %+v", kind, changes)
			}
		})
	}
}

func TestFixIndexPreservesModeDeletionAndBinary(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "mode", Mode: workspace.Regular, Data: []byte("same")}, {Path: "deleted", Mode: workspace.Regular, Data: []byte("deleted")}})
	if err := os.Chmod(filepath.Join(source, "mode"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "deleted")); err != nil {
		t.Fatal(err)
	}
	writeIndexFixture(t, source, "binary", []byte{0, 1, 2})
	changes, err := index.changes(context.Background())
	want := []workspace.Change{{Path: "binary", Mode: workspace.Regular, Data: []byte{0, 1, 2}}, {Path: "deleted", Delete: true}, {Path: "mode", Mode: workspace.Executable, Data: []byte("same")}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %+v, %v", changes, err)
	}
}

func TestFixIndexBoundsBinaryDiffAndCancellation(t *testing.T) {
	index, source := offlineIndexFixture(t, nil)
	data := make([]byte, 1000<<10)
	_, _ = rand.New(rand.NewSource(1)).Read(data) // Deterministic incompressible binary patch.
	writeIndexFixture(t, source, "binary", data)
	if _, err := index.changes(context.Background()); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized binary patch = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := index.changes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled index = %v", err)
	}
}

func TestFixIndexMustRemainOutsideModelGrants(t *testing.T) {
	root := t.TempDir()
	metadata := filepath.Join(root, "index")
	runner := Runner{
		Main:  "codex",
		Codex: config.CodexConfig{ReadOnlyDirs: []string{root}},
	}
	if err := runner.checkIndexIsolation(metadata); err == nil {
		t.Fatal("allowed index inside configured model read grant")
	}
}
