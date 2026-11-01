package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckUnchangedWithProgressCreditsOnlyVerifiedFiles(t *testing.T) {
	root := t.TempDir()
	files := []File{{Path: "source.txt", Mode: Regular, Data: []byte("original")}}
	if err := MaterializeWithProgress(t.Context(), root, files, nil); err != nil {
		t.Fatal(err)
	}
	progress := 0
	if err := CheckUnchangedWithProgress(t.Context(), root, files, func() { progress++ }); err != nil || progress != 1 {
		t.Fatalf("unchanged source: progress=%d error=%v", progress, err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	progress = 0
	if err := CheckUnchangedWithProgress(t.Context(), root, files, func() { progress++ }); err == nil || progress != 0 {
		t.Fatalf("changed source: progress=%d error=%v", progress, err)
	}
}

func TestCheckUnchangedWithProgressCreditsDirectoryTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	progress := 0
	if err := CheckUnchangedWithProgress(t.Context(), root, nil, func() { progress++ }); err != nil || progress != 1 {
		t.Fatalf("directory traversal: progress=%d error=%v", progress, err)
	}
}

func TestCheckChangesUnchangedWithProgressExcludesMetadataAndStopsOnCancellation(t *testing.T) {
	root := t.TempDir()
	base := []File{{Path: "source.txt", Mode: Regular, Data: []byte("original")}}
	changes := []Change{{Path: "added.txt", Mode: Regular, Data: []byte("new")}}
	if err := MaterializeChangesWithProgress(t.Context(), root, base, changes, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	progress := 0
	if err := CheckChangesUnchangedWithProgress(t.Context(), root, base, changes, func() { progress++ }); err != nil || progress != 2 {
		t.Fatalf("frozen candidate: progress=%d error=%v", progress, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	progress = 0
	err := CheckChangesUnchangedWithProgress(ctx, root, base, changes, func() {
		progress++
		cancel()
	})
	if !errors.Is(err, context.Canceled) || progress != 1 {
		t.Fatalf("canceled verification: progress=%d error=%v", progress, err)
	}
}
