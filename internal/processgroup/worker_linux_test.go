//go:build linux

package processgroup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/workspace"
)

func TestWorkerRequiresRecordBeforeStart(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	report := filepath.Join(t.TempDir(), "worker")
	cmd := workerHelperCommand(context.Background(), "exit", report, false)
	before := testCgroups(t)
	if h, err := StartWorker(cmd, nil); err == nil || h != nil || cmd.Process != nil {
		if h != nil {
			t.Cleanup(func() { _, _ = h.Wait() })
		}
		t.Fatalf("missing record callback started or owned a worker: handle=%v process=%v error=%v", h, cmd.Process, err)
	}
	if after := testCgroups(t); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("missing record callback allocated a root: before=%v after=%v", before, after)
	}
	refused := errors.New("record refused")
	calls := 0
	var recorded string
	var callbackErr error
	h, startErr := StartWorker(cmd, func(scope string) error {
		calls++
		recorded = scope
		if cmd.Process != nil {
			callbackErr = errors.New("worker started before its recording callback")
		}
		if _, err := os.Stat(report); !errors.Is(err, os.ErrNotExist) {
			callbackErr = errors.Join(callbackErr, fmt.Errorf("worker acted before recording: %v", err))
		}
		if populated, err := cgroupPopulated(scope); err != nil || populated {
			callbackErr = errors.Join(callbackErr, fmt.Errorf("new lifetime root was not empty: populated=%t error=%v", populated, err))
		}
		return errors.Join(refused, callbackErr)
	})
	if h != nil {
		t.Cleanup(func() { _, _ = h.Wait() })
	}
	if h == nil || !errors.Is(startErr, refused) || callbackErr != nil || calls != 1 || cmd.Process != nil || recorded != h.dir {
		t.Fatalf("refused record lost ownership or started child: handle=%v calls=%d process=%v scope=%q error=%v", h, calls, cmd.Process, recorded, startErr)
	}
	released, waitErr := h.Wait()
	if !released || !errors.Is(errors.Join(startErr, waitErr), refused) || errors.Is(waitErr, ErrScopeUnconfirmed) {
		t.Fatalf("refused record root was not released with original cause: released=%t error=%v", released, waitErr)
	}
	if _, err := os.Stat(report); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused record worker acted: %v", err)
	}
	assertWorkerRemoved(t, h)
}

func TestWorkerFailedExecRetainsOwnership(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	invalid := filepath.Join(t.TempDir(), "invalid")
	if err := os.WriteFile(invalid, []byte("not an executable format\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), invalid} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			cmd := exec.Command(path)
			calls := 0
			h, startErr := StartWorker(cmd, func(scope string) error {
				calls++
				if cmd.Process != nil || !filepath.IsAbs(scope) {
					return fmt.Errorf("recording did not precede exec: process=%v scope=%q", cmd.Process, scope)
				}
				return nil
			})
			if h != nil {
				t.Cleanup(func() { _, _ = h.Wait() })
			}
			if h == nil || startErr == nil || calls != 1 {
				t.Fatalf("failed exec discarded its lifetime handle: handle=%v calls=%d error=%v", h, calls, startErr)
			}
			wantCause := syscall.ENOENT
			if path == invalid {
				wantCause = syscall.ENOEXEC
			}
			released, waitErr := h.Wait()
			if !released || !errors.Is(waitErr, wantCause) || errors.Is(waitErr, ErrScopeUnconfirmed) {
				t.Fatalf("failed exec lost cause or release: released=%t start=%v wait=%v", released, startErr, waitErr)
			}
			if againReleased, againErr := h.Wait(); againReleased != released || againErr != waitErr {
				t.Fatalf("repeated Wait changed failed exec result: (%t, %v), (%t, %v)", released, waitErr, againReleased, againErr)
			}
			assertWorkerRemoved(t, h)
		})
	}
}

func TestWorkerImmediateExitIsReapedOnce(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	cmd := workerHelperCommand(context.Background(), "exit", filepath.Join(t.TempDir(), "worker"), false)
	h, err := StartWorker(cmd, func(string) error { return nil })
	if h != nil {
		t.Cleanup(func() { _, _ = h.Wait() })
	}
	if err != nil || h == nil {
		t.Fatalf("start immediate worker: %v", err)
	}
	released, waitErr := h.Wait()
	if !released || waitErr != nil || cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("immediate worker was not reaped and released: released=%t error=%v state=%v", released, waitErr, cmd.ProcessState)
	}
	if againReleased, againErr := h.Wait(); !againReleased || againErr != nil {
		t.Fatalf("repeated immediate Wait: released=%t error=%v", againReleased, againErr)
	}
	assertWorkerRemoved(t, h)
}

func TestWorkerNestedSupervisorRunsOrdinaryCommands(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report := filepath.Join(t.TempDir(), "worker")
	cmd := workerHelperCommand(ctx, "nested-clean", report, false)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.WaitDelay = 10 * time.Second
	calls := 0
	h, err := StartWorker(cmd, func(scope string) error {
		calls++
		if cmd.Process != nil {
			return errors.New("worker started before successful recording")
		}
		return workspace.WriteAtomic(report+".recorded", []byte(scope))
	})
	if h != nil {
		t.Cleanup(func() { cancel(); _, _ = h.Wait() })
	}
	if err != nil || h == nil {
		t.Fatalf("start nested worker: %v", err)
	}
	if calls != 1 || cmd.WaitDelay != 2*time.Second {
		t.Fatalf("worker setup: record calls=%d pipe bound=%v", calls, cmd.WaitDelay)
	}
	paths := strings.Split(waitForReport(t, report), "\n")
	if len(paths) != 2 || paths[0] != filepath.Join(h.dir, supervisorLeaf) || filepath.Dir(paths[1]) != h.dir || paths[1] == paths[0] {
		t.Fatalf("worker and command did not share the lifetime parent: %q", paths)
	}
	for _, name := range []string{"memory", "pids"} {
		data, err := os.ReadFile(filepath.Join(h.dir, "cgroup.subtree_control"))
		if err != nil || !strings.Contains(" "+strings.TrimSpace(string(data))+" ", " "+name+" ") {
			t.Fatalf("worker root did not delegate %s: %q error=%v", name, data, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(h.dir, "cgroup.procs")); err != nil || strings.TrimSpace(string(data)) != "" {
		t.Fatalf("worker ran in internal root: %q error=%v", data, err)
	}
	gotLimits := strings.Split(waitForReport(t, report+".limits"), "\n")
	wantLimits := []string{strings.TrimPrefix(paths[1], cgroupMount), "64", strconv.FormatInt(512<<20, 10), "0", "1", "0"}
	if fmt.Sprint(gotLimits) != fmt.Sprint(wantLimits) {
		t.Fatalf("nested ordinary command lost its limits: got %q want %q", gotLimits, wantLimits)
	}
	command := waitForHelper(t, report+".command.pid")
	defer command.Close()
	if err := workspace.WriteAtomic(report+".release", []byte("release")); err != nil {
		t.Fatal(err)
	}
	released, waitErr := h.Wait()
	if !released || waitErr != nil || cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("nested worker failed: released=%t error=%v output=%q", released, waitErr, output.String())
	}
	if againReleased, againErr := h.Wait(); !againReleased || againErr != nil {
		t.Fatalf("repeated successful Wait: released=%t error=%v", againReleased, againErr)
	}
	waitCtx, stopWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopWait()
	if err := command.WaitExited(waitCtx); err != nil {
		t.Fatalf("nested ordinary command survived clean exit: %v", err)
	}
	assertWorkerRemoved(t, h)
}

func TestWorkerCrashDrainsRealCommandAndPreservesSibling(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	for _, holdPipe := range []bool{false, true} {
		t.Run(fmt.Sprintf("inherited_pipe=%t", holdPipe), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			siblingReport := filepath.Join(t.TempDir(), "sibling")
			siblingCmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessgroupHelperProcess$")
			siblingCmd.Env = append(os.Environ(), "MINION_PROCESSGROUP_TEST_MODE=limits-block", "MINION_PROCESSGROUP_TEST_PID_FILE="+siblingReport)
			sibling, err := Start(siblingCmd)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cancel(); _ = sibling.Wait() })
			_ = waitForReport(t, siblingReport)
			siblingPin, err := os.Open(sibling.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer siblingPin.Close()
			siblingInfo, err := siblingPin.Stat()
			if err != nil {
				t.Fatal(err)
			}
			report := filepath.Join(t.TempDir(), "worker")
			cmd := workerHelperCommand(ctx, "crash-command", report, holdPipe)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			h, err := StartWorker(cmd, func(string) error { return nil })
			if h != nil {
				t.Cleanup(func() { cancel(); _, _ = h.Wait() })
			}
			if err != nil || h == nil {
				t.Fatalf("start worker: %v", err)
			}
			paths := strings.Split(waitForReport(t, report), "\n")
			if len(paths) != 2 || paths[0] != filepath.Join(h.dir, supervisorLeaf) || filepath.Dir(paths[1]) != h.dir {
				t.Fatalf("crash helper did not create an owned ordinary scope: %q", paths)
			}
			leader := waitForHelper(t, report+".command.pid")
			defer leader.Close()
			child := waitForHelper(t, report+".child.pid")
			defer child.Close()
			if err := workspace.WriteAtomic(report+".release", []byte("crash")); err != nil {
				t.Fatal(err)
			}
			released, waitErr := h.Wait()
			var exitErr *exec.ExitError
			if !released || !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 23 || errors.Is(waitErr, ErrScopeUnconfirmed) || errors.Is(waitErr, ErrUnconfirmed) {
				t.Fatalf("crash lost exit cause or full release: released=%t error=%v output=%q", released, waitErr, output.String())
			}
			if againReleased, againErr := h.Wait(); !againReleased || againErr != waitErr {
				t.Fatalf("repeated crash Wait changed result: released=%t error=%v", againReleased, againErr)
			}
			waitCtx, stopWait := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopWait()
			for _, process := range []*pinnedProcess{leader, child} {
				if err := process.WaitExited(waitCtx); err != nil {
					t.Fatalf("ordinary command survived worker crash: %v", err)
				}
			}
			assertWorkerRemoved(t, h)
			currentSibling, statErr := os.Stat(sibling.dir)
			populated, populationErr := cgroupPopulated(sibling.dir)
			if statErr != nil || !os.SameFile(siblingInfo, currentSibling) || populationErr != nil || !populated {
				t.Fatalf("unrelated owned sibling changed or stopped: stat=%v population=%t error=%v", statErr, populated, populationErr)
			}
			cancel()
			if err := sibling.Wait(); err == nil || errors.Is(err, ErrUnconfirmed) || errors.Is(err, ErrScopeUnconfirmed) {
				t.Fatalf("independent sibling cleanup was not confirmed: %v", err)
			}
		})
	}
}

func TestWorkerCancellationDrainsInheritedPipes(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report := filepath.Join(t.TempDir(), "worker")
	cmd := workerHelperCommand(ctx, "block-command", report, true)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	h, err := StartWorker(cmd, func(string) error { return nil })
	if h != nil {
		t.Cleanup(func() { cancel(); _, _ = h.Wait() })
	}
	if err != nil || h == nil {
		t.Fatalf("start worker: %v", err)
	}
	_ = waitForReport(t, report)
	leader := waitForHelper(t, report+".command.pid")
	defer leader.Close()
	child := waitForHelper(t, report+".child.pid")
	defer child.Close()
	started := time.Now()
	cancel()
	released, waitErr := h.Wait()
	if !released || waitErr == nil || errors.Is(waitErr, ErrUnconfirmed) || errors.Is(waitErr, ErrScopeUnconfirmed) {
		t.Fatalf("canceled worker did not release its full tree: released=%t error=%v output=%q", released, waitErr, output.String())
	}
	if elapsed := time.Since(started); elapsed > cleanupWait+graceWait+3*time.Second {
		t.Fatalf("canceled worker pipe drain exceeded cleanup bound: %v", elapsed)
	}
	waitCtx, stopWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopWait()
	for _, process := range []*pinnedProcess{leader, child} {
		if err := process.WaitExited(waitCtx); err != nil {
			t.Fatalf("command survived worker cancellation: %v", err)
		}
	}
	assertWorkerRemoved(t, h)
}

func TestWorkerLostRootPinRetainsUncertaintyAndExitCause(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	report := filepath.Join(t.TempDir(), "worker")
	cmd := workerHelperCommand(context.Background(), "exit-error", report, false)
	h, err := StartWorker(cmd, func(string) error { return nil })
	if h != nil {
		t.Cleanup(func() { _, _ = h.Wait() })
	}
	if err != nil || h == nil {
		t.Fatalf("start worker: %v", err)
	}
	_ = waitForReport(t, report)
	rootPin, err := os.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rootPin.Close() })
	leaf := filepath.Join(h.dir, supervisorLeaf)
	leafPin, err := os.Open(leaf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leafPin.Close() })
	// These independent live pins authorize only fixture cleanup after proof is deliberately lost.
	t.Cleanup(func() {
		for _, owned := range []struct {
			path string
			pin  *os.File
		}{{leaf, leafPin}, {h.dir, rootPin}} {
			pinned, pinErr := owned.pin.Stat()
			current, statErr := os.Stat(owned.path)
			if pinErr != nil || statErr != nil || !os.SameFile(pinned, current) {
				t.Errorf("independent fixture cleanup lost identity for %s: pin=%v stat=%v", owned.path, pinErr, statErr)
				return
			}
			if populated, err := cgroupPopulated(owned.path); err != nil || populated {
				t.Errorf("independent fixture scope is not empty: %s populated=%t error=%v", owned.path, populated, err)
				return
			}
			if err := os.Remove(owned.path); err != nil {
				t.Errorf("remove independently pinned fixture scope %s: %v", owned.path, err)
				return
			}
		}
	})
	stat, err := os.Open(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	worker := &pinnedProcess{stat: stat}
	defer worker.Close()
	waitCtx, stopWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopWait()
	if err := worker.WaitExited(waitCtx); err != nil {
		t.Fatalf("fault fixture worker still alive: %v", err)
	}
	if err := h.root.Close(); err != nil {
		t.Fatal(err)
	}
	released, waitErr := h.Wait()
	var exitErr *exec.ExitError
	if released || !errors.Is(waitErr, ErrScopeUnconfirmed) || !strings.Contains(waitErr.Error(), h.dir) || !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("lost pin falsely released or erased original exit: released=%t error=%v", released, waitErr)
	}
	if !errors.Is(waitErr, os.ErrClosed) && !errors.Is(waitErr, syscall.EBADF) {
		t.Fatalf("lost pin erased descriptor failure: %v", waitErr)
	}
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 23 {
		t.Fatalf("uncertain ownership skipped child reaping: %v", cmd.ProcessState)
	}
	if againReleased, againErr := h.Wait(); againReleased || againErr != waitErr {
		t.Fatalf("repeated uncertain Wait changed evidence: released=%t error=%v", againReleased, againErr)
	}
}

func assertWorkerRemoved(t *testing.T, h *WorkerHandle) {
	t.Helper()
	if _, err := os.Stat(h.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker lifetime subtree not removed: %s: %v", h.dir, err)
	}
}

func workerHelperCommand(ctx context.Context, mode, report string, holdPipe bool) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkerHelperProcess$")
	cmd.Env = append(os.Environ(), "MINION_WORKER_TEST_MODE="+mode, "MINION_WORKER_TEST_REPORT="+report,
		"MINION_WORKER_TEST_HOLD_PIPE="+strconv.FormatBool(holdPipe))
	return cmd
}

func TestWorkerHelperProcess(t *testing.T) {
	mode := os.Getenv("MINION_WORKER_TEST_MODE")
	if mode == "" {
		return
	}
	report := os.Getenv("MINION_WORKER_TEST_REPORT")
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	var supervisor string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::/") {
			supervisor = filepath.Join(cgroupMount, strings.TrimPrefix(line, "0::/"))
		}
	}
	if filepath.Base(supervisor) != supervisorLeaf {
		t.Fatalf("worker not placed in supervisor leaf: %q", supervisor)
	}
	if mode == "exit" || mode == "exit-error" {
		if err := workspace.WriteAtomic(report, []byte(supervisor)); err != nil {
			t.Fatal(err)
		}
		if mode == "exit-error" {
			os.Exit(23)
		}
		os.Exit(0)
	}
	if mode != "nested-clean" && mode != "crash-command" && mode != "block-command" {
		t.Fatalf("unknown worker fixture mode %q", mode)
	}
	if err := Configure(64, 512<<20); err != nil {
		t.Fatal(err)
	}
	if err := Check(); err != nil {
		t.Fatalf("nested readiness: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cmd *exec.Cmd
	if mode == "nested-clean" {
		if got := waitForReport(t, report+".recorded"); got != filepath.Dir(supervisor) {
			t.Fatalf("worker acted without its successful parent record: %q", got)
		}
		cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessgroupHelperProcess$")
		cmd.Env = append(os.Environ(), "MINION_PROCESSGROUP_TEST_MODE=limits-block", "MINION_PROCESSGROUP_TEST_PID_FILE="+report+".limits")
	} else {
		cmd = helperCommand("leader-block", report+".child.pid", os.Getenv("MINION_WORKER_TEST_HOLD_PIPE") == "true")
		if os.Getenv("MINION_WORKER_TEST_HOLD_PIPE") == "true" {
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		}
	}
	h, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteAtomic(report+".command.pid", []byte(strconv.Itoa(cmd.Process.Pid))); err != nil {
		t.Fatal(err)
	}
	if mode == "nested-clean" {
		_ = waitForReport(t, report+".limits")
	} else {
		_ = waitForReport(t, report+".child.pid")
	}
	if err := workspace.WriteAtomic(report, []byte(supervisor+"\n"+h.dir)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(report + ".release"); err == nil {
			if mode == "crash-command" {
				os.Exit(23)
			}
			if mode == "nested-clean" {
				cancel()
				if err := h.Wait(); err == nil || errors.Is(err, ErrUnconfirmed) || errors.Is(err, ErrScopeUnconfirmed) || errors.Is(err, ErrDescendants) {
					t.Fatalf("nested ordinary command cleanup: %v", err)
				}
				os.Exit(0)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker fixture was not released or canceled")
}
