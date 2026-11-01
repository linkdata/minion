package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWalkStopsBeforeNextFileAfterCancellation(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	visited := 0
	err := walkWithLimit(ctx, root, false, 2, func(string, os.DirEntry) error {
		visited++
		if visited == 1 {
			cancel()
			return nil
		}
		return errors.New("later file failed verification")
	})
	if !errors.Is(err, context.Canceled) || visited != 1 {
		t.Fatalf("canceled walk: visited=%d error=%v", visited, err)
	}
}
