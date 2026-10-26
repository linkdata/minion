//go:build !windows

package action

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
)

func TestRunHTTPLoopUsesOnlyEachAssignmentsCredentials(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	t.Setenv("CODEX_HOME", t.TempDir())
	const workerToken = "worker-transport-only-secret"
	t.Setenv("MINION_WORKER_TOKEN", workerToken)
	secrets := []string{"first-job-provider-secret", "second-job-provider-secret"}
	report := filepath.Join(t.TempDir(), "native-credentials")
	fakeOnPath(t, "codex", "#!/bin/sh\nprintf '%s|%s|%s\\n' \"${CODEX_API_KEY-absent}\" \"${CODEX_ACCESS_TOKEN-absent}\" \"${MINION_WORKER_TOKEN-absent}\" >> "+shellQuote(report)+"\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"test\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\"}'\n")
	executor, err := NewHTTPExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.SetWorkerCredentials(WorkerCredentials{CodexAPIKey: secrets[0]}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/next" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			for _, secret := range append(secrets, workerToken) {
				if bytes.Contains(body, []byte(secret)) {
					t.Errorf("credential reached %s request", r.URL.Path)
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		executor.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer executor.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	runner := &Runner{WorkRoot: t.TempDir()}
	workerDone := make(chan error, 1)
	go func() { workerDone <- RunHTTPLoop(ctx, server.Client(), server.URL, "local", runner) }()
	for i, secret := range secrets {
		if i == 1 {
			// Simulate a parent credential rotation while retaining the same worker.
			executor.mu.Lock()
			executor.credentials = WorkerCredentials{CodexAccessToken: secret}
			executor.mu.Unlock()
		}
		job := testJob(command.Comment)
		job.Identity.JobID = "credential-loop-job-" + string(rune('1'+i))
		job.Identity.AttemptID = "credential-loop-attempt-" + string(rune('1'+i))
		result, err := executor.Execute(ctx, job, runnerExecutionCheckpoints{})
		if err != nil || result.Answer != "done" || result.Validate(job) != nil {
			t.Fatalf("job %d outcome: answer=%q error=%v validation=%v", i+1, result.Answer, err, result.Validate(job))
		}
		if runner.Codex.APIKey != "" || runner.Codex.NativeAPIKey != "" || runner.Codex.NativeAccessToken != "" || runner.Claude.APIKey != "" {
			t.Fatal("base worker Runner retained assignment credentials")
		}
	}
	if err := executor.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatalf("worker loop failed after two acknowledged jobs: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("worker loop did not close: %v", ctx.Err())
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	want := secrets[0] + "|absent|absent\nabsent|" + secrets[1] + "|absent\n"
	if string(data) != want || strings.Contains(string(data), workerToken) {
		t.Fatalf("native calls received stale or worker authentication: %q", data)
	}
}
