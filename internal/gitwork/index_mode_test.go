package gitwork

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/workspace"
)

func TestOfflineIndexPreservesExecutableSourceMode(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is unavailable")
	}
	root := t.TempDir()
	source, metadata := filepath.Join(root, "source"), filepath.Join(root, "index")
	for _, dir := range []string{source, metadata} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := []workspace.File{{Path: "tool.sh", Mode: workspace.Executable, Data: []byte("#!/bin/sh\nexit 0\n")}}
	if err := workspace.Materialize(source, files); err != nil {
		t.Fatal(err)
	}
	index, err := NewOfflineIndex(t.Context(), metadata, source, files)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := index.run(t.Context(), 256, nil, "ls-tree", index.baseline, "--", "tool.sh")
	if err != nil || !strings.HasPrefix(string(tree), "100755 blob ") {
		t.Fatalf("executable source mode was lost: tree=%q error=%v", tree, err)
	}
}
