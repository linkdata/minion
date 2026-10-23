//go:build linux

package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/processgroup"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestUnixExecutionOwnedWorker(t *testing.T) {
	if err := processgroup.Check(); err != nil {
		if os.Getenv("MINION_REQUIRE_PROCESSGROUP") == "1" {
			t.Fatalf("required delegated process ownership unavailable: %v", err)
		}
		t.Skipf("delegated process ownership unavailable: %v", err)
	}
	base, err := os.MkdirTemp("/tmp", "minion-unix-")
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(base, "socket")
	grant := filepath.Join(base, "SKILL.md")
	var endpoint *UnixEndpoint
	var worker *processgroup.WorkerHandle
	var cancel context.CancelFunc
	closed, released := false, false
	defer func() {
		socketClosed := closed || endpoint == nil
		if endpoint != nil && !closed {
			if err := endpoint.Close(); err != nil {
				t.Errorf("retain unproved Unix socket in %s: %v", runtimeDir, err)
			} else {
				socketClosed = true
			}
		}
		if cancel != nil {
			cancel()
		}
		if worker != nil && !released {
			var waitErr error
			released, waitErr = worker.Wait()
			if waitErr != nil {
				t.Errorf("cleanup owned worker: released=%t error=%v", released, waitErr)
			}
		}
		if !socketClosed || worker != nil && !released {
			t.Logf("retaining runtime directory without proved socket closure and worker release: %s", runtimeDir)
			return
		}
		if err := os.Remove(runtimeDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("retain nonempty runtime directory %s: %v", runtimeDir, err)
			return
		}
		if err := os.Remove(grant); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove selected-grant fixture: %v", err)
			return
		}
		if err := os.Remove(base); err != nil {
			t.Errorf("remove empty runtime parent: %v", err)
		}
	}()
	if err := os.Mkdir(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grant, []byte("test grant\n"), 0600); err != nil {
		t.Fatal(err)
	}
	core, err := NewHTTPExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err = ListenUnixExecution(runtimeDir, core)
	if err != nil {
		t.Fatal(err)
	}

	// A selected model grant of the parent directory exposes the protected socket.
	roles := withClaudeCritic(testJob(command.Comment))
	selected := Runner{Codex: config.CodexConfig{Skills: []string{grant}}}
	if err := selected.CheckPrivatePaths([]string{runtimeDir, endpoint.SocketPath()}, roles.Main.Provider, roles.Critic.Provider); err == nil || !strings.Contains(err.Error(), "visible through model filesystem grant") {
		t.Fatalf("selected parent grant did not refuse readiness: %v", err)
	}
	selected.Codex.Skills = nil
	if err := selected.CheckPrivatePaths([]string{runtimeDir, endpoint.SocketPath()}, roles.Main.Provider, roles.Critic.Provider); err != nil {
		t.Fatalf("private socket refused with safe selected roles: %v", err)
	}

	ctx, stop := context.WithTimeout(context.Background(), 90*time.Second)
	cancel = stop
	report := filepath.Join(t.TempDir(), "worker")
	workRoot := t.TempDir()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnixExecutionWorkerHelper$")
	cmd.Env = append(os.Environ(), "MINION_UNIX_WORKER_HELPER=1", "MINION_UNIX_WORKER_SOCKET="+endpoint.SocketPath(),
		"MINION_UNIX_WORKER_REPORT="+report, "MINION_UNIX_WORKER_WORKROOT="+workRoot)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	var recorded string
	worker, startErr := processgroup.StartWorker(cmd, func(scope string) error {
		if cmd.Process != nil {
			return errors.New("worker started before ownership was recorded")
		}
		recorded = scope
		return workspace.WriteAtomic(report+".recorded", []byte(scope))
	})
	if startErr != nil || worker == nil || recorded == "" {
		t.Fatalf("start owned Unix worker: handle=%v scope=%q error=%v", worker, recorded, startErr)
	}
	if data, err := os.ReadFile(report + ".recorded"); err != nil || string(data) != recorded {
		t.Fatalf("worker ownership record was not durable: data=%q scope=%q error=%v", data, recorded, err)
	}
	if pid := unixWorkerReport(t, ctx, report, ".ready"); pid != strconv.Itoa(cmd.Process.Pid) {
		t.Fatalf("ready worker PID = %q; want %d", pid, cmd.Process.Pid)
	}

	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	tokens := usage.Tokens{Input: 9, CachedInput: 2, Output: 4}
	for index, digit := range []string{"a", "b"} {
		job := withClaudeCritic(testJob(command.Comment))
		job.Identity.JobID = strings.Repeat(strconv.Itoa(index+1), 64)
		fixture := newExecutionAdapterFixture(t, job)
		fixture.owner.attemptID = strings.Repeat(digit, 32)
		fixture.job.Identity.AttemptID = fixture.owner.attemptID
		fixture.sink = newExecutionCheckpoints(fixture.owner, fixture.job)
		fixture.owner.Usage = store
		tracked := usageJob()
		tracked.ID = fixture.job.Identity.JobID
		tracked.Repository.Name = fixture.job.Identity.Repository
		tracked.Source.Author.ID = fixture.job.Identity.RequesterID
		tracked.Source.Author.Login = fixture.job.Identity.Requester
		tracked.Command = command.Command{Action: command.Comment}
		finishUsage, err := fixture.owner.trackUsage(tracked)
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			if err := os.WriteFile(report+".second-go", []byte("go"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		type exchange struct {
			result Result
			err    error
		}
		done := make(chan exchange, 1)
		go func() {
			result, err := core.Execute(ctx, fixture.job, fixture.sink)
			done <- exchange{result, err}
		}()
		var got exchange
		select {
		case got = <-done:
		case <-ctx.Done():
			t.Fatalf("assignment %d did not complete: %v", index+1, ctx.Err())
		}
		finalErr := fixture.sink.finish(got.result, got.err)
		wantCalls := []CallUsage{
			{Provider: "codex", Role: roleMain, Round: 1, Tokens: tokens, Complete: true},
			{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Tokens: tokens, Approved: true, NoBlockers: true, ReviewComplete: true, Complete: true},
		}
		if finalErr = finishUsage(finalErr); finalErr != nil || got.result.Validate(fixture.job) != nil || got.result.Answer != "Worker answer" || got.result.CriticStopReason != "approved" || !slices.Equal(got.result.Calls, wantCalls) || !slices.Equal(got.result.Calls, fixture.sink.calls) {
			t.Fatalf("assignment %d lost durable receipts: result=%+v raw=%v final=%v", index+1, got.result, got.err, finalErr)
		}
		private := readPrivateReview(t, fixture.owner.WorkRoot)
		if private.Identity != fixture.job.Identity || private.CriticStopReason != "approved" || !slices.Equal(private.Calls, got.result.Calls) || len(private.Refutations) != 0 {
			t.Fatalf("assignment %d private ledger changed: %+v", index+1, private)
		}
		marker := ".first"
		if index == 1 {
			marker = ".second"
		}
		if got := unixWorkerReport(t, ctx, report, marker); got != fmt.Sprintf("%d:%d", cmd.Process.Pid, (index+1)*2) {
			t.Fatalf("assignment %d ran in another worker or lost native calls: %q", index+1, got)
		}
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 2 {
		t.Fatalf("durable usage records: %+v, %v", records, err)
	}
	for index, record := range records {
		if record.ID != strings.Repeat([]string{"a", "b"}[index], 32) || record.JobID != strings.Repeat(strconv.Itoa(index+1), 64) || record.Status != "succeeded" || record.CodexCalls != 1 || record.ClaudeCalls != 1 || record.Tokens != (usage.Tokens{Input: 2 * tokens.Input, CachedInput: 2 * tokens.CachedInput, Output: 2 * tokens.Output}) || !record.UsageComplete {
			t.Fatalf("assignment %d usage checkpoint changed: %+v", index+1, record)
		}
	}
	if err := endpoint.Close(); err != nil {
		t.Fatalf("drain endpoint before stopping worker: %v", err)
	}
	closed = true
	if err := os.WriteFile(report+".release", []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	released, waitErr := worker.Wait()
	if !released || waitErr != nil || cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("worker scope or nested commands survived: released=%t error=%v state=%v output=%s", released, waitErr, cmd.ProcessState, output.String())
	}
	if entries, err := os.ReadDir(workRoot); err != nil || len(entries) != 0 {
		t.Fatalf("worker work root was not cleaned: entries=%v error=%v", entries, err)
	}
}

func TestUnixExecutionWorkerHelper(t *testing.T) {
	if os.Getenv("MINION_UNIX_WORKER_HELPER") != "1" {
		return
	}
	report := os.Getenv("MINION_UNIX_WORKER_REPORT")
	defer func() {
		if t.Failed() {
			_ = os.WriteFile(report+".failed", []byte("worker helper failed"), 0600)
		}
	}()
	if data, err := os.ReadFile(report + ".recorded"); err != nil || len(data) == 0 {
		t.Fatalf("helper acted before parent ownership record: data=%q error=%v", data, err)
	}
	if err := processgroup.Configure(64, 512<<20); err != nil {
		t.Fatal(err)
	}
	if err := processgroup.Check(); err != nil {
		t.Fatalf("nested worker placement unavailable: %v", err)
	}
	if err := processgroup.Run(exec.Command("/bin/true")); err != nil {
		t.Fatalf("ordinary nested command was not released: %v", err)
	}
	if err := workspace.WriteAtomic(report+".ready", []byte(strconv.Itoa(os.Getpid()))); err != nil {
		t.Fatal(err)
	}
	client, err := NewUnixExecutionClient(os.Getenv("MINION_UNIX_WORKER_SOCKET"))
	if err != nil {
		t.Fatal(err)
	}
	nativeCalls := 0
	runner := &Runner{WorkRoot: os.Getenv("MINION_UNIX_WORKER_WORKROOT"), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		nativeCalls++
		answer := proposalJSON("Worker answer", "")
		if request.role == roleCritic {
			answer = approvedCritiqueJSON
		} else if request.role != roleMain {
			return aicli.Result{}, fmt.Errorf("unexpected model role %q", request.role)
		}
		return aicli.Result{Answer: answer, Tokens: usage.Tokens{Input: 9, CachedInput: 2, Output: 4}, UsageComplete: true}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	for index := 0; index < 2; index++ {
		if index == 1 {
			unixWorkerReport(t, ctx, report, ".second-go")
		}
		if err := RunHTTP(ctx, client, UnixExecutionBaseURL, "local", runner); err != nil {
			t.Fatalf("worker assignment %d: %v", index+1, err)
		}
		marker := ".first"
		if index == 1 {
			marker = ".second"
		}
		if err := workspace.WriteAtomic(report+marker, []byte(fmt.Sprintf("%d:%d", os.Getpid(), nativeCalls))); err != nil {
			t.Fatal(err)
		}
	}
	client.CloseIdleConnections()
	unixWorkerReport(t, ctx, report, ".release")
}

func unixWorkerReport(t *testing.T, ctx context.Context, report, suffix string) string {
	t.Helper()
	path := report + suffix
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read worker marker %s: %v", path, err)
		}
		if _, err := os.Stat(report + ".failed"); err == nil {
			t.Fatalf("worker failed before %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read worker failure marker: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait worker marker %s: %v", path, ctx.Err())
		case <-ticker.C:
		}
	}
}
