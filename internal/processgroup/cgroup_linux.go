//go:build linux

package processgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	DefaultMaxTasks     = 128
	DefaultMaxMemoryMiB = 3072
	cgroupMount         = "/sys/fs/cgroup"
	cgroup2Magic        = 0x63677270
	supervisorLeaf      = "minion-supervisor"
	cleanupWait         = 5 * time.Second
	graceWait           = time.Second
	settleWait          = 100 * time.Millisecond
)

type limits struct {
	tasks  int
	memory int64
}

var (
	limitsMu      sync.RWMutex
	commandLimits = limits{DefaultMaxTasks, DefaultMaxMemoryMiB << 20}
)

// Configure sets finite per-command ceilings before startup preflight.
func Configure(maxTasks int, maxMemoryBytes int64) error {
	if maxTasks < 1 || maxMemoryBytes < 1 {
		return errors.New("command process and memory ceilings must be positive")
	}
	limitsMu.Lock()
	commandLimits = limits{maxTasks, maxMemoryBytes}
	limitsMu.Unlock()
	return nil
}

func configuredLimits() limits {
	limitsMu.RLock()
	defer limitsMu.RUnlock()
	return commandLimits
}

// Handle owns the unique cgroup of a started command. Wait must be called to
// reap the leader, stop remaining descendants, and remove the cgroup.
type Handle struct {
	cmd       *exec.Cmd
	dir       string
	limits    limits
	startErr  error
	cancelled atomic.Bool
	forced    atomic.Bool
	cancelMu  sync.Mutex
	cancelErr error
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

// Check proves placement, detached-descendant stopping, and scope release.
func Check() error {
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		return fmt.Errorf("process probe requires setsid: %w", err)
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		return fmt.Errorf("process probe requires sleep: %w", err)
	}
	cmd := exec.Command(setsid, "-f", sleep, "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	limit := configuredLimits()
	probeLimits := limit
	probeLimits.tasks = max(probeLimits.tasks, 4)
	probeLimits.memory = max(probeLimits.memory, 64<<20)
	if limit != probeLimits {
		// Prove the requested ceilings, then allow enough room to test detached cleanup.
		dir, group, err := newCgroup(limit)
		if err != nil {
			return fmt.Errorf("process probe configured ceilings: %w", err)
		}
		closeErr := group.Close()
		removeErr := unreleasedScope(dir, removeEmptyCgroup(dir))
		if closeErr != nil || removeErr != nil {
			return errors.Join(fmt.Errorf("%w: process probe configured ceilings", ErrUnconfirmed), closeErr, removeErr)
		}
	}
	h, err := start(cmd, probeLimits)
	if err != nil {
		return fmt.Errorf("process probe start: %w", err)
	}
	err = h.Wait()
	if !errors.Is(err, ErrDescendants) || errors.Is(err, ErrUnconfirmed) || errors.Is(err, ErrExhausted) ||
		cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		return errors.Join(fmt.Errorf("%w: process probe did not prove detached descendant cleanup", ErrUnconfirmed), err)
	}
	return nil
}

// Start atomically places the command leader in a fresh cgroup. The caller
// must call Wait after a successful Start, including when the context expires.
func Start(cmd *exec.Cmd) (*Handle, error) {
	if cmd == nil {
		return nil, errors.New("nil command")
	}
	return start(cmd, configuredLimits())
}

func start(cmd *exec.Cmd, limit limits) (*Handle, error) {
	dir, group, err := newCgroup(limit)
	if err != nil {
		return nil, err
	}
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	previousAttr, previousCancel := cmd.SysProcAttr, cmd.Cancel
	h := &Handle{cmd: cmd, dir: dir, limits: limit}
	attr.UseCgroupFD, attr.CgroupFD = true, int(group.Fd())
	cmd.SysProcAttr = &attr
	if cmd.WaitDelay <= 0 || cmd.WaitDelay > 2*time.Second {
		cmd.WaitDelay = 2 * time.Second
	}
	if previousCancel != nil {
		cmd.Cancel = func() error {
			h.cancelled.Store(true)
			forced, stopErr := stopCgroupGracefully(cmd, dir)
			h.forced.Store(forced)
			h.cancelMu.Lock()
			h.cancelErr = stopErr
			h.cancelMu.Unlock()
			return stopErr
		}
	}
	err = cmd.Start()
	cmd.SysProcAttr = previousAttr
	if err != nil {
		cmd.Cancel = previousCancel
	}
	closeErr := group.Close()
	if err != nil {
		limitErr := readExhaustion(dir, limit)
		cleanupErr := unreleasedScope(dir, cleanupStartFailure(dir))
		if event, ok := limitErr.(*Exhaustion); ok {
			event.Stop, event.Released = "start_failed", !errors.Is(cleanupErr, ErrScopeUnconfirmed)
			if !event.Released {
				event.Scope = dir
			}
		}
		if cleanupErr != nil {
			return nil, errors.Join(err, limitErr, fmt.Errorf("%w: remove cgroup %s after start failure: %w", ErrUnconfirmed, dir, cleanupErr))
		}
		return nil, errors.Join(err, limitErr)
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
		cmdErr := h.cmd.Wait()
		h.cancelMu.Lock()
		cancelErr := h.cancelErr
		h.cancelMu.Unlock()
		limitErr := readExhaustion(h.dir, h.limits)
		cleanupErr := unreleasedScope(h.dir, cleanupCgroup(h.dir, h.cancelled.Load()))
		if event, ok := limitErr.(*Exhaustion); ok {
			event.Stop = "natural"
			if h.cancelled.Load() {
				event.Stop = "graceful"
				if h.forced.Load() {
					event.Stop = "forced"
				}
			} else if errors.Is(cleanupErr, ErrDescendants) || errors.Is(cleanupErr, ErrUnconfirmed) {
				event.Stop = "forced"
			}
			event.Released = !errors.Is(cleanupErr, ErrScopeUnconfirmed)
			if !event.Released {
				event.Scope = h.dir
			}
		}
		h.err = errors.Join(h.startErr, cmdErr, cancelErr, limitErr, cleanupErr)
	})
	return h.err
}

func newCgroup(limit limits) (string, *os.File, error) {
	parent, err := delegatedParent()
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(parent, cgroupNamePrefix())
	if err != nil {
		return "", nil, fmt.Errorf("%w: create delegated cgroup under %s: %v", ErrUnconfirmed, parent, err)
	}
	if err := setCgroupLimits(dir, limit); err != nil {
		removeErr := os.Remove(dir)
		return "", nil, unreleasedScope(dir, errors.Join(fmt.Errorf("%w: configure cgroup %s: %v", ErrUnconfirmed, dir, err), removeErr))
	}
	group, err := os.Open(dir)
	if err != nil {
		removeErr := os.Remove(dir)
		return "", nil, unreleasedScope(dir, errors.Join(fmt.Errorf("%w: open delegated cgroup %s: %v", ErrUnconfirmed, dir, err), removeErr))
	}
	return dir, group, nil
}

func delegatedParent() (string, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(cgroupMount, &stat); err != nil {
		return "", fmt.Errorf("%w: cgroup v2 mount unavailable: %v", ErrUnconfirmed, err)
	}
	if uint64(stat.Type) != cgroup2Magic {
		return "", fmt.Errorf("%w: %s is not a cgroup v2 mount", ErrUnconfirmed, cgroupMount)
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("%w: read current cgroup: %v", ErrUnconfirmed, err)
	}
	var current string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			current = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if !filepath.IsAbs(current) || filepath.Clean(current) != current || filepath.Base(current) != supervisorLeaf || filepath.Dir(current) == "/" {
		return "", fmt.Errorf("%w: current cgroup %q must be a %s leaf below a delegated service", ErrUnconfirmed, current, supervisorLeaf)
	}
	parent := filepath.Join(cgroupMount, strings.TrimPrefix(filepath.Dir(current), "/"))
	procs, err := os.ReadFile(filepath.Join(parent, "cgroup.procs"))
	if err != nil || len(strings.TrimSpace(string(procs))) != 0 {
		return "", fmt.Errorf("%w: delegated parent %s must have no direct processes: %v", ErrUnconfirmed, parent, err)
	}
	controllers, err := os.ReadFile(filepath.Join(parent, "cgroup.controllers"))
	if err != nil {
		return "", fmt.Errorf("%w: read controllers of %s: %v", ErrUnconfirmed, parent, err)
	}
	for _, name := range []string{"memory", "pids"} {
		if !slices.Contains(strings.Fields(string(controllers)), name) {
			return "", fmt.Errorf("%w: %s controller unavailable in %s", ErrUnconfirmed, name, parent)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory +pids"), 0); err != nil {
		return "", fmt.Errorf("%w: enable memory and pids controllers in %s: %v", ErrUnconfirmed, parent, err)
	}
	enabled, err := os.ReadFile(filepath.Join(parent, "cgroup.subtree_control"))
	if err != nil {
		return "", fmt.Errorf("%w: verify controllers in %s: %v", ErrUnconfirmed, parent, err)
	}
	for _, name := range []string{"memory", "pids"} {
		if !slices.Contains(strings.Fields(string(enabled)), name) {
			return "", fmt.Errorf("%w: %s controller not enabled in %s", ErrUnconfirmed, name, parent)
		}
	}
	return parent, nil
}

func cgroupNamePrefix() string {
	return fmt.Sprintf("minion-%d-", os.Getpid())
}

func setCgroupLimits(dir string, limit limits) error {
	for _, setting := range []struct{ name, value string }{
		{"pids.max", strconv.Itoa(limit.tasks)},
		{"memory.max", strconv.FormatInt(limit.memory, 10)},
		{"memory.swap.max", "0"},
		{"memory.oom.group", "1"},
		{"cgroup.max.descendants", "0"},
	} {
		path := filepath.Join(dir, setting.name)
		if err := os.WriteFile(path, []byte(setting.value), 0); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		actual, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(actual)) != setting.value {
			return fmt.Errorf("verify %s=%s: got %q, error %v", path, setting.value, strings.TrimSpace(string(actual)), err)
		}
	}
	return nil
}

func readExhaustion(dir string, limit limits) error {
	pids, err := readCounters(filepath.Join(dir, "pids.events"))
	if err != nil {
		return fmt.Errorf("%w: read limit events in %s: %v", ErrUnconfirmed, dir, err)
	}
	memory, err := readCounters(filepath.Join(dir, "memory.events"))
	if err != nil {
		return fmt.Errorf("%w: read limit events in %s: %v", ErrUnconfirmed, dir, err)
	}
	swap, err := readCounters(filepath.Join(dir, "memory.swap.events"))
	if err != nil {
		return fmt.Errorf("%w: read limit events in %s: %v", ErrUnconfirmed, dir, err)
	}
	if _, ok := pids["max"]; !ok {
		return fmt.Errorf("%w: pids.events in %s lacks max", ErrUnconfirmed, dir)
	}
	for _, key := range []string{"max", "oom", "oom_kill"} {
		if _, ok := memory[key]; !ok {
			return fmt.Errorf("%w: memory.events in %s lacks %s", ErrUnconfirmed, dir, key)
		}
	}
	if _, ok := swap["max"]; !ok {
		return fmt.Errorf("%w: memory.swap.events in %s lacks max", ErrUnconfirmed, dir)
	}
	if (memory["oom_kill"] != 0 || memory["oom_group_kill"] != 0) && memory["max"] == 0 && memory["oom"] == 0 {
		return fmt.Errorf("%w: cgroup %s observed OOM kill without reaching memory.max", ErrUnconfirmed, dir)
	}
	if pids["max"] == 0 && memory["max"] == 0 && memory["oom"] == 0 && swap["max"] == 0 {
		return nil
	}
	return &Exhaustion{
		MaxTasks: limit.tasks, MaxMemoryBytes: limit.memory,
		PIDEventsMax: pids["max"], MemoryEventsMax: memory["max"],
		MemorySwapEventsMax: swap["max"],
		MemoryOOM:           memory["oom"], MemoryOOMKill: memory["oom_kill"], MemoryOOMGroupKill: memory["oom_group_kill"],
	}
}

func readCounters(path string) (map[string]uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]uint64)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid counter %q", line)
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid counter %q: %w", line, err)
		}
		values[fields[0]] = value
	}
	return values, nil
}

func stopCgroupGracefully(cmd *exec.Cmd, dir string) (bool, error) {
	signalErr := cmd.Process.Signal(syscall.SIGTERM)
	if errors.Is(signalErr, os.ErrProcessDone) {
		signalErr = nil
	}
	deadline := time.Now().Add(graceWait)
	for {
		populated, err := cgroupPopulated(dir)
		if err == nil && !populated {
			return false, signalErr
		}
		if err != nil || time.Now().After(deadline) {
			if killErr := killCgroup(dir); killErr != nil {
				return true, errors.Join(signalErr, fmt.Errorf("%w: force-stop cgroup %s: %v", ErrUnconfirmed, dir, killErr))
			}
			return true, signalErr
		}
		time.Sleep(20 * time.Millisecond)
	}
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
			return errors.Join(fmt.Errorf("%w: await empty cgroup %s: %v", ErrUnconfirmed, dir, err), readErr, killErr)
		}
		if err := removeEmptyCgroup(dir); err != nil {
			return errors.Join(fmt.Errorf("%w: remove empty cgroup %s: %v", ErrUnconfirmed, dir, err), readErr, killErr)
		}
		if readErr != nil {
			return fmt.Errorf("%w: inspect cgroup %s after wait: %v", ErrUnconfirmed, dir, readErr)
		}
		if killErr != nil {
			return errors.Join(ErrUnconfirmed, ErrDescendants, killErr)
		}
		if cancelled {
			return nil
		}
		return ErrDescendants
	}
	if err := removeEmptyCgroup(dir); err != nil {
		// An apparently empty group can become busy before removal. Stop it
		// before returning an unconfirmed result instead of leaving a child.
		killErr := killCgroup(dir)
		emptyErr := waitEmptyCgroup(dir)
		removeErr := removeEmptyCgroup(dir)
		return errors.Join(fmt.Errorf("%w: remove empty cgroup %s: %v", ErrUnconfirmed, dir, err), killErr, emptyErr, removeErr)
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
	return os.Remove(dir)
}

func unreleasedScope(dir string, err error) error {
	if err == nil {
		return nil
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("%w: %s", ErrScopeUnconfirmed, dir), statErr)
	}
	return err
}
