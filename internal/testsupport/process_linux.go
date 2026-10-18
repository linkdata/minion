//go:build linux

package testsupport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
)

// WaitStopped observes a pinned proc stat file without reopening, signalling or closing it.
func WaitStopped(ctx context.Context, pinned *os.File) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	buffer := make([]byte, 4096)
	for {
		n, err := pinned.ReadAt(buffer, 0)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read pinned process state: %w", err)
		}
		_, state, ok := strings.Cut(string(buffer[:n]), ") ")
		if !ok {
			return fmt.Errorf("invalid pinned process state %q", buffer[:n])
		}
		if strings.HasPrefix(state, "Z ") {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
