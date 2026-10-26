//go:build linux

package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
	"github.com/linkdata/minion/internal/workspace"
)

func TestWorkerStartupClearsPreflightCredentials(t *testing.T) {
	startup := testWorkerStartup(t)
	startup.CodexAPIKey = ""
	startup.CodexNativeAPIKey = "private-native-key"
	startup.CodexAccessToken = "private-native-token"
	runner, err := startup.runner(nil)
	if err != nil {
		t.Fatal(err)
	}
	startup.clearCredentials(runner)
	if startup.CodexAPIKey != "" || startup.CodexNativeAPIKey != "" || startup.CodexAccessToken != "" || startup.ClaudeAPIKey != "" ||
		runner.Codex.APIKey != "" || runner.Codex.NativeAPIKey != "" || runner.Codex.NativeAccessToken != "" || runner.Claude.APIKey != "" || runner.selection == nil {
		t.Fatal("preflight credentials remained in reusable startup or runner")
	}
}

func TestSelectedWorkerStartupDirectChild(t *testing.T) {
	for _, tc := range []struct {
		name         string
		invalidInput bool
		unselected   bool
		badPreflight bool
		nativeAuth   bool
		wantError    string
	}{
		{name: "selected startup"},
		{name: "native credentials during preflight", nativeAuth: true},
		{name: "malformed input", invalidInput: true, wantError: "decode worker startup"},
		{name: "unselected input", unselected: true, wantError: "worker startup selection"},
		{name: "failed preflight", badPreflight: true, wantError: "Codex instruction boundary unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			for _, name := range []string{"work", "codex-home"} {
				if err := os.Mkdir(filepath.Join(base, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", base)
			t.Setenv("CODEX_HOME", filepath.Join(base, "codex-home"))
			for _, name := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_API_KEY"} {
				t.Setenv(name, "")
			}
			writeActionTestSkills(t, base, ".agents", testReviewSkillURL)
			codexScript := preflightCodexCLI
			if tc.badPreflight {
				codexScript = "#!/bin/sh\ncase \"$1 $2\" in 'login status') exit 0;; 'debug prompt-input') exit 2;; esac\nexit 99\n"
			} else if tc.nativeAuth {
				codexScript = strings.Replace(preflightCodexCLI, "    IFS= read -r request || exit 0\n", "    IFS= read -r request || exit 0\n    [ \"$CODEX_API_KEY\" = native-startup-key ] && [ \"$CODEX_ACCESS_TOKEN\" = native-startup-token ] || exit 19\n", 1)
				if codexScript == preflightCodexCLI {
					t.Fatal("native credential fixture marker missing")
				}
			}
			codexDir := fakeOnPath(t, "codex", codexScript)
			t.Setenv("PATH", codexDir+string(os.PathListSeparator)+gitOnlyPath(t))
			startup := WorkerStartup{
				Selection: WorkerSelection{Commands: map[string]config.CommandMapping{
					"comment": {Action: "comment", Skills: []string{testReviewSkillURL}},
				}, Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}},
				WorkRoot: filepath.Join(base, "work"),
			}
			if tc.nativeAuth {
				startup.CodexNativeAPIKey, startup.CodexAccessToken = "native-startup-key", "native-startup-token"
			}
			worker, err := startLocalWorker("local", os.Args[0], []string{"-test.run=^TestSelectedWorkerStartupDirectChildHelper$"}, 20*time.Second, WorkerCredentials{}, func(cmd *exec.Cmd, access LocalEndpointAccess) error {
				startup.Endpoint = access
				var input []byte
				var inputErr error
				switch {
				case tc.invalidInput:
					input = []byte("{")
				case tc.unselected:
					startup.Selection = WorkerSelection{}
					input, inputErr = json.Marshal(startup)
				default:
					input, inputErr = EncodeWorkerStartup(startup)
				}
				if inputErr != nil {
					return inputErr
				}
				cmd.Stdin = bytes.NewReader(input)
				cmd.Env = append(os.Environ(), "MINION_STARTUP_CHILD=1", "MINION_STARTUP_BASE="+base)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = worker.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			err = worker.WaitCapacity(ctx)
			if tc.invalidInput || tc.unselected || tc.badPreflight {
				if err == nil || ctx.Err() != nil {
					t.Fatalf("invalid startup reached a held pull or timed out: %v", err)
				}
				if worker.cmd.ProcessState == nil || worker.cmd.ProcessState.Success() || !strings.Contains(worker.stderr.String(), tc.wantError) {
					t.Fatalf("startup failure did not exit for %q: state=%v stderr=%q", tc.wantError, worker.cmd.ProcessState, worker.stderr.String())
				}
				worker.executor.mu.Lock()
				held := worker.executor.pull
				worker.executor.mu.Unlock()
				if held {
					t.Fatal("invalid startup requested a Job")
				}
				if _, err := os.Stat(filepath.Join(base, "model")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid startup called the model: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("selected startup did not hold a pull: %v", err)
			}
			job := testJob(command.Comment)
			job.Main.Model, job.Main.Effort, job.Main.Skills = "main-1", "high", []string{testReviewSkillURL}
			for i := 1; i <= 2; i++ {
				if i == 2 {
					job.Identity.JobID, job.Identity.AttemptID = "second-job", "second-attempt"
				}
				result, err := worker.Execute(ctx, job, runnerExecutionCheckpoints{})
				if err != nil || result.Answer != "Worker answer" || result.Validate(job) != nil {
					t.Fatalf("selected child action %d: result=%+v error=%v", i, result, err)
				}
				if err := worker.WaitCapacity(ctx); err != nil {
					t.Fatalf("selected child did not repull after action %d: %v", i, err)
				}
			}
			if err := worker.executor.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-worker.exited:
			case <-ctx.Done():
				t.Fatalf("selected child did not exit after idle close: %v", ctx.Err())
			}
			if worker.waitErr != nil || worker.cmd.ProcessState == nil || !worker.cmd.ProcessState.Success() {
				t.Fatalf("selected child did not exit cleanly: wait=%v state=%v", worker.waitErr, worker.cmd.ProcessState)
			}
			if data, err := os.ReadFile(filepath.Join(base, "model")); err != nil || string(data) != "2" {
				t.Fatalf("model calls = %q, %v; want two", data, err)
			}
		})
	}
}

func TestSelectedWorkerStartupDirectChildHelper(t *testing.T) {
	if os.Getenv("MINION_STARTUP_CHILD") != "1" {
		return
	}
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	base := os.Getenv("MINION_STARTUP_BASE")
	calls := 0
	err := runSelectedLocalWorker(ctx, os.Stdin, func(context.Context, modelRun) (aicli.Result, error) {
		calls++
		if err := workspace.WriteAtomic(filepath.Join(base, "model"), []byte(strconv.Itoa(calls))); err != nil {
			return aicli.Result{}, err
		}
		return aicli.Result{Answer: "Worker answer", UsageComplete: true}, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "minion preflight:", err)
		t.Fatal(err)
	}
}

func TestDirectHTTPSChildRetainsMainAndCriticReceipts(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", base)
	writeActionTestSkills(t, base, ".agents", testReviewSkillURL)
	job := withClaudeCritic(testJob(command.Comment))
	job.Main.Model, job.Main.Effort, job.Main.Skills = "main-1", "high", []string{testReviewSkillURL}
	job.Critic.Model, job.Critic.Effort, job.CriticMaxRounds = "critic-1", "medium", 1
	fixture := newExecutionAdapterFixture(t, job)
	store := fixture.startUsage()
	worker, err := startLocalWorker("local", os.Args[0], []string{"-test.run=^TestDirectHTTPSChildRetainsMainAndCriticReceiptsHelper$"}, 20*time.Second, WorkerCredentials{}, func(cmd *exec.Cmd, access LocalEndpointAccess) error {
		input, err := json.Marshal(access)
		if err != nil {
			return err
		}
		cmd.Stdin = bytes.NewReader(input)
		cmd.Env = append(os.Environ(), "MINION_CRITIC_CHILD=1", "MINION_CRITIC_WORK="+work)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := worker.WaitCapacity(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := worker.Execute(ctx, fixture.job, fixture.sink)
	finishErr := fixture.sink.finish(result, err)
	if err != nil || finishErr != nil || result.Answer != "Worker answer" || result.CriticStopReason != "approved" || result.Validate(fixture.job) != nil {
		t.Fatalf("direct child outcome: result=%+v error=%v checkpoint=%v", result, err, finishErr)
	}
	if len(result.Calls) != 2 || result.Calls[0].Role != roleMain || result.Calls[1].Role != roleCritic ||
		result.Calls[0].Tokens != (usage.Tokens{Input: 7, Output: 2}) || result.Calls[1].Tokens != (usage.Tokens{Input: 11, Output: 3}) ||
		!result.Calls[0].Complete || !result.Calls[1].Complete || result.Calls[1].ReviewPhase != reviewFull ||
		!result.Calls[1].Approved || !result.Calls[1].ReviewComplete {
		t.Fatalf("direct child did not complete both model calls: %+v", result.Calls)
	}
	if err := worker.WaitCapacity(ctx); err != nil {
		t.Fatalf("direct child did not repull after review: %v", err)
	}
	if err := worker.Close(); err != nil || worker.cmd.ProcessState == nil || !worker.cmd.ProcessState.Success() {
		t.Fatalf("direct child did not exit cleanly: error=%v state=%v", err, worker.cmd.ProcessState)
	}
	saved := readPrivateReview(t, fixture.owner.WorkRoot)
	if !slices.Equal(saved.Calls, result.Calls) || saved.CriticStopReason != "approved" {
		t.Fatalf("durable private review lost child receipts: %+v", saved)
	}
	records, err := usage.ReadAll(store.Dir)
	if err != nil || len(records) != 1 || records[0].CodexCalls != 1 || records[0].ClaudeCalls != 1 ||
		records[0].Tokens != (usage.Tokens{Input: 18, Output: 5}) || !records[0].UsageComplete {
		t.Fatalf("durable paid usage lost child calls: records=%+v error=%v", records, err)
	}
}

func TestDirectHTTPSChildRetainsMainAndCriticReceiptsHelper(t *testing.T) {
	if os.Getenv("MINION_CRITIC_CHILD") != "1" {
		return
	}
	var access LocalEndpointAccess
	if err := json.NewDecoder(os.Stdin).Decode(&access); err != nil {
		t.Fatal(err)
	}
	client, err := NewLocalExecutionClient(access)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	runner := Runner{WorkRoot: os.Getenv("MINION_CRITIC_WORK"), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.round != 1 {
			t.Fatalf("unexpected review round: %d", request.round)
		}
		switch request.role {
		case roleMain:
			return aicli.Result{Answer: proposalJSON("Worker answer", ""), Tokens: usage.Tokens{Input: 7, Output: 2}, UsageComplete: true}, nil
		case roleCritic:
			if request.reviewPhase != reviewFull {
				t.Fatalf("unexpected review phase: %s", request.reviewPhase)
			}
			return aicli.Result{Answer: approvedCritiqueJSON, Tokens: usage.Tokens{Input: 11, Output: 3}, UsageComplete: true}, nil
		default:
			t.Fatalf("unexpected model role: %s", request.role)
			return aicli.Result{}, nil
		}
	}}
	if err := RunHTTPLoop(t.Context(), client, access.BaseURL, "local", &runner); err != nil {
		t.Fatal(err)
	}
}
