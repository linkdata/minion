//go:build linux

package testsupport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Process pins one process created by a test-owned fixture.
// Callers retain their deadlines and native-result assertions.
type Process struct {
	stat     *os.File
	stopFile string
}

// WaitExited observes the pinned proc stat file without reopening, signalling or closing it.
func (p *Process) WaitExited(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	buffer := make([]byte, 4096)
	for {
		n, err := p.stat.ReadAt(buffer, 0)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read pinned process state: %w", err)
		}
		text := string(buffer[:n])
		end := strings.LastIndex(text, ") ")
		if end < 0 {
			return fmt.Errorf("invalid pinned process state %q", buffer[:n])
		}
		if strings.HasPrefix(text[end+2:], "Z ") {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// PinProcess pins a fixture child identified by a published PID file.
// shortcut: a recycled PID can bind observation to another process; use a birth identity handshake if assertions become flaky.
func PinProcess(pidFile string) (*Process, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return nil, err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return nil, os.ErrNotExist
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return nil, fmt.Errorf("invalid fixture child PID %q", data)
	}
	stat, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			err = fmt.Errorf("fixture child %d: %w", pid, os.ErrProcessDone)
		}
		return nil, err
	}
	return &Process{stat: stat, stopFile: pidFile + ".stop"}, nil
}

// WaitForProcess pins a fixture child before its run completes or startup expires.
func WaitForProcess[T any](ctx context.Context, pidFile string, done <-chan T) (*Process, error) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			child, err := PinProcess(pidFile)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return child, err
		case result := <-done:
			return nil, fmt.Errorf("run exited before fixture child was pinned: %+v", result)
		case <-ctx.Done():
			return nil, fmt.Errorf("fixture child did not start: %w", ctx.Err())
		}
	}
}

// StopOnCleanup releases a finite fixture even when its test fails before pinning it.
func StopOnCleanup(t testing.TB, stopFile string) {
	t.Helper()
	t.Cleanup(func() {
		if err := os.WriteFile(stopFile, nil, 0600); err != nil {
			t.Errorf("stop fixture: %v", err)
		}
	})
}

// Cleanup asks the fixture to stop, observes its exit and closes the stat file.
func (p *Process) Cleanup(ctx context.Context) error {
	err := os.WriteFile(p.stopFile, nil, 0600)
	return errors.Join(err, p.WaitExited(ctx), p.Close())
}

func (p *Process) Close() error { return p.stat.Close() }
