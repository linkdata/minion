package executil

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

type readyWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *readyWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return len(p), nil
}

func TestRunCancellationStopsDirectCommand(t *testing.T) {
	if os.Getenv("MINION_EXECUTIL_TEST_CHILD") == "1" {
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunCancellationStopsDirectCommand$")
	cmd.Env = append(os.Environ(), "MINION_EXECUTIL_TEST_CHILD=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	ready := &readyWriter{ready: make(chan struct{})}
	cmd.Stdout = ready
	done := make(chan error, 1)
	go func() { done <- Run(cmd) }()
	guard := time.NewTimer(10 * time.Second)
	defer guard.Stop()
	select {
	case <-ready.ready:
	case err := <-done:
		t.Fatalf("direct command exited before cancellation: %v", err)
	case <-guard.C:
		cancel()
		t.Fatal("direct command did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || cmd.ProcessState == nil {
			t.Fatalf("canceled direct command did not exit with an error: %v", err)
		}
	case <-guard.C:
		t.Fatal("canceled direct command did not exit promptly")
	}
}
