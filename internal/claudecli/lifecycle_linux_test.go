//go:build linux

package claudecli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"golang.org/x/sys/unix"
)

func TestSessionCreditsProcessStartAndExit(t *testing.T) {
	binary := fakeExecutable(t, "claude", "IFS= read -r release < \"$0.gate\"\nexit 7\n")
	if err := unix.Mkfifo(binary+".gate", 0600); err != nil {
		t.Fatal(err)
	}
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	prepared := make(chan struct{}, 1)
	progress := make(chan struct{}, 4)
	session.OnInput = func(_ aicli.PreparedInput) { prepared <- struct{}{} }
	session.OnProgress = func() { progress <- struct{}{} }
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	workspace := t.TempDir()
	go func() {
		_, err := session.Run(ctx, workspace, "task", false, "", config.ClaudeConfig{})
		done <- err
	}()
	select {
	case <-prepared:
	case err := <-done:
		t.Fatalf("process ended before prepared input: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-progress: // Process started and remains blocked on the FIFO.
	case err := <-done:
		t.Fatalf("process ended before start progress: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-progress:
		t.Fatal("process exit credited before FIFO release")
	case err := <-done:
		t.Fatalf("process exited before FIFO release: %v", err)
	default:
	}
	gate, err := os.OpenFile(binary+".gate", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	if _, err := gate.Write([]byte("release\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("nonzero CLI exit succeeded")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-progress:
	default:
		t.Fatal("process exit did not credit progress")
	}
	select {
	case <-progress:
		t.Fatal("silent CLI credited extra progress")
	default:
	}
}
