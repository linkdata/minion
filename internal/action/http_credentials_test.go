package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
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
		{"oversized Codex key", WorkerCredentials{CodexAPIKey: strings.Repeat("x", config.MaxProviderAPIKeyBytes+1)}, false},
		{"oversized native Codex key", WorkerCredentials{CodexNativeAPIKey: strings.Repeat("x", config.MaxProviderAPIKeyBytes+1)}, false},
		{"oversized access token", WorkerCredentials{CodexAccessToken: strings.Repeat("x", maxWorkerAccessTokenBytes+1)}, false},
		{"native key and access token", WorkerCredentials{CodexNativeAPIKey: strings.Repeat("x", config.MaxProviderAPIKeyBytes), CodexAccessToken: strings.Repeat("y", maxWorkerAccessTokenBytes)}, true},
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
	if err := (WorkerCredentials{ClaudeAPIKey: strings.Repeat("x", config.MaxProviderAPIKeyBytes+1)}).validate(); err == nil {
		t.Fatal("oversized Claude key accepted")
	}
	if err := (WorkerCredentials{CodexNativeAPIKey: "x", ClaudeAPIKey: strings.Repeat("y", config.MaxProviderAPIKeyBytes)}).validate(); err != nil {
		t.Fatalf("ambient Codex key with maximum saved Claude key: %v", err)
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

func TestRemoteCredentialsChangeRequiresFreshPreflight(t *testing.T) {
	job := testJob(command.Comment)
	executor, err := NewHTTPExecutor(job.Identity.ExecutorID)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.SetWorkerCredentials(WorkerCredentials{CodexAPIKey: "old-key"}); err != nil {
		t.Fatal(err)
	}
	executor.remoteChoices = &remoteChoicePreflight{Version: ProtocolVersion, ExecutorID: job.Identity.ExecutorID,
		Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}}
	executor.choicesFetched = true
	server := httptest.NewServer(executor)
	client := server.Client()
	t.Cleanup(func() {
		client.CloseIdleConnections()
		_ = executor.Close()
		server.Close()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	post := func(path string, body httpPull, output any) int {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if err := httpDecode(response.Body, httpControlBytes, output); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode
	}
	pullBody, err := json.Marshal(httpPull{Version: ProtocolVersion, ExecutorID: job.Identity.ExecutorID})
	if err != nil {
		t.Fatal(err)
	}
	pullRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/next", bytes.NewReader(pullBody))
	if err != nil {
		t.Fatal(err)
	}
	pullRequest.Header.Set("Content-Type", "application/json")
	type pullResult struct {
		status int
		err    error
	}
	pulled := make(chan pullResult, 1)
	t.Cleanup(func() {
		for range pulled {
		}
	})
	go func() {
		defer close(pulled)
		response, err := client.Do(pullRequest)
		if err != nil {
			pulled <- pullResult{err: err}
			return
		}
		defer response.Body.Close()
		_, err = io.Copy(io.Discard, response.Body)
		pulled <- pullResult{status: response.StatusCode, err: err}
	}()
	for {
		executor.mu.Lock()
		pull, changed := executor.pull, executor.changed
		executor.mu.Unlock()
		if pull {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("worker did not hold a pull")
		}
	}
	newCredentials := WorkerCredentials{CodexAPIKey: "new-key"}
	updated := make(chan error, 1)
	t.Cleanup(func() {
		for range updated {
		}
	})
	go func() {
		defer close(updated)
		updated <- executor.updateIdleWorkerCredentials(ctx, newCredentials)
	}()
	select {
	case got := <-pulled:
		if got.status != http.StatusTooEarly || got.err != nil {
			t.Fatalf("old pull was not retired: status=%d error=%v", got.status, got.err)
		}
	case <-ctx.Done():
		t.Fatal("old pull was not retired")
	}
	var rejection httpAcknowledgement
	if status := post("/next", httpPull{Version: ProtocolVersion, ExecutorID: job.Identity.ExecutorID}, &rejection); status != http.StatusPreconditionRequired {
		t.Fatalf("unvalidated candidate admitted a pull: status=%d", status)
	}
	var selection remoteChoicePreflight
	if status := post("/preflight", httpPull{Version: ProtocolVersion, ExecutorID: job.Identity.ExecutorID}, &selection); status != http.StatusOK ||
		!selection.Refresh || selection.PreflightGeneration == 0 || selection.Credentials != newCredentials {
		t.Fatalf("candidate preflight: status=%d selection=%+v", status, selection)
	}
	var stale httpAcknowledgement
	if status := post("/preflight", httpPull{Version: ProtocolVersion, ExecutorID: job.Identity.ExecutorID,
		PreflightGeneration: selection.PreflightGeneration + 1, PreflightResult: "ok"}, &stale); status != http.StatusPreconditionRequired {
		t.Fatalf("stale validation acknowledged: status=%d", status)
	}
	executor.mu.Lock()
	oldCredential, gateOpen := executor.credentials, executor.choicesFetched && !executor.preflightPending
	executor.mu.Unlock()
	if oldCredential.CodexAPIKey != "old-key" || gateOpen {
		t.Fatal("candidate became active before matching validation")
	}
	select {
	case err := <-updated:
		t.Fatalf("candidate update finished before validation: %v", err)
	default:
	}
	var ack httpAcknowledgement
	if status := post("/preflight", httpPull{Version: ProtocolVersion, ExecutorID: job.Identity.ExecutorID,
		PreflightGeneration: selection.PreflightGeneration, PreflightResult: "ok"}, &ack); status != http.StatusOK {
		t.Fatalf("candidate validation ACK status=%d", status)
	}
	if err := <-updated; err != nil {
		t.Fatalf("candidate was not activated: %v", err)
	}
	fixture := &httpServerTest{executor: executor, server: server, job: job, done: make(chan httpServerTestResult, 1)}
	go func() {
		result, err := executor.Execute(ctx, job, runnerExecutionCheckpoints{})
		fixture.done <- httpServerTestResult{result: result, err: err}
	}()
	assignment := fixture.pull(t)
	if assignment.Credentials != newCredentials {
		t.Fatalf("next assignment credentials=%+v", assignment.Credentials)
	}
	if err := executor.updateIdleWorkerCredentials(ctx, WorkerCredentials{}); err == nil {
		t.Fatal("credentials changed during an active assignment")
	}
	fixture.ack(t, "/outcome", fixture.outcome(1, nil), http.StatusOK, 1)
	if got := fixture.result(t); got.err != nil {
		t.Fatalf("known outcome failed: %v", got.err)
	}
}

func TestHTTPAssignmentBoundIncludesCredentialEscaping(t *testing.T) {
	job := testJob(command.Comment)
	credentials := WorkerCredentials{CodexAPIKey: strings.Repeat("\x01", config.MaxProviderAPIKeyBytes)}
	bound := httpAssignmentBound(job, credentials)
	encoded, err := httpEncode(httpAssignment{Job: job, Credentials: credentials, HeldFor: math.MaxInt64}, httpJobBytes)
	if err != nil || bound < int64(len(encoded)) {
		t.Fatalf("assignment bound=%d encoded=%d error=%v", bound, len(encoded), err)
	}
	job.Description = ""
	room := (int64(httpJobBytes) - httpJobBound(job) - 3*maxWorkerAccessTokenBytes) / 6
	job.Description = strings.Repeat("d", int(room))
	credentials = WorkerCredentials{CodexNativeAPIKey: strings.Repeat("x", config.MaxProviderAPIKeyBytes), CodexAccessToken: strings.Repeat("y", maxWorkerAccessTokenBytes)}
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

func TestRunHTTPRedactsWorkerAmbientCredentialFromPreparedInput(t *testing.T) {
	const secret = "worker-only-ambient-codex-key"
	t.Setenv("CODEX_API_KEY", secret)
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	job := testJob(command.Comment)
	job.Description = "Task text contains " + secret
	root := t.TempDir()
	preparedSaved := make(chan struct{}, 1)
	f := newHTTPServerTestWithStopServerRoot(t, t.Context(), job, runnerExecutionCheckpoints{},
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/status" {
					next.ServeHTTP(w, r)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				var observation httpObservation
				prepared := json.Unmarshal(body, &observation) == nil && len(observation.Logs) == 1 &&
					observation.Logs[0].EventSource == "codex.input.prepared"
				next.ServeHTTP(w, r)
				if prepared {
					select {
					case preparedSaved <- struct{}{}:
					default:
					}
				}
			})
		}, func(h http.Handler) *httptest.Server { return httptest.NewTestServer(t, h) }, 0, root)
	// The model stub isolates HTTP credential handling from CLI capability discovery.
	runner := &Runner{WorkRoot: t.TempDir()}
	runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.onInput == nil || request.onEvent == nil || !strings.Contains(request.prompt, secret) {
			return aicli.Result{}, errors.New("fixture did not reach the prepared Codex input")
		}
		request.onInput(aicli.PreparedInput{Mode: "fresh", Path: "/fixture/codex", Args: []string{"codex", "exec", "-"},
			Dir: request.dir, Stdin: request.prompt})
		wait, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		select {
		case <-preparedSaved:
		case <-wait.Done():
			return aicli.Result{}, wait.Err()
		}
		request.onEvent(aicli.StreamEvent{NativeID: "root", Kind: "identity"})
		return aicli.Result{Answer: "Done", UsageComplete: true}, nil
	}
	if err := RunHTTP(t.Context(), f.server.Client(), f.server.URL, job.Identity.ExecutorID, runner); err != nil {
		t.Fatal(err)
	}
	if got := f.result(t); got.err != nil || got.result.Answer != "Done" {
		t.Fatalf("remote worker result = %+v", got)
	}
	data := httpTestRecords(t, root, job)
	if bytes.Contains(data, []byte(secret)) {
		t.Fatal("worker-only ambient credential reached the retained attempt records")
	}
	if !bytes.Contains(data, []byte("[redacted]")) || !bytes.Contains(data, []byte("codex.input.prepared")) {
		t.Fatalf("prepared input was not retained in redacted form: %q", data)
	}
	if runner.Codex.APIKey != "" || runner.Codex.NativeAPIKey != "" || runner.Codex.NativeAccessToken != "" {
		t.Fatal("remote worker retained an ambient credential after the action")
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
