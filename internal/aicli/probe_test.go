//go:build !windows

package aicli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
		name, script, diagnostic string
		limited                  bool
	}{
		{"output cap", fmt.Sprintf("head -c %d /dev/zero\nexec sleep 30\n", MaxEventBytes+1), "", true},
		{"diagnostic", "printf 'first\\nforged journal line' >&2\nexit 7\n", `first\nforged journal line`, false},
		{"quiet failure", "exit 7\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := probeExecutable(t, tc.script)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, err := Probe(ctx, binary, "", nil, t.TempDir(), os.Environ(), "", "")
			if tc.limited {
				if !errors.Is(err, ErrOutputLimit) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("limit cause lost: %v", err)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
					t.Fatalf("native failure cause lost: %v", err)
				}
			}
			if tc.diagnostic == "" {
				if strings.Contains(err.Error(), `: ""`) {
					t.Fatalf("empty diagnostic suffix: %v", err)
				}
			} else if !strings.Contains(err.Error(), tc.diagnostic) || strings.Contains(err.Error(), "\nforged journal line") {
				t.Fatalf("unquoted diagnostic: %v", err)
			}
		})
	}
}

func TestProbeExactBoundAndCallerCancellation(t *testing.T) {
	binary := probeExecutable(t, fmt.Sprintf("head -c %d /dev/zero\n", MaxEventBytes))
	output, err := Probe(t.Context(), binary, "", nil, t.TempDir(), os.Environ(), "", "")
	if err != nil || len(output) != MaxEventBytes {
		t.Fatalf("exact bound bytes=%d error=%v", len(output), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = Probe(ctx, binary, "", nil, t.TempDir(), os.Environ(), "", ""); !errors.Is(err, context.Canceled) || errors.Is(err, ErrOutputLimit) || err.Error() != context.Canceled.Error() {
		t.Fatalf("caller cancellation cause changed: %v", err)
	}
}

func TestProbeRetainsBoundedStdoutOnCLIError(t *testing.T) {
	binary := probeExecutable(t, "printf '%s\\n' '{\"type\":\"result\",\"is_error\":true}'\nexit 7\n")
	output, err := Probe(t.Context(), binary, "", nil, t.TempDir(), os.Environ(), "", "")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 || string(output) != "{\"type\":\"result\",\"is_error\":true}\n" {
		t.Fatalf("output %q, error %v", output, err)
	}
}
