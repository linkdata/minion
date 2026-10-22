//go:build linux

package processgroup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/workspace"
)

func TestCgroupReadinessRequired(t *testing.T) {
	if os.Getenv("MINION_REQUIRE_PROCESSGROUP") != "1" {
		t.Skip("CI requires a delegated service")
	}
	if err := Check(); err != nil {
		t.Fatal(err)
	}
}

func TestCgroupCheckWithTightConfiguredLimits(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	setTestLimits(t, 1, 1<<20)
	if err := Check(); err != nil {
		t.Fatalf("readiness probe rejected verified tight limits: %v", err)
	}
}

func TestCgroupLimitEventAttribution(t *testing.T) {
	tests := []struct {
		name, pids, memory, swap       string
		wantExhausted, wantUnconfirmed bool
	}{
		{"none", "max 0\n", "max 0\noom 0\noom_kill 0\noom_group_kill 0\n", "max 0\n", false, false},
		{"pids", "max 1\n", "max 0\noom 0\noom_kill 0\noom_group_kill 0\n", "max 0\n", true, false},
		{"memory", "max 0\n", "max 1\noom 1\noom_kill 1\noom_group_kill 0\n", "max 0\n", true, false},
		{"swap", "max 0\n", "max 0\noom 0\noom_kill 0\noom_group_kill 0\n", "max 1\n", true, false},
		{"external OOM", "max 0\n", "max 0\noom 0\noom_kill 1\noom_group_kill 0\n", "max 0\n", false, true},
		{"missing swap counter", "max 0\n", "max 0\noom 0\noom_kill 0\noom_group_kill 0\n", "fail 0\n", false, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, data := range map[string]string{"pids.events": test.pids, "memory.events": test.memory, "memory.swap.events": test.swap} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := readExhaustion(dir, limits{tasks: 4, memory: 64 << 20})
			if errors.Is(err, ErrExhausted) != test.wantExhausted || errors.Is(err, ErrUnconfirmed) != test.wantUnconfirmed {
				t.Fatalf("unexpected event result: %v", err)
			}
			if test.name == "swap" {
				var event *Exhaustion
				if !errors.As(err, &event) || event.MemorySwapEventsMax != 1 {
					t.Fatalf("swap exhaustion not recorded: %v", err)
				}
			}
		})
	}
}

func TestUnreleasedScopeKeepsRecoveryReference(t *testing.T) {
	dir := t.TempDir()
	err := unreleasedScope(dir, ErrUnconfirmed)
	if !errors.Is(err, ErrScopeUnconfirmed) || !strings.Contains(err.Error(), dir) {
		t.Fatalf("unreleased scope lost recovery reference: %v", err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := unreleasedScope(dir, ErrUnconfirmed); errors.Is(err, ErrScopeUnconfirmed) {
		t.Fatalf("released scope remained latched: %v", err)
	}
}

func TestCgroupRunSuccess(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	for i := 0; i < 5; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessgroupHelperProcess$")
		cmd.WaitDelay = 10 * time.Second
		if err := Run(cmd); err != nil {
			t.Fatalf("clean command %d: %v", i, err)
		}
		if cmd.WaitDelay != 2*time.Second {
			t.Fatalf("pipe drain not bounded: %v", cmd.WaitDelay)
		}
	}
}

func TestCgroupCommandStopsDescendants(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	for _, holdPipe := range []bool{false, true} {
		name := "redirected"
		if holdPipe {
			name = "inherited pipe"
		}
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			cmd := helperCommand("leader", pidFile, holdPipe)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			h, err := Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Wait() })
			child := waitForHelper(t, pidFile)
			defer child.Close()
			started := time.Now()
			err = h.Wait()
			if !errors.Is(err, ErrDescendants) || errors.Is(err, ErrUnconfirmed) {
				t.Fatalf("expected confirmed descendant stop, got %v (output %q)", err, output.String())
			}
			if time.Since(started) > 6*time.Second {
				t.Fatalf("Wait took %v", time.Since(started))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := child.WaitExited(ctx); err != nil {
				t.Fatalf("detached child survived: %v", err)
			}
			if _, err := os.Stat(h.dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cgroup not removed: %v", err)
			}
		})
	}
}

func TestCgroupCommandContextCancellation(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessgroupHelperProcess$")
	cmd.Env = append(os.Environ(), "MINION_PROCESSGROUP_TEST_MODE=leader-block", "MINION_PROCESSGROUP_TEST_PID_FILE="+pidFile)
	h, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = h.Wait() })
	child := waitForHelper(t, pidFile)
	defer child.Close()
	started := time.Now()
	cancel()
	err = h.Wait()
	if err == nil || errors.Is(err, ErrUnconfirmed) || errors.Is(err, ErrDescendants) {
		t.Fatalf("cancellation not confirmed: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("forced cancellation exceeded bound: %v", time.Since(started))
	}
	waitCtx, stopWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopWait()
	if err := child.WaitExited(waitCtx); err != nil {
		t.Fatalf("detached child survived cancellation: %v", err)
	}
	if _, err := os.Stat(h.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cgroup not removed: %v", err)
	}
}

func TestCgroupStartFailureRemovesGroup(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	before := testCgroups(t)
	invalid := filepath.Join(t.TempDir(), "invalid-executable")
	if err := os.WriteFile(invalid, []byte("not an executable format\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "missing-executable"), invalid} {
		cmd := exec.Command(path)
		h, err := Start(cmd)
		if err == nil || h != nil || errors.Is(err, ErrUnconfirmed) {
			t.Fatalf("start failure for %s: handle=%v err=%v", path, h, err)
		}
	}
	after := testCgroups(t)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("start failure leaked a cgroup: before=%v after=%v", before, after)
	}
}

func TestCgroupLimitsBeforeActionAndGracefulCancellation(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	setTestLimits(t, 64, 512<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report := filepath.Join(t.TempDir(), "limits")
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessgroupHelperProcess$")
	cmd.Env = append(os.Environ(), "MINION_PROCESSGROUP_TEST_MODE=limits-block", "MINION_PROCESSGROUP_TEST_PID_FILE="+report)
	h, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = h.Wait() })
	got := strings.Split(strings.TrimSpace(waitForReport(t, report)), "\n")
	want := []string{strings.TrimPrefix(h.dir, cgroupMount), "64", strconv.FormatInt(512<<20, 10), "0", "1", "0"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("child acted without configured cgroup limits: got %q, want %q", got, want)
	}
	nested := filepath.Join(h.dir, "nested")
	if err := os.Mkdir(nested, 0700); err == nil {
		_ = os.Remove(nested)
		t.Fatal("command could create a nested cgroup despite cgroup.max.descendants=0")
	}
	started := time.Now()
	cancel()
	if err := h.Wait(); err == nil || errors.Is(err, ErrUnconfirmed) || errors.Is(err, ErrExhausted) || errors.Is(err, ErrDescendants) {
		t.Fatalf("graceful cancellation was not confirmed: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("graceful cancellation exceeded bound: %v", time.Since(started))
	}
	if got := waitForReport(t, report+".term"); got != "term" {
		t.Fatalf("helper did not acknowledge SIGTERM: %q", got)
	}
	if _, err := os.Stat(h.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cgroup not removed after cancellation: %v", err)
	}
}

func TestCgroupPIDLimitReportsExhaustion(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	setTestLimits(t, 1, 512<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "/bin/sleep 1 & wait")
	h, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	err = h.Wait()
	var event *Exhaustion
	if !errors.Is(err, ErrExhausted) || !errors.As(err, &event) || event.PIDEventsMax == 0 || event.MaxTasks != 1 || errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("PID ceiling was not reported as confirmed exhaustion: %v", err)
	}
	if _, err := os.Stat(h.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cgroup not removed after PID exhaustion: %v", err)
	}
}

func TestCgroupWrongTopologyFailsClosed(t *testing.T) {
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	parent, err := delegatedParent()
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := os.MkdirTemp(parent, "minion-test-wrong-leaf-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Remove(wrong); err != nil {
			t.Errorf("remove wrong-topology cgroup: %v", err)
		}
	}()
	group, err := os.Open(wrong)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	cmd := helperCommand("reject-topology", "", false)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(group.Fd())}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrong topology was accepted or not checked: %v: %s", err, output)
	}
}

// This probe is opt-in so a caller can place the entire test under independent
// systemd MemoryMax and RuntimeMaxSec bounds before deliberately causing OOM.
func TestCgroupMemoryLimitReportsOOM(t *testing.T) {
	if os.Getenv("MINION_PROCESSGROUP_LIVE_OOM") != "1" {
		t.Skip("requires an independently bounded delegated systemd service")
	}
	if err := Check(); err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	setTestLimits(t, 64, 96<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessgroupHelperProcess$")
	cmd.Env = append(os.Environ(), "MINION_PROCESSGROUP_TEST_MODE=memory-exhaust")
	h, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	err = h.Wait()
	var event *Exhaustion
	if !errors.Is(err, ErrExhausted) || !errors.As(err, &event) || event.MemoryOOMKill == 0 || event.MaxMemoryBytes != 96<<20 || errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("memory OOM was not reported as confirmed exhaustion: %v", err)
	}
	if _, err := os.Stat(h.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cgroup not removed after memory OOM: %v", err)
	}
}

func setTestLimits(t *testing.T, tasks int, memory int64) {
	t.Helper()
	previous := configuredLimits()
	if err := Configure(tasks, memory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := Configure(previous.tasks, previous.memory); err != nil {
			t.Error(err)
		}
	})
}

func waitForReport(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper failed to publish %s", path)
	return ""
}

func helperCommand(mode, pidFile string, holdPipe bool) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessgroupHelperProcess$")
	cmd.Env = append(os.Environ(), "MINION_PROCESSGROUP_TEST_MODE="+mode, "MINION_PROCESSGROUP_TEST_PID_FILE="+pidFile,
		"MINION_PROCESSGROUP_TEST_HOLD_PIPE="+strconv.FormatBool(holdPipe))
	return cmd
}

type pinnedProcess struct{ stat *os.File }

func (p *pinnedProcess) Close() error { return p.stat.Close() }

func (p *pinnedProcess) WaitExited(ctx context.Context) error {
	buf := make([]byte, 4096)
	for {
		n, err := p.stat.ReadAt(buf, 0)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		state := string(buf[:n])
		if end := strings.LastIndex(state, ") "); end >= 0 && strings.HasPrefix(state[end+2:], "Z ") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func waitForHelper(t *testing.T, pidFile string) *pinnedProcess {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil || pid <= 0 {
				t.Fatalf("invalid helper PID %q: %v", data, parseErr)
			}
			stat, openErr := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
			if openErr != nil {
				t.Fatal(openErr)
			}
			return &pinnedProcess{stat: stat}
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper failed to publish child PID at %s", pidFile)
	return nil
}

func testCgroups(t *testing.T) []string {
	t.Helper()
	parent, err := delegatedParent()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var groups []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), cgroupNamePrefix()) {
			groups = append(groups, entry.Name())
		}
	}
	return groups
}

func TestProcessgroupHelperProcess(t *testing.T) {
	mode := os.Getenv("MINION_PROCESSGROUP_TEST_MODE")
	if mode == "" {
		return
	}
	pidFile := os.Getenv("MINION_PROCESSGROUP_TEST_PID_FILE")
	switch mode {
	case "leader", "leader-block":
		child := helperCommand("child", pidFile, os.Getenv("MINION_PROCESSGROUP_TEST_HOLD_PIPE") == "true")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if os.Getenv("MINION_PROCESSGROUP_TEST_HOLD_PIPE") == "true" {
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
		} else {
			devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				os.Exit(2)
			}
			child.Stdout, child.Stderr = devNull, devNull
		}
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		_ = child.Process.Release()
		if mode == "leader-block" {
			time.Sleep(20 * time.Second)
		}
		os.Exit(0)
	case "child":
		if err := workspace.WriteAtomic(pidFile, []byte(strconv.Itoa(os.Getpid()))); err != nil {
			os.Exit(2)
		}
		time.Sleep(20 * time.Second)
		os.Exit(0)
	case "limits-block":
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		data, err := os.ReadFile("/proc/self/cgroup")
		if err != nil {
			os.Exit(2)
		}
		var current string
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "0::") {
				current = strings.TrimPrefix(line, "0::")
				break
			}
		}
		if !filepath.IsAbs(current) {
			os.Exit(2)
		}
		dir := filepath.Join(cgroupMount, strings.TrimPrefix(current, "/"))
		values := []string{current}
		for _, name := range []string{"pids.max", "memory.max", "memory.swap.max", "memory.oom.group", "cgroup.max.descendants"} {
			value, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				os.Exit(2)
			}
			values = append(values, strings.TrimSpace(string(value)))
		}
		if err := workspace.WriteAtomic(pidFile, []byte(strings.Join(values, "\n"))); err != nil {
			os.Exit(2)
		}
		select {
		case <-term:
			if err := workspace.WriteAtomic(pidFile+".term", []byte("term")); err != nil {
				os.Exit(2)
			}
			os.Exit(0)
		case <-time.After(20 * time.Second):
			os.Exit(2)
		}
	case "reject-topology":
		if err := Check(); !errors.Is(err, ErrUnconfirmed) {
			os.Exit(2)
		}
		if h, err := Start(exec.Command("/bin/true")); h != nil || !errors.Is(err, ErrUnconfirmed) {
			os.Exit(2)
		}
		os.Exit(0)
	case "memory-exhaust":
		block := make([]byte, 256<<20)
		for offset := 0; offset < len(block); offset += 4096 {
			block[offset] = 1
		}
		runtime.KeepAlive(block)
		os.Exit(0)
	default:
		os.Exit(2)
	}
}
