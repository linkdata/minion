//go:build !linux

package processgroup

import (
	"errors"
	"fmt"
	"os/exec"
)

var (
	ErrDescendants = errors.New("command left descendant processes")
	ErrUnconfirmed = errors.New("command process cleanup unconfirmed")
)

// Handle is unavailable without Linux cgroup v2 process ownership.
type Handle struct{}

func unsupported() error {
	return fmt.Errorf("%w: cgroup v2 process ownership requires Linux", ErrUnconfirmed)
}

func Check() error { return unsupported() }

func Start(*exec.Cmd) (*Handle, error) { return nil, unsupported() }

func Run(*exec.Cmd) error { return unsupported() }

func (*Handle) Wait() error { return unsupported() }
