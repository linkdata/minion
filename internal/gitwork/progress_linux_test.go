package gitwork

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOfflineGitProgressStartPrecedesHeldChildExit(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	script := "#!/bin/sh\nwhile [ ! -f " + shellQuote(release) + " ]; do sleep 0.01; done\n"
	index := &OfflineIndex{dir: t.TempDir(), source: t.TempDir(),
		binary: filepath.Join(fakeOnPath(t, "git-progress", script), "git-progress")}
	progress := make(chan struct{}, 2)
	index.progress = func() { progress <- struct{}{} }
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := index.run(ctx, 1024, nil, "status")
		done <- err
	}()
	select {
	case <-progress: // The child started; its exit remains gated.
	case err := <-done:
		t.Fatalf("Git returned before start progress: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-progress:
		t.Fatal("Git reported finish before child exit")
	case err := <-done:
		t.Fatalf("Git returned before release: %v", err)
	default:
	}
	if err := os.WriteFile(release, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-progress:
	default:
		t.Fatal("Git returned without finish progress")
	}
}
