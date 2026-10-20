//go:build linux

package processgroup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLeaderExitBoundsPipeDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	fixtureDir := t.TempDir()
	stopFile := filepath.Join(fixtureDir, "stop")
	t.Cleanup(func() {
		if err := os.WriteFile(stopFile, nil, 0600); err != nil {
			t.Error(err)
		}
	})
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "( i=0; while [ -d \"$2\" ] && [ \"$i\" -lt 30 ] && [ ! -f \"$1\" ]; do sleep 1; i=$((i+1)); done ) &\nexit 0\n", "sh", stopFile, fixtureDir)
	Configure(cmd)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := cmd.Wait()
	if !errors.Is(err, exec.ErrWaitDelay) || ctx.Err() != nil || time.Since(started) > 5*time.Second {
		t.Fatalf("pipe drain elapsed=%v err=%v parent=%v", time.Since(started), err, ctx.Err())
	}
}
