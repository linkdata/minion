//go:build linux

package codexcli

import (
	"os/exec"

	"github.com/linkdata/minion/internal/processgroup"
)

func configureCancellation(cmd *exec.Cmd) {
	processgroup.Configure(cmd)
}
