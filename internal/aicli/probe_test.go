package aicli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func probeExecutable(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbeRetainsOutputLimitAndQuotesDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		limited      bool
	}{
		{"output cap", fmt.Sprintf("head -c %d /dev/zero\nsleep 30\n", MaxEventBytes+1), true},
		{"diagnostic", "printf 'first\\nforged journal line' >&2\nexit 7\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := probeExecutable(t, tc.script)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := Probe(ctx, binary, "", nil, t.TempDir(), os.Environ(), "")
			if tc.limited {
				if !errors.Is(err, ErrOutputLimit) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("limit cause lost: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), `first\nforged journal line`) || strings.Contains(err.Error(), "\nforged journal line") {
				t.Fatalf("unquoted diagnostic: %v", err)
			}
		})
	}
}

func TestProbeExactBoundAndCallerCancellation(t *testing.T) {
	binary := probeExecutable(t, fmt.Sprintf("head -c %d /dev/zero\n", MaxEventBytes))
	output, err := Probe(context.Background(), binary, "", nil, t.TempDir(), os.Environ(), "")
	if err != nil || len(output) != MaxEventBytes {
		t.Fatalf("exact bound bytes=%d error=%v", len(output), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Probe(ctx, binary, "", nil, t.TempDir(), os.Environ(), ""); !errors.Is(err, context.Canceled) || errors.Is(err, ErrOutputLimit) {
		t.Fatalf("caller cancellation cause changed: %v", err)
	}
}
