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
	"time"
)

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

// Process pins one process created by a test-owned fixture.
// Callers retain their deadlines and native-result assertions.
type Process struct {
	process *os.Process
	stat    *os.File
}

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
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil, err
	}
	stat, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			err = fmt.Errorf("fixture child %d: %w", pid, os.ErrProcessDone)
		}
		return nil, errors.Join(err, process.Release())
	}
	return &Process{process: process, stat: stat}, nil
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

// Cleanup kills the pinned fixture, observes its exit and releases both handles.
func (p *Process) Cleanup(ctx context.Context) error {
	// Never signal a historical process group or look up the PID again.
	err := p.process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		err = nil
	}
	return errors.Join(err, p.WaitExited(ctx), p.Close())
}

func (p *Process) Close() error { return errors.Join(p.process.Release(), p.stat.Close()) }
