//go:build linux

package processgroup

import (
	"errors"
	"fmt"
	"io"
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
	workerMaxGroups  = 256
	workerMaxEntries = 65536
	workerControlMax = 4096
)

// WorkerHandle owns a worker and its command scopes. Every nonnil handle must
// reach Wait, even when StartWorker returned an error.
type WorkerHandle struct {
	cmd                *exec.Cmd
	dir                string
	parent             *os.File
	root               *os.File
	supervisor         *os.File
	supervisorRequired bool
	started            bool
	startErr           error
	cancelled          atomic.Bool
	cancelMu           sync.Mutex
	cancelErr          error
	once               sync.Once
	released           bool
	err                error
}

// StartWorker records ownership before atomically placing the worker in its
// supervisor leaf. Any created root returns a handle, including on error;
// the caller must call Wait and retain both the start and wait errors.
// The caller keeps cgroup mutation under trusted host control; live identity
// checks reject observed replacements.
func StartWorker(cmd *exec.Cmd, record func(scope string) error) (h *WorkerHandle, resultErr error) {
	if cmd == nil || record == nil {
		return nil, errors.New("worker command and ownership recorder are required")
	}
	if cmd.Process != nil || cmd.ProcessState != nil {
		return nil, errors.New("worker command was already started")
	}
	parentPath, err := delegatedParent()
	if err != nil {
		return nil, err
	}
	parent, err := openWorkerDirectory(nil, parentPath)
	if err != nil {
		return nil, fmt.Errorf("%w: pin worker parent: %w", ErrUnconfirmed, err)
	}
	dir, err := os.MkdirTemp(workerDirectoryPath(parent, ""), cgroupNamePrefix()+"worker-")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("%w: create worker root: %w", ErrUnconfirmed, err), parent.Close())
	}
	dir = filepath.Join(parentPath, filepath.Base(dir))
	h = &WorkerHandle{cmd: cmd, dir: dir, parent: parent}
	defer func() { h.startErr = resultErr }()
	h.root, err = openWorkerDirectory(parent, filepath.Base(dir))
	if err != nil {
		return h, fmt.Errorf("%w: pin worker root %s: %w", ErrUnconfirmed, dir, err)
	}
	if err := h.verifyRoot(); err != nil {
		return h, err
	}
	controllers, err := readWorkerControl(h.root, "cgroup.controllers")
	if err != nil {
		return h, err
	}
	for _, name := range []string{"memory", "pids"} {
		if !slices.Contains(strings.Fields(controllers), name) {
			return h, fmt.Errorf("worker %s controller unavailable", name)
		}
	}
	if err := writeWorkerControl(h.root, "cgroup.subtree_control", "+memory +pids"); err != nil {
		return h, err
	}
	enabled, err := readWorkerControl(h.root, "cgroup.subtree_control")
	if err != nil {
		return h, err
	}
	for _, name := range []string{"memory", "pids"} {
		if !slices.Contains(strings.Fields(enabled), name) {
			return h, fmt.Errorf("worker %s controller was not enabled", name)
		}
	}
	if err := syscall.Mkdirat(int(h.root.Fd()), supervisorLeaf, 0700); err != nil {
		h.supervisorRequired = errors.Is(err, syscall.EEXIST)
		return h, err
	}
	h.supervisorRequired = true
	h.supervisor, err = openWorkerDirectory(h.root, supervisorLeaf)
	if err != nil {
		return h, err
	}
	if err := writeWorkerControl(h.supervisor, "cgroup.max.descendants", "0"); err != nil {
		return h, err
	}
	if value, err := readWorkerControl(h.supervisor, "cgroup.max.descendants"); err != nil || strings.TrimSpace(value) != "0" {
		return h, errors.Join(errors.New("worker supervisor descendants bound was not applied"), err)
	}
	if err := record(dir); err != nil {
		return h, err
	}
	// The callback may have waited on a context-free save; recheck live ownership.
	if err := h.verifyWorker(); err != nil {
		return h, err
	}
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	previousAttr, previousCancel := cmd.SysProcAttr, cmd.Cancel
	attr.UseCgroupFD, attr.CgroupFD = true, int(h.supervisor.Fd())
	cmd.SysProcAttr = &attr
	if cmd.WaitDelay <= 0 || cmd.WaitDelay > 2*time.Second {
		cmd.WaitDelay = 2 * time.Second
	}
	if previousCancel != nil {
		cmd.Cancel = func() error {
			h.cancelled.Store(true)
			err := h.stop()
			h.cancelMu.Lock()
			h.cancelErr = errors.Join(h.cancelErr, err)
			h.cancelMu.Unlock()
			return err
		}
	}
	err = cmd.Start()
	cmd.SysProcAttr = previousAttr
	h.started = err == nil
	if err != nil {
		cmd.Cancel = previousCancel
	}
	return h, err
}

// Wait reaps once, then proves and removes the whole owned tree. Released is
// positive cleanup evidence, independent of start, exit and descriptor errors.
func (h *WorkerHandle) Wait() (released bool, err error) {
	h.once.Do(func() {
		var cmdErr error
		if h.started {
			// Cmd.Wait joins its context watcher, so Cancel cannot race FD cleanup.
			cmdErr = h.cmd.Wait()
		}
		h.cancelMu.Lock()
		cancelErr := h.cancelErr
		h.cancelMu.Unlock()
		workerSucceeded := h.started && h.startErr == nil && cmdErr == nil && cancelErr == nil && !h.cancelled.Load()
		h.released, err = h.release(workerSucceeded)
		if !h.released {
			err = errors.Join(err, ErrUnconfirmed, fmt.Errorf("%w: %s", ErrScopeUnconfirmed, h.dir))
		}
		var closeErr error
		if h.supervisor != nil {
			closeErr = h.supervisor.Close()
		}
		if h.root != nil {
			closeErr = errors.Join(closeErr, h.root.Close())
		}
		if h.parent != nil {
			closeErr = errors.Join(closeErr, h.parent.Close())
		}
		h.err = errors.Join(h.startErr, cmdErr, cancelErr, err, closeErr)
	})
	return h.released, h.err
}

func openWorkerDirectory(parent *os.File, name string) (*os.File, error) {
	flags := syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	var fd int
	var err error
	if parent == nil {
		fd, err = syscall.Open(name, flags, 0)
	} else {
		fd, err = syscall.Openat(int(parent.Fd()), name, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	var stat syscall.Statfs_t
	if err := syscall.Fstatfs(fd, &stat); err != nil || uint64(stat.Type) != cgroup2Magic {
		return nil, errors.Join(errors.New("worker directory is not cgroup v2"), err, file.Close())
	}
	return file, nil
}

func sameWorkerDirectory(parent *os.File, name string, pinned *os.File) error {
	current, err := openWorkerDirectory(parent, name)
	if err != nil {
		return err
	}
	want, wantErr := pinned.Stat()
	actual, actualErr := current.Stat()
	closeErr := current.Close()
	if wantErr != nil || actualErr != nil || !os.SameFile(want, actual) {
		return errors.Join(errors.New("worker directory identity changed"), wantErr, actualErr, closeErr)
	}
	return closeErr
}

func (h *WorkerHandle) verifyRoot() error {
	if h.parent == nil || h.root == nil {
		return errors.New("worker root is not pinned")
	}
	parent, err := os.Lstat(filepath.Dir(h.dir))
	if err != nil {
		return err
	}
	pinned, err := h.parent.Stat()
	if err != nil || !os.SameFile(parent, pinned) {
		return errors.Join(errors.New("worker parent identity changed"), err)
	}
	return sameWorkerDirectory(h.parent, filepath.Base(h.dir), h.root)
}

func (h *WorkerHandle) verifyWorker() error {
	if err := h.verifyRoot(); err != nil {
		return err
	}
	if !h.supervisorRequired {
		return nil
	}
	if h.supervisor == nil {
		return errors.New("worker supervisor is not pinned")
	}
	return sameWorkerDirectory(h.root, supervisorLeaf, h.supervisor)
}

func readWorkerControl(dir *os.File, name string) (value string, resultErr error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	data, err := io.ReadAll(io.LimitReader(file, workerControlMax+1))
	if len(data) > workerControlMax {
		return "", errors.New("worker control file exceeds inspection bound")
	}
	return string(data), err
}

func writeWorkerControl(dir *os.File, name, value string) (resultErr error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	n, err := io.WriteString(file, value)
	if err == nil && n != len(value) {
		err = io.ErrShortWrite
	}
	return err
}

func (h *WorkerHandle) populated() (bool, error) {
	value, err := readWorkerControl(h.root, "cgroup.events")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(value, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" && (fields[1] == "0" || fields[1] == "1") {
			return fields[1] == "1", nil
		}
	}
	return false, errors.New("worker cgroup.events lacks valid population")
}

func (h *WorkerHandle) kill() error {
	if err := h.verifyWorker(); err != nil {
		return err
	}
	return writeWorkerControl(h.root, "cgroup.kill", "1")
}

func (h *WorkerHandle) stop() error {
	if err := h.verifyWorker(); err != nil {
		return fmt.Errorf("%w: stop worker: %w", ErrUnconfirmed, err)
	}
	signalErr := h.cmd.Process.Signal(syscall.SIGTERM)
	if errors.Is(signalErr, os.ErrProcessDone) {
		signalErr = nil
	}
	deadline := time.Now().Add(graceWait)
	for {
		populated, err := h.populated()
		if err == nil && !populated {
			return signalErr
		}
		if err != nil || !time.Now().Before(deadline) {
			killErr := h.kill()
			if err != nil || killErr != nil {
				return errors.Join(signalErr, ErrUnconfirmed, err, killErr)
			}
			return signalErr
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *WorkerHandle) release(workerSucceeded bool) (released bool, resultErr error) {
	if err := h.verifyWorker(); err != nil {
		return false, err
	}
	populated, readErr := h.populated()
	settle := time.Now().Add(settleWait)
	for populated && readErr == nil && time.Now().Before(settle) {
		time.Sleep(10 * time.Millisecond)
		populated, readErr = h.populated()
	}
	if populated || readErr != nil {
		killErr := h.kill()
		if readErr != nil || killErr != nil {
			resultErr = errors.Join(ErrUnconfirmed, readErr, killErr)
		}
		deadline := time.Now().Add(cleanupWait)
		for {
			busy, err := h.populated()
			if err != nil {
				return false, errors.Join(resultErr, err)
			}
			if !busy {
				break
			}
			if !time.Now().Before(deadline) {
				return false, errors.Join(resultErr, errors.New("worker remained populated after kill"))
			}
			time.Sleep(20 * time.Millisecond)
		}
		if populated && readErr == nil && !h.cancelled.Load() && h.started {
			resultErr = errors.Join(resultErr, ErrDescendants)
		}
	}
	// Only direct supervisor/command leaves are valid; inspect finite chunks.
	entries, groups := 0, 0
	for {
		children, err := h.root.ReadDir(128)
		for _, child := range children {
			entries++
			if entries > workerMaxEntries {
				return false, errors.Join(resultErr, errors.New("worker tree exceeds inspection bound"))
			}
			if !child.IsDir() {
				if child.Type()&os.ModeType != 0 {
					return false, errors.Join(resultErr, errors.New("worker tree contains a non-regular control"))
				}
				continue
			}
			groups++
			if groups > workerMaxGroups {
				return false, errors.Join(resultErr, errors.New("worker tree has too many command groups"))
			}
			leaf, openErr := openWorkerDirectory(h.root, child.Name())
			if openErr != nil {
				return false, errors.Join(resultErr, openErr)
			}
			removeErr := h.removeLeaf(child.Name(), leaf, &entries)
			closeErr := leaf.Close()
			if removeErr != nil {
				return false, errors.Join(resultErr, removeErr, closeErr)
			}
			resultErr = errors.Join(resultErr, closeErr)
			if workerSucceeded && resultErr == nil && child.Name() != supervisorLeaf {
				resultErr = fmt.Errorf("%w: worker left command scope %s", ErrUnconfirmed, filepath.Join(h.dir, child.Name()))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, errors.Join(resultErr, err)
		}
	}
	if err := h.verifyRoot(); err != nil {
		return false, errors.Join(resultErr, err)
	}
	if err := os.Remove(workerDirectoryPath(h.parent, filepath.Base(h.dir))); err != nil {
		return false, errors.Join(resultErr, err)
	}
	return true, resultErr
}

func (h *WorkerHandle) removeLeaf(name string, leaf *os.File, entries *int) error {
	for {
		children, err := leaf.ReadDir(128)
		for _, child := range children {
			*entries++
			if *entries > workerMaxEntries || child.IsDir() || child.Type()&os.ModeType != 0 {
				return errors.New("worker command group has an unsupported or excessive layout")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if err := h.verifyRoot(); err != nil {
		return err
	}
	if name == supervisorLeaf {
		if h.supervisor == nil {
			return errors.New("worker supervisor is not pinned")
		}
		if err := sameWorkerDirectory(h.root, name, h.supervisor); err != nil {
			return err
		}
	}
	if err := sameWorkerDirectory(h.root, name, leaf); err != nil {
		return err
	}
	return os.Remove(workerDirectoryPath(h.root, name))
}

// Resolve removal through a live parent descriptor, never through saved scope text.
func workerDirectoryPath(parent *os.File, name string) string {
	return filepath.Join("/proc/self/fd", strconv.Itoa(int(parent.Fd())), name)
}
