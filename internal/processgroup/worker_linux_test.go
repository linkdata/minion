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
	if !released || !errors.Is(waitErr, refused) || errors.Is(waitErr, ErrScopeUnconfirmed) {
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
			if !errors.Is(startErr, wantCause) {
				t.Fatalf("failed exec lost its returned start cause: got %v want %v", startErr, wantCause)
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
			if len(paths) != 3 || paths[0] != filepath.Join(h.dir, supervisorLeaf) || filepath.Dir(paths[1]) != h.dir || filepath.Dir(paths[2]) != h.dir || paths[1] == paths[2] {
				t.Fatalf("crash helper did not create two owned ordinary scopes: %q", paths)
			}
			leader := waitForHelper(t, report+".command.pid")
			defer leader.Close()
			child := waitForHelper(t, report+".child.pid")
			defer child.Close()
			secondLeader := waitForHelper(t, report+".second.command.pid")
			defer secondLeader.Close()
			secondChild := waitForHelper(t, report+".second.child.pid")
			defer secondChild.Close()
			if err := workspace.WriteAtomic(report+".release", []byte("crash")); err != nil {
				t.Fatal(err)
			}
			released, waitErr := h.Wait()
			var exitErr *exec.ExitError
			if !released || !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 23 || !errors.Is(waitErr, ErrDescendants) || errors.Is(waitErr, ErrScopeUnconfirmed) || errors.Is(waitErr, ErrUnconfirmed) {
				t.Fatalf("crash lost exit cause or full release: released=%t error=%v output=%q", released, waitErr, output.String())
			}
			if againReleased, againErr := h.Wait(); !againReleased || againErr != waitErr {
				t.Fatalf("repeated crash Wait changed result: released=%t error=%v", againReleased, againErr)
			}
			waitCtx, stopWait := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopWait()
			for _, process := range []*pinnedProcess{leader, child, secondLeader, secondChild} {
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

func TestWorkerKillErrorStillAllowsEmptyRelease(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	for _, test := range []struct {
		name    string
		release bool
	}{{"survivor exits within proof bound", true}, {"survivor remains", false}} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			report := filepath.Join(t.TempDir(), "worker")
			cmd := workerHelperCommand(ctx, "kill-error", report, false)
			h, err := StartWorker(cmd, func(string) error { return nil })
			if h != nil {
				t.Cleanup(func() { cancel(); _, _ = h.Wait() })
			}
			if err != nil || h == nil {
				t.Fatalf("start worker: %v", err)
			}
			paths := strings.Split(waitForReport(t, report), "\n")
			if len(paths) != 2 || paths[0] != filepath.Join(h.dir, supervisorLeaf) || filepath.Dir(paths[1]) != h.dir {
				t.Fatalf("kill-error helper did not create an owned command scope: %q", paths)
			}
			survivor := waitForHelper(t, report+".survivor")
			defer survivor.Close()
			stat, err := os.Open(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
			if err != nil {
				t.Fatal(err)
			}
			worker := &pinnedProcess{stat: stat}
			defer worker.Close()
			waitCtx, stopWait := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopWait()
			if err := worker.WaitExited(waitCtx); err != nil {
				t.Fatalf("kill-error fixture worker still alive: %v", err)
			}
			var pins []*os.File
			for _, path := range []string{h.dir, paths[0], paths[1]} {
				pin, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = pin.Close() })
				pins = append(pins, pin)
			}
			killControl, err := os.OpenFile(filepath.Join(h.dir, "cgroup.kill"), os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = killControl.Close() })
			killInfo, err := killControl.Stat()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				pinned, pinErr := pins[0].Stat()
				current, statErr := os.Stat(h.dir)
				if errors.Is(statErr, os.ErrNotExist) {
					return
				}
				if pinErr != nil || statErr != nil || !os.SameFile(pinned, current) {
					t.Errorf("kill-error fixture lost root identity: pin=%v stat=%v", pinErr, statErr)
					return
				}
				if err := killControl.Chmod(killInfo.Mode().Perm()); err != nil {
					t.Errorf("restore independently owned kill permission: %v", err)
					return
				}
				if _, err := killControl.WriteString("1"); err != nil {
					t.Errorf("stop independently owned fixture tree: %v", err)
					return
				}
				if err := waitEmptyCgroup(h.dir); err != nil {
					t.Errorf("drain independently owned fixture tree: %v", err)
					return
				}
				cleanupPinnedWorkerFixture(t, pins[0], pins[1:]...)
			})
			if err := killControl.Chmod(0); err != nil {
				t.Fatal(err)
			}
			denied, denyErr := os.OpenFile(filepath.Join(h.dir, "cgroup.kill"), os.O_WRONLY, 0)
			if denyErr == nil {
				_ = denied.Close()
				if err := killControl.Chmod(killInfo.Mode().Perm()); err != nil {
					t.Fatal(err)
				}
				t.Skip("host privileges bypass the real kill-permission fault")
			}
			if !errors.Is(denyErr, syscall.EACCES) {
				t.Fatalf("fixture did not deny a new kill writer: %v", denyErr)
			}
			var gateDone chan struct{}
			var gateErr error
			if test.release {
				gateDone = make(chan struct{})
				// Permit exit after settling and within cleanupWait; EACCES below proves the failed kill actually occurred.
				timer := time.AfterFunc(time.Second, func() {
					gateErr = workspace.WriteAtomic(report+".survivor.release", []byte("exit"))
					close(gateDone)
				})
				t.Cleanup(func() {
					if !timer.Stop() {
						<-gateDone
					}
				})
			}
			released, waitErr := h.Wait()
			var exitErr *exec.ExitError
			if released != test.release || !errors.Is(waitErr, syscall.EACCES) || !errors.Is(waitErr, ErrUnconfirmed) || errors.Is(waitErr, ErrScopeUnconfirmed) == test.release || !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 23 {
				t.Errorf("failed kill lost cause or later release proof: released=%t error=%v", released, waitErr)
			}
			if againReleased, againErr := h.Wait(); againReleased != released || againErr != waitErr {
				t.Errorf("repeated kill-error Wait changed result: released=%t error=%v", againReleased, againErr)
			}
			if !test.release {
				if waitErr == nil || !strings.Contains(waitErr.Error(), h.dir) {
					t.Errorf("persistent survivor lost its owned reference: %v", waitErr)
				}
				if populated, err := cgroupPopulated(h.dir); err != nil || !populated {
					t.Errorf("persistent survivor fixture did not remain populated: populated=%t error=%v", populated, err)
				}
				return
			}
			if !errors.Is(waitErr, ErrDescendants) {
				t.Errorf("positive survivor observation lost classification: %v", waitErr)
			}
			<-gateDone
			if gateErr != nil {
				t.Errorf("release survivor fixture gate: %v", gateErr)
			}
			survivorCtx, stopSurvivorWait := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopSurvivorWait()
			if err := survivor.WaitExited(survivorCtx); err != nil {
				t.Errorf("gated survivor did not exit: %v", err)
			}
			if _, err := os.Stat(h.dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("positively released worker tree remains: %v", err)
			}
		})
	}
}

func TestWorkerEmptyUnwaitedCommandRejectsSuccess(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report := filepath.Join(t.TempDir(), "worker")
	cmd := workerHelperCommand(ctx, "empty-command", report, false)
	h, err := StartWorker(cmd, func(string) error { return nil })
	if h != nil {
		t.Cleanup(func() { cancel(); _, _ = h.Wait() })
	}
	if err != nil || h == nil {
		t.Fatalf("start worker: %v", err)
	}
	paths := strings.Split(waitForReport(t, report), "\n")
	if len(paths) != 3 || paths[0] != filepath.Join(h.dir, supervisorLeaf) || filepath.Dir(paths[1]) != h.dir || paths[2] != "23" {
		t.Fatalf("helper did not leave the reaped failing command scope: %q", paths)
	}
	if populated, err := cgroupPopulated(paths[1]); err != nil || populated {
		t.Fatalf("unwaited command scope was not empty: populated=%t error=%v", populated, err)
	}
	released, waitErr := h.Wait()
	if !released || !errors.Is(waitErr, ErrUnconfirmed) || errors.Is(waitErr, ErrScopeUnconfirmed) || errors.Is(waitErr, ErrDescendants) {
		t.Fatalf("empty unwaited command escaped rejection or release: released=%t error=%v", released, waitErr)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("fixture worker did not exit successfully: %v", cmd.ProcessState)
	}
	if !strings.Contains(waitErr.Error(), paths[1]) {
		t.Fatalf("unwaited command error lost its concrete scope: %v", waitErr)
	}
	if againReleased, againErr := h.Wait(); !againReleased || againErr != waitErr {
		t.Fatalf("repeated unwaited-command Wait changed result: released=%t error=%v", againReleased, againErr)
	}
	assertWorkerRemoved(t, h)
}

func TestWorkerReplacementPreservesForeignDirectory(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	for _, test := range []struct {
		name         string
		duringRecord bool
		refuseRecord bool
		root         bool
	}{
		{name: "during successful record", duringRecord: true},
		{name: "during refused record", duringRecord: true, refuseRecord: true},
		{name: "supervisor after worker exit"},
		{name: "root after worker exit", root: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			report := filepath.Join(t.TempDir(), "worker")
			cmd := workerHelperCommand(ctx, "exit-error", report, false)
			refused := errors.New("replacement fixture record refused")
			var rootPin, replacementPin *os.File
			replace := func(scope string) error {
				var err error
				rootPin, err = os.Open(scope)
				if err != nil {
					return err
				}
				t.Cleanup(func() { _ = rootPin.Close() })
				leaf := filepath.Join(scope, supervisorLeaf)
				original, err := os.Open(leaf)
				if err != nil {
					return err
				}
				t.Cleanup(func() { _ = original.Close() })
				if err := os.Remove(leaf); err != nil {
					return err
				}
				replacedPath, originalPin := leaf, original
				if test.root {
					if err := waitEmptyCgroup(scope); err != nil {
						return err
					}
					if err := os.Remove(scope); err != nil {
						return err
					}
					replacedPath, originalPin = scope, rootPin
				}
				if err := os.Mkdir(replacedPath, 0700); err != nil {
					return err
				}
				replacementPin, err = os.Open(replacedPath)
				if err != nil {
					return err
				}
				t.Cleanup(func() { _ = replacementPin.Close() })
				t.Cleanup(func() {
					if test.root {
						cleanupPinnedWorkerFixture(t, replacementPin)
					} else {
						cleanupPinnedWorkerFixture(t, rootPin, replacementPin)
					}
				})
				oldInfo, oldErr := originalPin.Stat()
				newInfo, newErr := replacementPin.Stat()
				if oldErr != nil || newErr != nil || os.SameFile(oldInfo, newInfo) {
					return fmt.Errorf("fixture failed to replace directory identity: old=%v new=%v", oldErr, newErr)
				}
				return nil
			}
			var fixtureErr error
			h, startErr := StartWorker(cmd, func(scope string) error {
				if test.duringRecord {
					fixtureErr = replace(scope)
				}
				if test.refuseRecord {
					return errors.Join(fixtureErr, refused)
				}
				return fixtureErr
			})
			if h != nil {
				t.Cleanup(func() { cancel(); _, _ = h.Wait() })
			}
			if fixtureErr != nil || h == nil {
				t.Fatalf("replacement fixture failed: setup=%v handle=%v start=%v", fixtureErr, h, startErr)
			}
			if test.duringRecord {
				if startErr == nil || cmd.Process != nil {
					t.Fatalf("replaced supervisor allowed child start: process=%v error=%v", cmd.Process, startErr)
				}
				if test.refuseRecord && !errors.Is(startErr, refused) {
					t.Fatalf("replacement erased recording refusal: %v", startErr)
				}
				if _, err := os.Stat(report); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("replaced supervisor worker acted: %v", err)
				}
			} else {
				if startErr != nil {
					t.Fatalf("start worker before replacement: %v", startErr)
				}
				_ = waitForReport(t, report)
				stat, err := os.Open(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
				if err != nil {
					t.Fatal(err)
				}
				worker := &pinnedProcess{stat: stat}
				defer worker.Close()
				waitCtx, stopWait := context.WithTimeout(context.Background(), 2*time.Second)
				defer stopWait()
				if err := worker.WaitExited(waitCtx); err != nil {
					t.Fatalf("replacement fixture worker still alive: %v", err)
				}
				if err := replace(h.dir); err != nil {
					t.Fatal(err)
				}
			}
			released, waitErr := h.Wait()
			if released || !errors.Is(waitErr, ErrScopeUnconfirmed) || !strings.Contains(waitErr.Error(), h.dir) {
				t.Errorf("worker replacement falsely released: released=%t error=%v", released, waitErr)
			}
			if waitErr == nil || !strings.Contains(waitErr.Error(), "worker directory identity changed") {
				t.Errorf("worker replacement lost identity failure: %v", waitErr)
			}
			if test.refuseRecord && !errors.Is(waitErr, refused) {
				t.Errorf("Wait erased recording refusal after replacement: %v", waitErr)
			}
			if !test.duringRecord {
				var exitErr *exec.ExitError
				if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 23 || cmd.ProcessState == nil {
					t.Errorf("replacement lost original worker exit or reaping: %v", waitErr)
				}
			}
			pinned, pinErr := replacementPin.Stat()
			current, statErr := os.Stat(replacementPin.Name())
			if pinErr != nil || statErr != nil || !os.SameFile(pinned, current) {
				t.Errorf("cleanup removed or replaced foreign directory: pin=%v stat=%v", pinErr, statErr)
			}
			if againReleased, againErr := h.Wait(); againReleased != released || againErr != waitErr {
				t.Errorf("repeated replacement Wait changed result: released=%t error=%v", againReleased, againErr)
			}
		})
	}
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
	t.Cleanup(func() { cleanupPinnedWorkerFixture(t, rootPin, leafPin) })
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
	var verificationErr *os.PathError
	if !errors.As(waitErr, &verificationErr) || verificationErr.Op != "stat" || !errors.Is(verificationErr, os.ErrClosed) {
		t.Fatalf("lost pin erased identity verification failure: %v", waitErr)
	}
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 23 {
		t.Fatalf("uncertain ownership skipped child reaping: %v", cmd.ProcessState)
	}
	if againReleased, againErr := h.Wait(); againReleased || againErr != waitErr {
		t.Fatalf("repeated uncertain Wait changed evidence: released=%t error=%v", againReleased, againErr)
	}
}

func cleanupPinnedWorkerFixture(t *testing.T, root *os.File, leaves ...*os.File) {
	t.Helper()
	for _, pin := range append(leaves, root) {
		pinned, pinErr := pin.Stat()
		current, statErr := os.Stat(pin.Name())
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if pinErr != nil || statErr != nil || !os.SameFile(pinned, current) {
			t.Errorf("independent fixture cleanup lost identity for %s: pin=%v stat=%v", pin.Name(), pinErr, statErr)
			return
		}
		if populated, err := cgroupPopulated(pin.Name()); err != nil || populated {
			t.Errorf("independent fixture scope is not empty: %s populated=%t error=%v", pin.Name(), populated, err)
			return
		}
		if err := os.Remove(pin.Name()); err != nil {
			t.Errorf("remove independently pinned fixture scope %s: %v", pin.Name(), err)
			return
		}
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
	if mode == "gated-survivor" {
		if err := workspace.WriteAtomic(report, []byte(strconv.Itoa(os.Getpid()))); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(report + ".release"); err == nil {
				os.Exit(0)
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("survivor fixture was not released")
	}
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
	if mode != "nested-clean" && mode != "crash-command" && mode != "block-command" && mode != "kill-error" && mode != "empty-command" {
		t.Fatalf("unknown worker fixture mode %q", mode)
	}
	if err := Configure(64, 512<<20); err != nil {
		t.Fatal(err)
	}
	if err := Check(); err != nil {
		t.Fatalf("nested readiness: %v", err)
	}
	if mode == "empty-command" {
		cmd := exec.Command("/bin/sh", "-c", "exit 23")
		command, err := Start(cmd)
		if err != nil {
			t.Fatal(err)
		}
		// Reap the fixture process while deliberately omitting Handle.Wait and its scope/limit checks.
		state, err := cmd.Process.Wait()
		if err != nil || state == nil || state.ExitCode() != 23 {
			t.Fatalf("fixture command did not fail as intended: state=%v error=%v", state, err)
		}
		if err := waitEmptyCgroup(command.dir); err != nil {
			t.Fatal(err)
		}
		if err := workspace.WriteAtomic(report, []byte(supervisor+"\n"+command.dir+"\n"+strconv.Itoa(state.ExitCode()))); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
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
	} else if mode == "kill-error" {
		cmd = workerHelperCommand(ctx, "gated-survivor", report+".survivor", false)
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
	} else if mode == "kill-error" {
		_ = waitForReport(t, report+".survivor")
	} else {
		_ = waitForReport(t, report+".child.pid")
	}
	paths := []string{supervisor, h.dir}
	if mode == "crash-command" {
		second := helperCommand("leader-block", report+".second.child.pid", os.Getenv("MINION_WORKER_TEST_HOLD_PIPE") == "true")
		if os.Getenv("MINION_WORKER_TEST_HOLD_PIPE") == "true" {
			second.Stdout, second.Stderr = os.Stdout, os.Stderr
		}
		secondHandle, err := Start(second)
		if err != nil {
			t.Fatal(err)
		}
		if err := workspace.WriteAtomic(report+".second.command.pid", []byte(strconv.Itoa(second.Process.Pid))); err != nil {
			t.Fatal(err)
		}
		_ = waitForReport(t, report+".second.child.pid")
		paths = append(paths, secondHandle.dir)
	}
	if err := workspace.WriteAtomic(report, []byte(strings.Join(paths, "\n"))); err != nil {
		t.Fatal(err)
	}
	if mode == "kill-error" {
		os.Exit(23)
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
