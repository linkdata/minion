//go:build windows

package aicli

import (
	"os/exec"
	"testing"
)

func makeJunctionWindows(t *testing.T, link, target string) {
	t.Helper()
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("create junction %q -> %q: %v: %s", link, target, err, output)
	}
}
