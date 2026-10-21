//go:build linux

package processgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	// ErrDescendants means the command's leader exited while processes remained
	// in its cgroup. The descendants were stopped, but the result is rejected.
	ErrDescendants = errors.New("command left descendant processes")
	// ErrUnconfirmed means process cleanup or quiescence could not be proved.
	ErrUnconfirmed = errors.New("command process cleanup unconfirmed")
)

const (
	cgroupMount  = "/sys/fs/cgroup"
	cgroup2Magic = 0x63677270
	cleanupWait  = 5 * time.Second
	settleWait   = 100 * time.Millisecond
)

// Handle owns the unique cgroup of a started command. Wait must be called to
// reap the leader, stop remaining descendants, and remove the cgroup.
type Handle struct {
	cmd       *exec.Cmd
	dir       string
	startErr  error
	cancelled atomic.Bool
	once      sync.Once
	err       error
}

// Run starts a command in its own cgroup and waits for complete cleanup.
func Run(cmd *exec.Cmd) error {
	h, err := Start(cmd)
	if err != nil {
		return err
	}
	return h.Wait()
}

// Check verifies that this process can create and remove a delegated cgroup.
// Start is the definitive check that the kernel allows atomic placement.
func Check() error {
	dir, group, err := newCgroup()
	if err != nil {
		return err
	}
	closeErr := group.Close()
	killFile, openErr := os.OpenFile(filepath.Join(dir, "cgroup.kill"), os.O_WRONLY, 0)
	if openErr == nil {
		openErr = killFile.Close()
	}
	populated, readErr := cgroupPopulated(dir)
	if populated && readErr == nil {
		readErr = errors.New("new cgroup unexpectedly populated")
	}
	removeErr := removeEmptyCgroup(dir)
	if err := errors.Join(closeErr, openErr, readErr, removeErr); err != nil {
		return fmt.Errorf("%w: check delegated cgroup: %v", ErrUnconfirmed, err)
	}
	return nil
}

// Start atomically places the command leader in a fresh cgroup. The caller
// must call Wait after a successful Start, including when the context expires.
func Start(cmd *exec.Cmd) (*Handle, error) {
	if cmd == nil {
		return nil, errors.New("nil command")
	}
	dir, group, err := newCgroup()
	if err != nil {
		return nil, err
	}
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	previousAttr, previousCancel := cmd.SysProcAttr, cmd.Cancel
	h := &Handle{cmd: cmd, dir: dir}
	attr.UseCgroupFD, attr.CgroupFD = true, int(group.Fd())
	cmd.SysProcAttr = &attr
	if cmd.WaitDelay <= 0 || cmd.WaitDelay > 2*time.Second {
		cmd.WaitDelay = 2 * time.Second
	}
	if previousCancel != nil {
		cmd.Cancel = func() error {
			h.cancelled.Store(true)
			return killCgroup(dir)
		}
	}
	err = cmd.Start()
	cmd.SysProcAttr = previousAttr
	if err != nil {
		cmd.Cancel = previousCancel
	}
	closeErr := group.Close()
	if err != nil {
		cleanupErr := cleanupStartFailure(dir)
		if cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("%w: remove cgroup after start failure: %v", ErrUnconfirmed, cleanupErr))
		}
		return nil, err
	}
	// Once started, ownership always passes to a Handle. Report a descriptor
	// close error from Wait so a caller never discards a live command on error.
	h.startErr = closeErr
	return h, nil
}

func cleanupStartFailure(dir string) error {
	if err := removeEmptyCgroup(dir); err == nil {
		return nil
	}
	// An exec failure can occur after clone and before the cgroup observes the
	// short-lived child as gone. Kill and prove it empty before giving up.
	_ = killCgroup(dir)
	if err := waitEmptyCgroup(dir); err != nil {
		return err
	}
	return removeEmptyCgroup(dir)
}

// Wait reaps the leader, then proves the recursive cgroup empty before removal.
// A successful leader exit is rejected if any descendants survived it.
func (h *Handle) Wait() error {
	h.once.Do(func() {
		h.err = errors.Join(h.startErr, h.cmd.Wait(), cleanupCgroup(h.dir, h.cancelled.Load()))
	})
	return h.err
}

func newCgroup() (string, *os.File, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(cgroupMount, &stat); err != nil {
		return "", nil, fmt.Errorf("%w: cgroup v2 mount unavailable: %v", ErrUnconfirmed, err)
	}
	if uint64(stat.Type) != cgroup2Magic {
		return "", nil, fmt.Errorf("%w: %s is not a cgroup v2 mount", ErrUnconfirmed, cgroupMount)
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", nil, fmt.Errorf("%w: read current cgroup: %v", ErrUnconfirmed, err)
	}
	var current string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			current = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if !filepath.IsAbs(current) || filepath.Clean(current) != current {
		return "", nil, fmt.Errorf("%w: invalid current cgroup %q", ErrUnconfirmed, current)
	}
	parent := filepath.Join(cgroupMount, strings.TrimPrefix(current, "/"))
	dir, err := os.MkdirTemp(parent, cgroupNamePrefix())
	if err != nil {
		return "", nil, fmt.Errorf("%w: create delegated cgroup: %v", ErrUnconfirmed, err)
	}
	group, err := os.Open(dir)
	if err != nil {
		removeErr := os.Remove(dir)
		return "", nil, errors.Join(fmt.Errorf("%w: open delegated cgroup: %v", ErrUnconfirmed, err), removeErr)
	}
	return dir, group, nil
}

func cgroupNamePrefix() string {
	return fmt.Sprintf("minion-%d-", os.Getpid())
}

func cleanupCgroup(dir string, cancelled bool) error {
	populated, readErr := cgroupPopulated(dir)
	if populated && readErr == nil {
		// Reaping the leader can precede the kernel's cgroup.events update.
		// Give that update a short chance before treating it as a survivor.
		populated, readErr = waitCgroupSettle(dir)
	}
	if populated || readErr != nil {
		// If observation failed, stop the group anyway before attempting proof.
		killErr := killCgroup(dir)
		if err := waitEmptyCgroup(dir); err != nil {
			return errors.Join(fmt.Errorf("%w: await empty cgroup: %v", ErrUnconfirmed, err), readErr, killErr)
		}
		if err := removeEmptyCgroup(dir); err != nil {
			return errors.Join(fmt.Errorf("%w: remove empty cgroup: %v", ErrUnconfirmed, err), readErr, killErr)
		}
		if readErr != nil {
			return fmt.Errorf("%w: inspect cgroup after wait: %v", ErrUnconfirmed, readErr)
		}
		if cancelled {
			return killErr
		}
		return errors.Join(ErrDescendants, killErr)
	}
	if err := removeEmptyCgroup(dir); err != nil {
		// An apparently empty group can become busy before removal. Stop it
		// before returning an unconfirmed result instead of leaving a child.
		killErr := killCgroup(dir)
		emptyErr := waitEmptyCgroup(dir)
		removeErr := removeEmptyCgroup(dir)
		return errors.Join(fmt.Errorf("%w: remove empty cgroup: %v", ErrUnconfirmed, err), killErr, emptyErr, removeErr)
	}
	return nil
}

func waitCgroupSettle(dir string) (bool, error) {
	deadline := time.Now().Add(settleWait)
	for {
		populated, err := cgroupPopulated(dir)
		if err != nil || !populated || time.Now().After(deadline) {
			return populated, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func killCgroup(dir string) error {
	return os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0)
}

func cgroupPopulated(dir string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			switch fields[1] {
			case "0":
				return false, nil
			case "1":
				return true, nil
			}
		}
	}
	return false, errors.New("cgroup.events lacks valid populated value")
}

func waitEmptyCgroup(dir string) error {
	deadline := time.Now().Add(cleanupWait)
	for {
		populated, err := cgroupPopulated(dir)
		if err != nil || !populated {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("cgroup remained populated after kill")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func removeEmptyCgroup(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := removeEmptyCgroup(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return os.Remove(dir)
}
