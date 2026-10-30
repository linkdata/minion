//go:build linux

package action

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func remoteCLIJob() Job {
	job := testJob(command.Comment)
	job.Main.Model, job.Main.Effort = "main-1", "high"
	job.Main.Skills = []string{testReviewSkillURL}
	return job
}

func remoteCLIServerTest(t *testing.T, job Job, credentials WorkerCredentials) *httpServerTest {
	return newHTTPServerTestWithServer(t, t.Context(), job, runnerExecutionCheckpoints{}, nil,
		func(handler http.Handler) *httptest.Server { return httptest.NewTestServer(t, handler) }, credentials)
}

func TestRemoteRunnerChecksCLIWithEachAssignmentCredential(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, key := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "")
	}
	report := filepath.Join(t.TempDir(), "keys")
	script := strings.Replace(preflightCodexCLI, "    IFS= read -r request || exit 0\n", "    IFS= read -r request || exit 0\n    printf '%s\\n' \"${CODEX_API_KEY-}\" >> "+shellQuote(report)+"\n    [ -n \"${CODEX_API_KEY-}\" ] || exit 7\n", 1)
	fakeOnPath(t, "codex", script)
	workRoot := t.TempDir()
	runner := NewRemoteRunner(workRoot)
	modelCalls := 0
	runner.runModel = func(context.Context, modelRun) (aicli.Result, error) {
		modelCalls++
		return aicli.Result{Answer: "Ready", UsageComplete: true}, nil
	}
	for i, credential := range []string{"assigned-first", "assigned-second"} {
		job := remoteCLIJob()
		job.Identity.JobID += string(rune('1' + i))
		job.Identity.AttemptID += string(rune('1' + i))
		f := remoteCLIServerTest(t, job, WorkerCredentials{CodexNativeAPIKey: credential})
		if err := RunHTTP(t.Context(), f.server.Client(), f.server.URL, job.Identity.ExecutorID, runner); err != nil {
			t.Fatalf("assignment %d: %v", i+1, err)
		}
		got := f.result(t)
		if got.err != nil || got.result.Answer != "Ready" || len(got.result.Calls) != 1 || runner.Codex.NativeAPIKey != "" {
			t.Fatalf("assignment %d: result=%+v error=%v", i+1, got.result, got.err)
		}
	}
	data, err := os.ReadFile(report)
	if err != nil || modelCalls != 2 || !strings.Contains(string(data), "assigned-first\n") || !strings.Contains(string(data), "assigned-second\n") {
		t.Fatalf("per-assignment CLI checks = %q, %v; model calls=%d", data, err, modelCalls)
	}
	assertEmptyDir(t, workRoot)
}

func TestRemoteRunnerCLIFailureUsesFinalOutcome(t *testing.T) {
	for _, tc := range []struct {
		name        string
		credentials WorkerCredentials
		script      string
		model       string
		effort      string
		want        string
	}{
		{name: "login", script: "#!/bin/sh\ncase \"$1 $2\" in 'login status') printf 'missing login\\n' >&2; exit 7;; esac\nexit 99\n", want: preflightCodexLoginUnavailable},
		{name: "instruction boundary", credentials: WorkerCredentials{CodexNativeAPIKey: "assigned-secret"}, script: "#!/bin/sh\ncase \"$1 $2\" in 'debug prompt-input') printf 'bad boundary\\n' >&2; exit 7;; esac\nexit 99\n", want: preflightCodexBoundaryUnavailable},
		{name: "model", credentials: WorkerCredentials{CodexNativeAPIKey: "assigned-secret"}, script: preflightCodexCLI, model: "missing-model", want: preflightCodexInvocationUnsupported},
		{name: "effort", credentials: WorkerCredentials{CodexNativeAPIKey: "assigned-secret"}, script: preflightCodexCLI, effort: "low", want: preflightCodexInvocationUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installActionTestSkills(t, testReviewSkillURL)
			t.Setenv("CODEX_HOME", t.TempDir())
			for _, key := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
				t.Setenv(key, "")
			}
			fakeOnPath(t, "codex", tc.script)
			job := remoteCLIJob()
			if tc.model != "" {
				job.Main.Model = tc.model
			}
			if tc.effort != "" {
				job.Main.Effort = tc.effort
			}
			f := remoteCLIServerTest(t, job, tc.credentials)
			workRoot := t.TempDir()
			runner := NewRemoteRunner(workRoot)
			runner.runModel = func(context.Context, modelRun) (aicli.Result, error) {
				t.Fatal("CLI failure reached model call")
				return aicli.Result{}, nil
			}
			err := RunHTTP(t.Context(), f.server.Client(), f.server.URL, job.Identity.ExecutorID, runner)
			got := f.result(t)
			if !errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "assigned-secret") {
				t.Fatalf("worker CLI failure was not terminal and safe: %v", err)
			}
			var workerFailure *workerFailureError
			if !errors.As(got.err, &workerFailure) || !workerFailure.failure.Terminal || !strings.Contains(workerFailure.failure.Message, tc.want) || strings.Contains(workerFailure.failure.Message, "assigned-secret") {
				t.Fatalf("CLI failure did not reach the final outcome: %v", got.err)
			}
			if len(got.result.Calls) != 0 || runner.Codex.NativeAPIKey != "" {
				t.Fatalf("failed CLI reached model or retained credential: %+v", got.result)
			}
			assertEmptyDir(t, workRoot)
		})
	}
}
