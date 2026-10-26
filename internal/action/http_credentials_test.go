package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestWorkerCredentialsValidateAndAssign(t *testing.T) {
	job := testJob(command.Comment)
	secret := "worker-assignment-only-secret"
	for _, test := range []struct {
		name        string
		credentials WorkerCredentials
		valid       bool
	}{
		{"saved login", WorkerCredentials{}, true},
		{"configured Codex key", WorkerCredentials{CodexAPIKey: secret}, true},
		{"native Codex key", WorkerCredentials{CodexNativeAPIKey: secret}, true},
		{"native Codex token", WorkerCredentials{CodexAccessToken: secret}, true},
		{"unselected Claude key", WorkerCredentials{ClaudeAPIKey: secret}, false},
		{"mixed Codex auth", WorkerCredentials{CodexAPIKey: secret, CodexNativeAPIKey: "native-secret"}, false},
		{"invalid UTF-8", WorkerCredentials{CodexAPIKey: string([]byte{0xff})}, false},
		{"newline", WorkerCredentials{CodexAPIKey: secret + "\n"}, false},
		{"oversized", WorkerCredentials{CodexAPIKey: strings.Repeat("x", maxWorkerCredentialBytes+1)}, false},
		{"aggregate oversized", WorkerCredentials{CodexNativeAPIKey: strings.Repeat("x", 4096), CodexAccessToken: strings.Repeat("y", 4097)}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.credentials.validateJob(job)
			for _, value := range []string{test.credentials.CodexAPIKey, test.credentials.CodexNativeAPIKey, test.credentials.CodexAccessToken, test.credentials.ClaudeAPIKey} {
				if value != "" && err != nil && strings.Contains(err.Error(), value) {
					t.Fatal("credential validation error exposed a credential")
				}
			}
			if (err == nil) != test.valid {
				t.Fatalf("credential validity=%t got_error=%t", test.valid, err != nil)
			}
		})
	}
	credentials := WorkerCredentials{CodexAPIKey: secret}
	f := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil, credentials)
	assignment := f.pull(t)
	if assignment.Credentials != credentials {
		t.Fatal("assignment lost selected credentials")
	}
	for name, value := range map[string]any{
		"Job":    assignment.Job,
		"Result": Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head},
		"Status": Status{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Sequence: 1, WorkerState: "busy", Phase: "prepare"},
	} {
		encoded, err := json.Marshal(value)
		if err != nil || bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("%s contains an assignment credential: %v", name, err)
		}
	}
	if err := f.executor.SetWorkerCredentials(WorkerCredentials{}); err == nil {
		t.Fatal("credentials changed after worker startup")
	}
	f.ack(t, "/outcome", f.outcome(1, errors.New("known failure")), http.StatusOK, 1)
	if got := f.result(t); got.err == nil {
		t.Fatal("known failed outcome was lost")
	}

	wrong, err := NewHTTPExecutor(job.Identity.ExecutorID)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.SetWorkerCredentials(WorkerCredentials{ClaudeAPIKey: secret}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Execute(t.Context(), job, runnerExecutionCheckpoints{}); !errors.Is(err, monitor.ErrTerminalJob) || strings.Contains(err.Error(), secret) {
		t.Fatal("unselected credential was dispatched or exposed")
	}
}

func TestHTTPAssignmentBoundIncludesCredentialEscaping(t *testing.T) {
	job := testJob(command.Comment)
	credentials := WorkerCredentials{CodexAPIKey: strings.Repeat("\x01", 1024)}
	bound := httpAssignmentBound(job, credentials)
	encoded, err := httpEncode(httpAssignment{Job: job, Credentials: credentials, HeldFor: math.MaxInt64}, httpJobBytes)
	if err != nil || bound < int64(len(encoded)) {
		t.Fatalf("assignment bound=%d encoded=%d error=%v", bound, len(encoded), err)
	}
	job.Description = ""
	room := (int64(httpJobBytes) - httpJobBound(job) - 3*maxWorkerCredentialBytes) / 6
	job.Description = strings.Repeat("d", int(room))
	credentials.CodexAPIKey = strings.Repeat("x", maxWorkerCredentialBytes)
	if err := job.Validate(); err != nil || credentials.validateJob(job) != nil || httpJobBound(job) > httpJobBytes || httpAssignmentBound(job, credentials) <= httpJobBytes {
		t.Fatalf("credential growth bypassed pre-encoding limit: job=%d assignment=%d error=%v", httpJobBound(job), httpAssignmentBound(job, credentials), err)
	}
}

func TestRunHTTPKeepsCredentialsPerJobAndCleansBeforeOutcome(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	workRoot := t.TempDir()
	secrets := []string{"first-job-provider-secret", "second-job-provider-secret"}
	modelCalls := 0
	runner := &Runner{WorkRoot: workRoot, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if strings.Contains(request.prompt, secrets[0]) || strings.Contains(request.prompt, secrets[1]) {
			t.Fatal("credential reached model prompt")
		}
		secret := secrets[modelCalls]
		modelCalls++
		return aicli.Result{Answer: "Report " + secret, UsageComplete: true}, nil
	}}
	for i, secret := range secrets {
		job := testJob(command.Comment)
		job.Identity.JobID = "credential-job-" + string(rune('1'+i))
		job.Identity.AttemptID = "credential-attempt-" + string(rune('1'+i))
		f := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil, WorkerCredentials{CodexAPIKey: secret})
		client := *f.server.Client()
		transport := client.Transport
		client.Transport = httpClientTestTransport(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/outcome" {
				entries, err := os.ReadDir(workRoot)
				if err != nil || len(entries) != 0 {
					t.Errorf("attempt workspaces remained during outcome upload: %v, %v", entries, err)
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
				}
				for _, value := range secrets {
					if bytes.Contains(body, []byte(value)) {
						t.Error("provider credential reached outcome")
					}
				}
				request.Body = io.NopCloser(bytes.NewReader(body))
			}
			return transport.RoundTrip(request)
		})
		if err := RunHTTP(t.Context(), &client, f.server.URL, job.Identity.ExecutorID, runner); err != nil {
			t.Fatal(err)
		}
		got := f.result(t)
		if got.err != nil || got.result.Answer != "Report [redacted]" || runner.Codex.APIKey != "" || runner.Codex.NativeAPIKey != "" || runner.Codex.NativeAccessToken != "" || runner.Claude.APIKey != "" {
			t.Fatalf("job %d retained or exposed provider credentials", i+1)
		}
	}
	if modelCalls != len(secrets) {
		t.Fatalf("model calls=%d, want %d", modelCalls, len(secrets))
	}
}

func TestHTTPExecutorQuarantinesCleanupFailure(t *testing.T) {
	job := testJob(command.Comment)
	f := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil)
	f.pull(t)
	outcome := f.outcome(1, nil)
	outcome.Result.Diagnostics = []Diagnostic{{Code: "cleanup_failed", Message: "cleanup failed"}}
	f.ack(t, "/outcome", outcome, http.StatusOK, 1)
	if got := f.result(t); got.err != nil || len(got.result.Diagnostics) != 1 {
		t.Fatalf("cleanup failure was not retained: %+v", got)
	}
	job.Identity.JobID, job.Identity.AttemptID = "later-job", "later-attempt"
	if _, err := f.executor.Execute(t.Context(), job, runnerExecutionCheckpoints{}); err == nil {
		t.Fatal("unclean executor accepted another job")
	}
}
