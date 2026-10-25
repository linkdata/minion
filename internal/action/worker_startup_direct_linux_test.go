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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/workspace"
)

func TestSelectedWorkerStartupDirectChild(t *testing.T) {
	for _, tc := range []struct {
		name         string
		invalidInput bool
		unselected   bool
		badPreflight bool
		wantError    string
	}{
		{name: "selected startup"},
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
			}
			codexDir := fakeOnPath(t, "codex", codexScript)
			t.Setenv("PATH", codexDir+string(os.PathListSeparator)+gitOnlyPath(t))
			startup := WorkerStartup{
				Selection: WorkerSelection{Commands: map[string]config.CommandMapping{
					"comment": {Action: "comment", Skills: []string{testReviewSkillURL}},
				}, Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}},
				WorkRoot: filepath.Join(base, "work"),
			}
			var inputErr error
			worker, err := startLocalWorker(unixTestDirectory(t), "local", os.Args[0], []string{"-test.run=^TestSelectedWorkerStartupDirectChildHelper$"}, 20*time.Second, func(cmd *exec.Cmd, socket string) {
				startup.SocketPath = socket
				var input []byte
				switch {
				case tc.invalidInput:
					input = []byte("{")
				case tc.unselected:
					startup.Selection = WorkerSelection{}
					input, inputErr = json.Marshal(startup)
				default:
					input, inputErr = EncodeWorkerStartup(startup)
				}
				if inputErr == nil {
					cmd.Stdin = bytes.NewReader(input)
				}
				cmd.Env = append(os.Environ(), "MINION_STARTUP_CHILD=1", "MINION_STARTUP_BASE="+base)
			})
			if inputErr != nil {
				if worker != nil {
					_ = worker.Close()
				}
				t.Fatal(inputErr)
			}
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
