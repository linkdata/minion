//go:build !linux

package processgroup

import (
	"errors"
	"fmt"
	"os/exec"
)

var (
	ErrDescendants = errors.New("command left descendant processes")
	// ErrUnconfirmed means command ownership, limit observation, or cleanup could not be proved.
	ErrUnconfirmed = errors.New("command process safety unconfirmed")
)

const (
	DefaultMaxTasks     = 128
	DefaultMaxMemoryMiB = 3072
)

func Configure(maxTasks int, maxMemoryBytes int64) error {
	if maxTasks < 1 || maxMemoryBytes < 1 {
		return errors.New("command process and memory ceilings must be positive")
	}
	return nil
}

// Handle is unavailable without Linux cgroup v2 process ownership.
type Handle struct{}

func unsupported() error {
	return fmt.Errorf("%w: cgroup v2 process ownership requires Linux", ErrUnconfirmed)
}

func Check() error { return unsupported() }

func Start(*exec.Cmd) (*Handle, error) { return nil, unsupported() }

func Run(*exec.Cmd) error { return unsupported() }

func (*Handle) Wait() error { return unsupported() }
