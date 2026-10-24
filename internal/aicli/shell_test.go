//go:build !windows

package aicli

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func FuzzShellQuote(f *testing.F) {
	for _, value := range []string{"", "plain", "space ' apostrophe", "$PATH $(printf expanded)", "`printf expanded`", "line\nbreak", "'; printf expanded; '", "\xff"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if strings.ContainsRune(value, 0) || len(value) > 4096 {
			t.Skip("shell arguments cannot contain NUL; keep subprocess input bounded")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		// #nosec G204 -- This test verifies that ShellQuote preserves arbitrary input literally.
		data, err := exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%s' "+ShellQuote(value)).Output()
		if err != nil || string(data) != value {
			t.Fatalf("shell quote changed input %q: %q, %v", value, data, err)
		}
	})
}
