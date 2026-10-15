//go:build !linux

package claudecli

import "os/exec"

func configureCancellation(_ *exec.Cmd) {}
