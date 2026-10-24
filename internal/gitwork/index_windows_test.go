package gitwork

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestFixIndexCaseOnlyRenameOnWindows(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "README.md", Mode: workspace.Regular, Data: []byte("fixture\n")}})
	config, err := index.run(t.Context(), 16, nil, "config", "--bool", "--default=false", "core.ignoreCase")
	if err != nil || strings.TrimSpace(string(config)) != "true" {
		t.Fatalf("Windows offline index must use core.ignoreCase=true: %q, %v", config, err)
	}
	intermediate := filepath.Join(source, "case-rename.tmp")
	if err := os.Rename(filepath.Join(source, "README.md"), intermediate); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(intermediate, filepath.Join(source, "readme.md")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil || len(entries) != 1 || entries[0].Name() != "readme.md" {
		t.Fatalf("case-only rename did not change disk spelling: %+v, %v", entries, err)
	}
	changes, err := index.Changes(t.Context())
	if err != nil || len(changes) != 2 || changes[0].Path != "README.md" || !changes[0].Delete || changes[1].Path != "readme.md" || changes[1].Delete || string(changes[1].Data) != "fixture\n" {
		t.Fatalf("Windows case-only rename changes=%+v error=%v", changes, err)
	}
}

func TestFixIndexPreservesExecutableCaseOnlyRenameOnWindows(t *testing.T) {
	index, source := offlineIndexFixture(t, []workspace.File{{Path: "TOOL.sh", Mode: workspace.Executable, Data: []byte("#!/bin/sh\n")}})
	intermediate := filepath.Join(source, "case-rename.tmp")
	if err := os.Rename(filepath.Join(source, "TOOL.sh"), intermediate); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(intermediate, filepath.Join(source, "tool.sh")); err != nil {
		t.Fatal(err)
	}
	changes, err := index.Changes(t.Context())
	if err != nil || len(changes) != 2 || changes[0].Path != "TOOL.sh" || !changes[0].Delete || changes[1].Path != "tool.sh" || changes[1].Delete || changes[1].Mode != workspace.Executable || string(changes[1].Data) != "#!/bin/sh\n" {
		t.Fatalf("Windows executable case-only rename changes=%+v error=%v", changes, err)
	}
}
