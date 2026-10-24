//go:build windows

package codexcli

import (
	"os/exec"
	"testing"
)

func TestExpectedDiscoveryStopWindowsRequiresDirectStop(t *testing.T) {
	err := exec.CommandContext(t.Context(), "cmd.exe", "/c", "exit", "7").Run()
	if err == nil || expectedDiscoveryStop(err, false) || !expectedDiscoveryStop(err, true) || expectedDiscoveryStop(exec.ErrWaitDelay, true) {
		t.Fatalf("Windows discovery shutdown classification: %v", err)
	}
}
