package action

import (
	"context"
	"errors"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/workspace"
	"os"
	"path/filepath"
	"testing"
)

func offlineIndexFixture(t *testing.T, files []workspace.File) (*gitwork.OfflineIndex, string) {
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
	index, err := gitwork.NewOfflineIndex(context.Background(), metadata, source, files)
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
