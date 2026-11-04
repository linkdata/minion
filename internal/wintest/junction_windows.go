package wintest

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// CreateJunction passes fixture paths through quoted command variables.
func CreateJunction(link, target string) error {
	cmd := exec.Command("cmd.exe")
	// cmd.exe parses /c itself, so Go's normal Windows argument quoting does not apply.
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + cmd.Path + `" /d /v:off /s /c "mklink /J "%MINION_TEST_JUNCTION_LINK%" "%MINION_TEST_JUNCTION_TARGET%""`}
	cmd.Env = append(os.Environ(), "MINION_TEST_JUNCTION_LINK="+link, "MINION_TEST_JUNCTION_TARGET="+target)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create junction %q -> %q: %w: %s", link, target, err, output)
	}
	return nil
}
