package gitwork

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
)

func TestOfflineGitProgressReportsStartedAndAcceptedCommands(t *testing.T) {
	binary, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	progress := 0
	index := &OfflineIndex{dir: t.TempDir(), source: t.TempDir(), binary: binary,
		progress: func() { progress++ }}
	if _, err := index.run(t.Context(), 1024, nil, "init", "--quiet", "--template=", "--object-format=sha1", "--ref-format=files"); err != nil || progress != 2 {
		t.Fatalf("Git init progress=%d error=%v", progress, err)
	}
	progress = 0
	for range 2 {
		if _, err := index.run(t.Context(), 1024, nil, "status", "--short"); err != nil {
			t.Fatal(err)
		}
	}
	if progress != 4 {
		t.Fatalf("two successful Git commands credited %d progress events, want 4", progress)
	}
	progress = 0
	if _, err := index.run(t.Context(), 1024, nil, "check-ignore", "unignored"); err != nil || progress != 2 {
		t.Fatalf("accepted check-ignore miss: progress=%d error=%v", progress, err)
	}
	progress = 0
	if _, err := index.run(t.Context(), 1024, nil, "not-a-git-command"); err == nil || progress != 1 {
		t.Fatalf("failed Git command: progress=%d error=%v", progress, err)
	}
	progress = 0
	if _, err := index.run(t.Context(), 1, nil, "--version"); !errors.Is(err, ErrCheckoutTooLarge) || progress != 1 {
		t.Fatalf("output-limited Git command: progress=%d error=%v", progress, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	index.progress = func() { progress++; cancel() }
	progress = 0
	if _, err := index.run(ctx, 1024, nil, "status", "--short"); !errors.Is(err, context.Canceled) || progress != 1 {
		t.Fatalf("canceled Git command: progress=%d error=%v", progress, err)
	}
	beforeStart, cancelBeforeStart := context.WithCancel(t.Context())
	cancelBeforeStart()
	progress = 0
	if _, err := index.run(beforeStart, 1024, nil, "status", "--short"); !errors.Is(err, context.Canceled) || progress != 0 {
		t.Fatalf("pre-canceled Git command: progress=%d error=%v", progress, err)
	}
	index.binary = filepath.Join(t.TempDir(), "missing-git")
	progress = 0
	if _, err := index.run(t.Context(), 1024, nil, "status", "--short"); err == nil || progress != 0 {
		t.Fatalf("failed Git launch: progress=%d error=%v", progress, err)
	}
}

func TestOfflineIndexKeepsProgressAcrossChangeCaptures(t *testing.T) {
	progress := 0
	index, err := NewOfflineIndexWithProgress(t.Context(), t.TempDir(), t.TempDir(), nil, func() { progress++ })
	if err != nil || progress < 4 {
		t.Fatalf("index preparation progress=%d error=%v", progress, err)
	}
	prepared := progress
	if changes, err := index.Changes(t.Context()); err != nil || len(changes) != 0 || progress <= prepared+2 {
		t.Fatalf("change capture progress=%d prepared=%d changes=%v error=%v", progress, prepared, changes, err)
	}
}

func TestOfflineIndexCreditsFilesystemScansBeforeGit(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	events := 0
	index := &OfflineIndex{source: source, tracked: map[string]int64{"tracked.txt": 1},
		progress: func() { events++ }}
	if err := index.checkControlFilesWithLimit(t.Context(), 2); err != nil || events != 1 {
		t.Fatalf("control-file scan: events=%d error=%v", events, err)
	}
	events = 0
	if stale, err := index.caseRenamedPaths(t.Context(), 2); err != nil || len(stale) != 0 || events != 1 {
		t.Fatalf("case scan: events=%d stale=%q error=%v", events, stale, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events = 0
	index.progress = func() {
		events++
		if events == 2 {
			cancel()
		}
	}
	if _, err := index.Changes(ctx); !errors.Is(err, context.Canceled) || events != 2 {
		t.Fatalf("tracked scan before Git: events=%d error=%v", events, err)
	}
}
