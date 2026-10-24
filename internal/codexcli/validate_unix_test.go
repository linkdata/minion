//go:build !windows

package codexcli

import (
	"os/exec"
	"testing"
)

func TestExpectedDiscoveryStopRequiresDirectSignal(t *testing.T) {
	err := exec.CommandContext(t.Context(), "sh", "-c", "kill -TERM $$").Run()
	if err == nil || expectedDiscoveryStop(err, false) || !expectedDiscoveryStop(err, true) {
		t.Fatalf("Unix discovery shutdown classification: %v", err)
	}
}
