//go:build linux

package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestUnixExecutionDirectWorker(t *testing.T) {
	tempRoot, err := workspace.CanonicalPath(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name, err := skills.Name(testReviewSkillURL)
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(tempRoot, "minion-unix-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Errorf("remove Unix worker fixture %s: %v", base, err)
		}
	})
	skillDir := filepath.Join(base, ".agents", "skills", name)
	runtimeDir := filepath.Join(skillDir, "socket")
	grant := filepath.Join(skillDir, "SKILL.md")
	report := filepath.Join(base, "worker")
	workRoot := filepath.Join(base, "work")
	var endpoint *UnixEndpoint
	var core *HTTPExecutor
	var cmd *exec.Cmd
	var cancel context.CancelFunc
	var output bytes.Buffer
	closed, waited := false, false
	defer func() {
		if core != nil {
			_ = core.Close()
		}
		if cancel != nil {
			cancel()
		}
		if cmd != nil && !waited {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
			waited = true
		}
		if endpoint != nil && !closed {
			if err := endpoint.Close(); err != nil {
				t.Errorf("close Unix socket in %s: %v", runtimeDir, err)
			} else {
				closed = true
			}
		}
		if waited && t.Failed() {
			t.Logf("direct worker output: %s", output.String())
		}
		socketClosed := closed || endpoint == nil
		if !socketClosed {
			t.Logf("socket close failed; fixture cleanup will remove %s", runtimeDir)
			return
		}
		if err := os.Remove(runtimeDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove runtime directory %s: %v", runtimeDir, err)
			return
		}
		for _, path := range []string{report + ".ready", report + ".first", report + ".second", report + ".second-go", report + ".third-go", report + ".cancel-seen", report + ".third", report + ".release", report + ".failed", workRoot, grant, filepath.Join(skillDir, ".minion-install.json"), skillDir, filepath.Join(base, ".agents", "skills"), filepath.Join(base, ".agents")} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove owned path %s: %v", path, err)
				return
			}
		}
		for mismatch := range unixWorkerSelectionMismatches(withClaudeCritic(testJob(command.Comment))) {
			if err := os.Remove(report + fmt.Sprintf(".mismatch-%d", mismatch)); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove mismatch marker %d: %v", mismatch, err)
				return
			}
		}
		if err := os.Remove(base); err != nil {
			t.Errorf("remove empty runtime parent: %v", err)
		}
	}()
	t.Setenv("HOME", base)
	writeActionTestSkills(t, base, ".agents", testReviewSkillURL)
	if err := os.Mkdir(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workRoot, 0700); err != nil {
		t.Fatal(err)
	}
	core, err = NewHTTPExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err = ListenUnixExecution(runtimeDir, core)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 90*time.Second)
	cancel = stop
	cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnixExecutionWorkerHelper$")
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "MINION_UNIX_WORKER_HELPER=1", "MINION_UNIX_WORKER_SOCKET="+endpoint.SocketPath(),
		"MINION_UNIX_WORKER_REPORT="+report, "MINION_UNIX_WORKER_WORKROOT="+workRoot)
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start Unix worker: %v", err)
	}
	if pid := unixWorkerReport(t, ctx, report, ".ready"); pid != strconv.Itoa(cmd.Process.Pid) {
		t.Fatalf("ready worker PID = %q; want %d", pid, cmd.Process.Pid)
	}

	store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
	tokens := usage.Tokens{Input: 9, CachedInput: 2, Output: 4}
	awaitResult := func(done <-chan httpServerTestResult, label string) httpServerTestResult {
		t.Helper()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case got := <-done:
				return got
			case <-ctx.Done():
				t.Fatalf("%s did not complete: %v", label, ctx.Err())
			case <-ticker.C:
				if _, err := os.Stat(report + ".failed"); err == nil {
					t.Fatalf("worker failed before %s completed; output follows after worker release", label)
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("inspect worker failure marker: %v", err)
				}
			}
		}
	}
	for index, digit := range []string{"a", "b"} {
		job := withClaudeCritic(testJob(command.Comment))
		job.Main.Model, job.Main.Effort = "main-1", "high"
		job.Critic.Model, job.Critic.Effort = "critic-1", "medium"
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
		done := make(chan httpServerTestResult, 1)
		go func() {
			result, err := core.Execute(ctx, fixture.job, fixture.sink)
			done <- httpServerTestResult{result, err}
		}()
		got := awaitResult(done, fmt.Sprintf("assignment %d", index+1))
		finalErr := fixture.sink.finish(got.result, got.err)
		wantCalls := []CallUsage{
			{Provider: "codex", Role: roleMain, Round: 1, Model: job.Main.Model, Effort: job.Main.Effort, Tokens: tokens, Complete: true},
			{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Model: job.Critic.Model, Effort: job.Critic.Effort, Tokens: tokens, Approved: true, NoBlockers: true, ReviewComplete: true, Complete: true},
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
		if index == 0 {
			if err := os.WriteFile(report+".second-go", []byte("go"), 0600); err != nil {
				t.Fatal(err)
			}
			for mismatch, rejected := range unixWorkerSelectionMismatches(job) {
				rejected.Identity.JobID = fmt.Sprintf("mismatch-%d", mismatch)
				rejected.Identity.AttemptID = fmt.Sprintf("mismatch-%d", mismatch)
				if err := rejected.Validate(); err != nil {
					t.Fatalf("invalid mismatch fixture %d: %v", mismatch, err)
				}
				result, err := core.Execute(ctx, rejected, runnerExecutionCheckpoints{})
				var workerFailure *workerFailureError
				if !errors.As(err, &workerFailure) || !workerFailure.failure.Terminal || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !result.matchesJob(rejected) || len(result.Calls) != 0 {
					t.Fatalf("mismatch %d was not a known no-call failure: result=%+v error=%v", mismatch, result, err)
				}
				if got := unixWorkerReport(t, ctx, report, fmt.Sprintf(".mismatch-%d", mismatch)); got != fmt.Sprintf("%d:2", cmd.Process.Pid) {
					t.Fatalf("mismatch %d changed child or model call count: %q", mismatch, got)
				}
			}
		}
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 2 {
		t.Fatalf("durable usage records: %+v, %v", records, err)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	for index, record := range records {
		if record.ID != strings.Repeat([]string{"a", "b"}[index], 32) || record.JobID != strings.Repeat(strconv.Itoa(index+1), 64) || record.Status != "succeeded" || record.CodexCalls != 1 || record.ClaudeCalls != 1 || record.Tokens != (usage.Tokens{Input: 2 * tokens.Input, CachedInput: 2 * tokens.CachedInput, Output: 2 * tokens.Output}) || !record.UsageComplete {
			t.Fatalf("assignment %d usage checkpoint changed: %+v", index+1, record)
		}
	}
	thirdJob := withClaudeCritic(testJob(command.Comment))
	thirdJob.Main.Model, thirdJob.Main.Effort = "main-1", "high"
	thirdJob.Critic.Model, thirdJob.Critic.Effort = "critic-1", "medium"
	thirdJob.Identity.JobID = strings.Repeat("3", 64)
	third := newExecutionAdapterFixture(t, thirdJob)
	third.owner.attemptID = strings.Repeat("c", 32)
	third.job.Identity.AttemptID = third.owner.attemptID
	third.sink = newExecutionCheckpoints(third.owner, third.job)
	third.owner.Usage = store
	tracked := usageJob()
	tracked.ID = third.job.Identity.JobID
	tracked.Repository.Name = third.job.Identity.Repository
	tracked.Source.Author.ID = third.job.Identity.RequesterID
	tracked.Source.Author.Login = third.job.Identity.Requester
	tracked.Command = command.Command{Action: command.Comment}
	finishUsage, err := third.owner.trackUsage(tracked)
	if err != nil {
		t.Fatal(err)
	}
	saved, release := make(chan struct{}), make(chan struct{})
	gate := runnerExecutionCheckpoints{before: third.sink.BeforeCall, review: third.sink.Review, after: func(fact CallCheckpoint) error {
		if err := third.sink.AfterCall(fact); err != nil {
			return err
		}
		close(saved)
		<-release
		return nil
	}}
	gateReleased := false
	defer func() {
		if !gateReleased {
			close(release)
		}
	}()
	if err := os.WriteFile(report+".third-go", []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	thirdDone := make(chan httpServerTestResult, 1)
	go func() {
		result, err := core.Execute(ctx, third.job, gate)
		thirdDone <- httpServerTestResult{result, err}
	}()
	select {
	case <-saved:
	case <-ctx.Done():
		t.Fatalf("paid main receipt was not saved: %v", ctx.Err())
	}
	busyJob := third.job
	busyJob.Identity.JobID, busyJob.Identity.AttemptID = strings.Repeat("4", 64), strings.Repeat("d", 32)
	if result, err := core.Execute(ctx, busyJob, runnerExecutionCheckpoints{}); err == nil || !strings.Contains(err.Error(), "busy") || !result.matchesJob(busyJob) {
		t.Fatalf("active Unix worker admitted a second Job: result=%+v error=%v", result, err)
	}
	probe, err := NewUnixExecutionClient(endpoint.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer probe.CloseIdleConnections()
	var rejection httpAcknowledgement
	status, err := unixTestJSON(t, probe, "/next", httpPull{Version: ProtocolVersion, ExecutorID: "local"}, &rejection, httpControlBytes)
	if err != nil || status != http.StatusConflict || rejection.Kind != "next" || rejection.Failure == nil {
		t.Fatalf("active Unix assignment was delivered twice: status=%d ack=%+v error=%v", status, rejection, err)
	}
	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		close(closeStarted)
		closeDone <- core.Close()
	}()
	<-closeStarted
	if witness := unixWorkerReport(t, ctx, report, ".cancel-seen"); witness != context.Canceled.Error() || ctx.Err() != nil {
		t.Fatalf("worker native context cancellation before paid ACK: marker=%q parent=%v", witness, ctx.Err())
	}
	select {
	case err := <-closeDone:
		t.Fatalf("endpoint closed before entered paid ACK drained: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	gateReleased = true
	got := awaitResult(thirdDone, "canceled paid assignment")
	wantMain := CallUsage{Provider: "codex", Role: roleMain, Round: 1, Model: third.job.Main.Model, Effort: third.job.Main.Effort, Tokens: tokens, Complete: true}
	if !errors.Is(got.err, context.Canceled) || errors.Is(got.err, context.DeadlineExceeded) || errors.Is(got.err, monitor.ErrTerminalJob) || errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !got.result.matchesJob(third.job) || !slices.Equal(got.result.Calls, []CallUsage{wantMain}) || got.result.Answer != "" || got.result.CriticStopReason != "" || len(got.result.Refutations) != 0 {
		t.Fatalf("canceled paid assignment lost its known raw main receipt: result=%+v error=%v", got.result, got.err)
	}
	finalErr := third.sink.finish(got.result, got.err)
	if finalErr = finishUsage(finalErr); !errors.Is(finalErr, context.Canceled) || errors.Is(finalErr, monitor.ErrTerminalJob) {
		t.Fatalf("canceled paid assignment failed durable completion: %v", finalErr)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("drain core after paid ACK: %v", err)
	}
	if _, err := os.Lstat(endpoint.SocketPath()); err != nil {
		t.Fatalf("core drain removed socket before worker release: %v", err)
	}
	if got := unixWorkerReport(t, ctx, report, ".third"); got != fmt.Sprintf("%d:5", cmd.Process.Pid) {
		t.Fatalf("canceled assignment ran in another worker or invoked critic: %q", got)
	}
	private := readPrivateReview(t, third.owner.WorkRoot)
	if private.Identity != third.job.Identity || private.SourceRef != third.job.Source.Head || len(private.Calls) != 0 || len(private.Refutations) != 0 || private.CriticStopReason != "" {
		t.Fatalf("canceled assignment changed initial private review ledger: %+v", private)
	}
	records, err = usage.ReadAll(store.Dir)
	if err != nil || len(records) != 3 {
		t.Fatalf("canceled assignment usage checkpoint: %+v error=%v", records, err)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	for index, record := range records[:2] {
		if record.ID != strings.Repeat([]string{"a", "b"}[index], 32) || record.JobID != strings.Repeat(strconv.Itoa(index+1), 64) || record.Status != "succeeded" || record.CodexCalls != 1 || record.ClaudeCalls != 1 || record.Tokens != (usage.Tokens{Input: 2 * tokens.Input, CachedInput: 2 * tokens.CachedInput, Output: 2 * tokens.Output}) || !record.UsageComplete {
			t.Fatalf("completed assignment %d usage changed after endpoint close: %+v", index+1, record)
		}
	}
	if record := records[2]; record.ID != strings.Repeat("c", 32) || record.JobID != third.job.Identity.JobID || record.Status != "failed" || record.CodexCalls != 1 || record.ClaudeCalls != 0 || record.Tokens != tokens || !record.UsageComplete {
		t.Fatalf("canceled assignment usage checkpoint: %+v", record)
	}
	if err := os.WriteFile(report+".release", []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	waited = true
	if waitErr != nil || cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatalf("direct worker did not finish: error=%v state=%v output=%s", waitErr, cmd.ProcessState, output.String())
	}
	if err := endpoint.Close(); err != nil {
		t.Fatalf("close endpoint after direct worker exit: %v", err)
	}
	closed = true
	if _, err := os.Lstat(endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released worker left owned socket: %v", err)
	}
	if entries, err := os.ReadDir(workRoot); err != nil || len(entries) != 0 {
		t.Fatalf("worker work root was not cleaned: entries=%v error=%v", entries, err)
	}
}

func unixWorkerTestSelection() WorkerSelection {
	return WorkerSelection{
		Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}},
		Main:     RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"},
		Critic:   &RoleSettings{Provider: "claude", Model: "critic-1", Effort: "medium"},
	}
}

func unixWorkerSelectionMismatches(base Job) []Job {
	change := func(edit func(*Job)) Job {
		job := base
		critic := *base.Critic
		job.Critic = &critic
		edit(&job)
		return job
	}
	return []Job{
		change(func(job *Job) { job.Command = "other" }),
		change(func(job *Job) { job.Action = command.Fix }),
		change(func(job *Job) { job.Main.Model = "other" }),
		change(func(job *Job) { job.Main.Effort = "low" }),
		change(func(job *Job) { job.Main.Skills = []string{testReviewSkillURL} }),
		change(func(job *Job) { job.Main.AlwaysOnSkills = []string{skills.BuiltinAntiPromptInjection} }),
		change(func(job *Job) { job.Main.Provider, job.Critic.Provider = "claude", "codex" }),
		change(func(job *Job) { job.Critic.Model = "other" }),
		change(func(job *Job) { job.Critic.Effort = "low" }),
		change(func(job *Job) { job.Critic.AlwaysOnSkills = []string{skills.BuiltinAntiPromptInjection} }),
		change(func(job *Job) { job.Critic = nil; job.CriticMaxRounds = 0 }),
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
	if err := workspace.WriteAtomic(report+".ready", []byte(strconv.Itoa(os.Getpid()))); err != nil {
		t.Fatal(err)
	}
	client, err := NewUnixExecutionClient(os.Getenv("MINION_UNIX_WORKER_SOCKET"))
	if err != nil {
		t.Fatal(err)
	}
	nativeCalls := 0
	cancelWitness := make(chan error, 1)
	wantRuns := []modelRun{
		{provider: "codex", role: roleMain, model: "main-1", effort: "high"},
		{provider: "claude", role: roleCritic, model: "critic-1", effort: "medium"},
		{provider: "codex", role: roleMain, model: "main-1", effort: "high"},
		{provider: "claude", role: roleCritic, model: "critic-1", effort: "medium"},
		{provider: "codex", role: roleMain, model: "main-1", effort: "high"},
	}
	runner, err := NewSelectedRunner(Runner{WorkRoot: os.Getenv("MINION_UNIX_WORKER_WORKROOT"), runModel: func(callCtx context.Context, request modelRun) (aicli.Result, error) {
		nativeCalls++
		if nativeCalls > len(wantRuns) {
			return aicli.Result{}, fmt.Errorf("unexpected model call: %+v", request)
		}
		want := wantRuns[nativeCalls-1]
		if request.provider != want.provider || request.role != want.role || request.model != want.model || request.effort != want.effort {
			return aicli.Result{}, fmt.Errorf("model settings for call %d: got=%+v want=%+v", nativeCalls, request, want)
		}
		if nativeCalls == 5 {
			context.AfterFunc(callCtx, func() {
				cancelWitness <- workspace.WriteAtomic(report+".cancel-seen", []byte(callCtx.Err().Error()))
			})
		}
		answer := proposalJSON("Worker answer", "")
		if request.role == roleCritic {
			answer = approvedCritiqueJSON
		} else if request.role != roleMain {
			return aicli.Result{}, fmt.Errorf("unexpected model role %q", request.role)
		}
		return aicli.Result{Answer: answer, Tokens: usage.Tokens{Input: 9, CachedInput: 2, Output: 4}, UsageComplete: true}, nil
	}}, unixWorkerTestSelection())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	for index := 0; index < 2; index++ {
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
		if index == 0 {
			unixWorkerReport(t, ctx, report, ".second-go")
			for mismatch := range unixWorkerSelectionMismatches(withClaudeCritic(testJob(command.Comment))) {
				if err := RunHTTP(ctx, client, UnixExecutionBaseURL, "local", runner); !errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
					t.Fatalf("mismatch %d lost known failure: %v", mismatch, err)
				}
				if err := workspace.WriteAtomic(report+fmt.Sprintf(".mismatch-%d", mismatch), []byte(fmt.Sprintf("%d:%d", os.Getpid(), nativeCalls))); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	unixWorkerReport(t, ctx, report, ".third-go")
	if err := RunHTTP(ctx, client, UnixExecutionBaseURL, "local", runner); !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
		t.Fatalf("paid canceled worker assignment: %v", err)
	}
	select {
	case err := <-cancelWitness:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("native cancellation marker callback did not drain: %v", ctx.Err())
	}
	if err := workspace.WriteAtomic(report+".third", []byte(fmt.Sprintf("%d:%d", os.Getpid(), nativeCalls))); err != nil {
		t.Fatal(err)
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
