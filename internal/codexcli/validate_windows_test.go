//go:build windows

package codexcli

import (
	"errors"
	"os/exec"
	"testing"
)

func TestExpectedDiscoveryStopWindowsRequiresDirectStop(t *testing.T) {
	stopped := exec.CommandContext(t.Context(), "cmd.exe", "/c", "exit", "1").Run()
	failed := exec.CommandContext(t.Context(), "cmd.exe", "/c", "exit", "7").Run()
	if stopped == nil || failed == nil || expectedDiscoveryStop(stopped, false) || !expectedDiscoveryStop(stopped, true) ||
		expectedDiscoveryStop(failed, true) || expectedDiscoveryStop(errors.Join(stopped, exec.ErrWaitDelay), true) {
		t.Fatalf("Windows discovery shutdown classification: stopped=%v failed=%v", stopped, failed)
	}
}
