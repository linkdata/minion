package wintest

import (
	"fmt"
	"os"
	"os/exec"
)

// CreateJunction keeps fixture paths out of a command interpreter.
func CreateJunction(link, target string) error {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"New-Item -ItemType Junction -Path $env:MINION_TEST_JUNCTION_LINK -Target $env:MINION_TEST_JUNCTION_TARGET -ErrorAction Stop | Out-Null")
	cmd.Env = append(os.Environ(), "MINION_TEST_JUNCTION_LINK="+link, "MINION_TEST_JUNCTION_TARGET="+target)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create junction %q -> %q: %w: %s", link, target, err, output)
	}
	return nil
}
