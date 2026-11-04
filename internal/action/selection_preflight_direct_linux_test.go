//go:build linux

package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestSelectionPreflightDirectChild(t *testing.T) {
	for _, tc := range []struct {
		name, want string
	}{
		{name: "ready"},
		{name: "known failure"},
		{name: "missing outcome ACK"},
		{name: "malformed required ACK"},
		{name: "selected CLI", want: "Codex instruction boundary unavailable"},
		{name: "selected critic", want: "Claude settings check failed"},
		{name: "selected skill", want: "selected command skill unavailable"},
		{name: "Git", want: "Git readiness unavailable"},
		{name: "audit tooling", want: "audit toolchain unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ackFailure := tc.name == "missing outcome ACK" || tc.name == "malformed required ACK"
			base, err := os.MkdirTemp(os.TempDir(), "minion-preflight-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(base); err != nil {
					t.Errorf("remove direct-child fixture: %v", err)
				}
			})
			for _, dir := range []string{"work", "codex-home"} {
				if err := os.Mkdir(filepath.Join(base, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", base)
			t.Setenv("CODEX_HOME", filepath.Join(base, "codex-home"))
			for _, key := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_API_KEY"} {
				t.Setenv(key, "")
			}
			writeActionTestSkills(t, base, ".agents", testReviewSkillURL, testAuditSkillURL)
			codexDir := fakeOnPath(t, "codex", preflightCodexCLI)
			switch tc.name {
			case "ready", "known failure", "missing outcome ACK", "malformed required ACK", "selected critic":
				t.Setenv("PATH", codexDir+string(os.PathListSeparator)+gitOnlyPath(t))
			case "selected CLI":
				fakeOnPath(t, "codex", "#!/bin/sh\ncase \"$1 $2\" in 'login status') exit 0;; 'debug prompt-input') printf 'unsupported prompt-input\\n' >&2; exit 2;; esac\nexit 99\n")
			case "selected skill":
				if err := os.Remove(filepath.Join(base, ".agents", "skills", "ponytail-review", "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			case "Git":
				git, err := aicli.AbsoluteExecutable("git")
				if err != nil {
					t.Fatal(err)
				}
				fakeOnPath(t, "git", "#!/bin/sh\ncase \" $* \" in *' --version '*) printf 'git version 2.45.0\\n'; exit 0;; esac\nexec "+shellQuote(git)+" \"$@\"\n")
			case "audit tooling":
				if err := os.Remove(filepath.Join(base, ".agents", "skills", "security-audit", "validate-findings.cjs")); err != nil {
					t.Fatal(err)
				}
			}
			worker, err := startLocalWorker("local", os.Args[0], []string{"-test.run=^TestSelectionPreflightDirectChildHelper$"}, 40*time.Second, WorkerCredentials{}, func(cmd *exec.Cmd, access LocalEndpointAccess) error {
				input, err := json.Marshal(access)
				if err != nil {
					return err
				}
				cmd.Stdin = bytes.NewReader(input)
				cmd.Env = append(os.Environ(), "MINION_PREFLIGHT_CHILD=1", "MINION_PREFLIGHT_CASE="+tc.name,
					"MINION_PREFLIGHT_BASE="+base)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := worker.Close(); err != nil && tc.want == "" && !ackFailure && !t.Failed() {
					t.Errorf("close direct child: %v", err)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			err = worker.WaitCapacity(ctx)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "worker startup diagnostic") || ctx.Err() != nil || len(err.Error()) > 9<<10 {
					t.Fatalf("failed preflight diagnostic or held pull: error=%v", err)
				}
				for _, marker := range []string{"pull", "model"} {
					if _, statErr := os.Stat(filepath.Join(base, marker)); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("failed preflight reached %s: %v", marker, statErr)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("valid child did not hold a pull: %v", err)
			}
			if _, err := os.Stat(filepath.Join(base, "pull")); err != nil {
				t.Fatalf("child pulled before preflight marker: %v", err)
			}
			job := testJob(command.Comment)
			job.Main.Model, job.Main.Effort, job.Main.Skills = "main-1", "high", []string{testReviewSkillURL}
			result, err := worker.Execute(ctx, job, runnerExecutionCheckpoints{})
			if ackFailure {
				if !result.matchesJob(job) || tc.name == "missing outcome ACK" && (err != nil || result.Answer != "Worker answer" || len(result.Calls) != 1) || tc.name == "malformed required ACK" && (err == nil || len(result.Calls) != 0) {
					t.Fatalf("failed ACK changed parent outcome: result=%+v error=%v", result, err)
				}
				select {
				case <-worker.exited:
				case <-time.After(5 * time.Second):
					t.Fatal("child did not exit after failed ACK")
				}
				var exit *exec.ExitError
				if !errors.As(worker.waitErr, &exit) || exit.ExitCode() != 1 {
					t.Fatalf("failed ACK did not stop child with test failure: %v", worker.waitErr)
				}
				if _, err := os.Stat(filepath.Join(base, "second-pull")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("child pulled again after failed ACK: %v", err)
				}
				model, modelErr := os.ReadFile(filepath.Join(base, "model"))
				if tc.name == "missing outcome ACK" && (modelErr != nil || string(model) != "1") || tc.name == "malformed required ACK" && !errors.Is(modelErr, os.ErrNotExist) {
					t.Fatalf("failed ACK changed model calls: marker=%q error=%v", model, modelErr)
				}
				return
			}
			if tc.name == "known failure" {
				if err == nil || errors.Is(err, monitor.ErrExecutionOutcomeUnknown) || !result.matchesJob(job) || result.Answer != "" || len(result.Calls) != 1 {
					t.Fatalf("known failed action lost its outcome: result=%+v error=%v", result, err)
				}
			} else if err != nil || result.Answer != "Worker answer" || len(result.Calls) != 1 || result.Validate(job) != nil {
				t.Fatalf("preflighted child did not execute once: result=%+v error=%v", result, err)
			}
			if err := worker.WaitCapacity(ctx); err != nil {
				t.Fatalf("preflighted child did not hold another pull: %v", err)
			}
			job.Identity.JobID, job.Identity.AttemptID = "second-job", "second-attempt"
			result, err = worker.Execute(ctx, job, runnerExecutionCheckpoints{})
			if err != nil || result.Answer != "Worker answer" || len(result.Calls) != 1 || result.Validate(job) != nil {
				t.Fatalf("preflighted child did not execute second action: result=%+v error=%v", result, err)
			}
			if err := worker.WaitCapacity(ctx); err != nil {
				t.Fatalf("preflighted child did not hold idle pull: %v", err)
			}
			if err := worker.executor.Close(); err != nil {
				t.Fatalf("held-pull shutdown failed: %v", err)
			}
			select {
			case <-worker.exited:
			case <-ctx.Done():
				t.Fatalf("child did not exit after held-pull shutdown: %v", ctx.Err())
			}
			if tc.name == "ready" && worker.waitErr != nil {
				t.Fatalf("child did not consume idle shutdown: %v", worker.waitErr)
			}
			if worker.cmd.ProcessState == nil || !worker.cmd.ProcessState.Success() {
				t.Fatalf("direct child did not exit successfully: %v", worker.cmd.ProcessState)
			}
			if err := worker.Close(); err != nil {
				t.Fatalf("close exited worker: %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(base, "model")); err != nil || string(data) != "2" {
				t.Fatalf("model call marker = %q, %v", data, err)
			}
		})
	}
}

func TestSelectionPreflightDirectChildHelper(t *testing.T) {
	if os.Getenv("MINION_PREFLIGHT_CHILD") != "1" {
		return
	}
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	base := os.Getenv("MINION_PREFLIGHT_BASE")
	selection := WorkerSelection{Commands: map[string]config.CommandMapping{
		"comment": {Action: "comment", Skills: []string{testReviewSkillURL}},
	}, Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}}
	if os.Getenv("MINION_PREFLIGHT_CASE") == "audit tooling" {
		selection.Commands["security-audit"] = config.CommandMapping{Action: "security-audit", Skills: []string{testAuditSkillURL}}
	}
	if os.Getenv("MINION_PREFLIGHT_CASE") == "selected critic" {
		selection.Critic = &RoleSettings{Provider: "claude", Model: "critic-1", Effort: "medium"}
	}
	calls := 0
	runner, err := NewSelectedRunner(Runner{WorkRoot: filepath.Join(base, "work"), runModel: func(context.Context, modelRun) (aicli.Result, error) {
		calls++
		if err := workspace.WriteAtomic(filepath.Join(base, "model"), []byte(strconv.Itoa(calls))); err != nil {
			return aicli.Result{}, err
		}
		if os.Getenv("MINION_PREFLIGHT_CASE") == "known failure" && calls == 1 {
			return aicli.Result{}, &url.Error{Op: "Post", URL: "https://provider.invalid", Err: errors.New("fixture model failure")}
		}
		return aicli.Result{Answer: "Worker answer", UsageComplete: true}, nil
	}}, selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.PreflightSelection(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "minion preflight:", err)
		t.Fatalf("preflight: %v", err)
	}
	if err := workspace.WriteAtomic(filepath.Join(base, "pull"), []byte("ready")); err != nil {
		t.Fatal(err)
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
	caseName := os.Getenv("MINION_PREFLIGHT_CASE")
	if caseName == "missing outcome ACK" || caseName == "malformed required ACK" {
		transport := client.Transport
		pulls := 0
		client.Transport = httpClientTestTransport(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/next" {
				pulls++
				if pulls == 2 {
					if err := workspace.WriteAtomic(filepath.Join(base, "second-pull"), []byte("pulled")); err != nil {
						return nil, err
					}
				}
			}
			response, err := transport.RoundTrip(request)
			if err != nil {
				return response, err
			}
			if caseName == "missing outcome ACK" && request.URL.Path == "/outcome" || caseName == "malformed required ACK" && request.URL.Path == "/before" {
				_ = response.Body.Close()
				body := ""
				if caseName == "malformed required ACK" {
					body = "{"
				}
				response.Body = io.NopCloser(strings.NewReader(body))
			}
			return response, nil
		})
	}
	if err := RunHTTPLoop(ctx, client, access.BaseURL, "local", runner); err != nil {
		if caseName == "missing outcome ACK" || caseName == "malformed required ACK" {
			if !errors.Is(err, monitor.ErrExecutionOutcomeUnknown) {
				t.Fatalf("failed ACK did not report unknown outcome: %v", err)
			}
		}
		t.Fatal(err)
	}
	if caseName == "missing outcome ACK" || caseName == "malformed required ACK" {
		t.Fatal("failed ACK was treated as clean shutdown")
	}
}

func TestPreflightSelectionRedactsNativeCredentialFromCLIError(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	t.Setenv("CODEX_HOME", t.TempDir())
	const secret = "native-preflight-credential"
	const databasePassword = "database-credential"
	const proxy = "http://user:credential@proxy.example"
	t.Setenv("CODEX_API_KEY", secret)
	t.Setenv("PGPASSWORD", databasePassword)
	t.Setenv("HTTPS_PROXY", proxy)
	script := strings.Replace(preflightCodexCLI, "  app-server)\n", "  app-server)\n    printf '%s\\n' \"$CODEX_API_KEY $PGPASSWORD $HTTPS_PROXY\" >&2\n    exit 7\n", 1)
	fakeOnPath(t, "codex", script)
	runner := selectedPreflightRunner(t, map[string]config.CommandMapping{
		"review": {Action: string(command.Review), Skills: []string{testReviewSkillURL}},
	})
	err := runner.PreflightSelection(t.Context())
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), databasePassword) || strings.Contains(err.Error(), proxy) || err.Error() != "Codex model or invocation unsupported" {
		t.Fatalf("native credential escaped preflight error redaction: %v", err)
	}
}

const preflightCodexCLI = `#!/bin/sh
case "$1 $2" in
  'login status') exit 0 ;;
  'debug prompt-input')
    case " $* " in
      *' skills.include_instructions=true '*) printf '%s\n' '[{"role":"user","content":[{"text":"MINION_TRUSTED_PROMPT_78 MINION_UNTRUSTED_AGENTS_78 MINION_UNTRUSTED_SKILL_78"}]}]' ;;
      *) printf '%s\n' '[{"role":"user","content":[{"text":"MINION_TRUSTED_PROMPT_78"}]}]' ;;
    esac
    exit 0 ;;
esac
case "$1" in
  app-server)
    case " $* " in
      *' minion_unsupported_capability_probe=true '*) printf '%s\n' 'unknown config key: minion_unsupported_capability_probe' >&2; exit 2 ;;
    esac
    IFS= read -r request || exit 0
    printf '%s\n' '{"id":1,"result":{}}'
    IFS= read -r request || exit 1
    IFS= read -r request || exit 1
    printf '%s\n' '{"id":2,"result":{"data":[{"model":"main-1","isDefault":true,"defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}}'
    exec /bin/cat >/dev/null ;;
  exec)
    case " $* " in
      *' --minion-unsupported-capability-probe '*) printf '%s\n' 'unexpected argument --minion-unsupported-capability-probe' >&2; exit 2 ;;
    esac
    case "$2" in
      resume) printf '%s\n' 'Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]' ;;
      *) printf '%s\n' 'Usage: codex exec [OPTIONS] [PROMPT]' ;;
    esac
    exit 0 ;;
esac
exit 99
`

func TestOfferedCodexChoicesProbeSharedPrerequisitesOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, key := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "")
	}
	logPath := filepath.Join(t.TempDir(), "probes")
	script := strings.Replace(preflightCodexCLI, "#!/bin/sh\n", "#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$2\" >> "+shellQuote(logPath)+"\n", 1)
	script = strings.Replace(script, `"supportedReasoningEfforts":[{"reasoningEffort":"high"}]`,
		`"supportedReasoningEfforts":[{"reasoningEffort":"high"},{"reasoningEffort":"medium"}]`, 1)
	fakeOnPath(t, "codex", script)
	roles := []RoleSettings{{Provider: "codex", Model: "main-1", Effort: "high"}, {Provider: "codex", Model: "main-1", Effort: "medium"}}
	if message, err := checkOfferedRolesCLI(t.Context(), &Runner{}, roles); err != nil {
		t.Fatalf("offered roles: %s: %v", message, err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil || strings.Count(string(data), "login status\n") != 1 ||
		strings.Count(string(data), "debug prompt-input\n") != 3 || strings.Count(string(data), "app-server ") < 2 {
		t.Fatalf("provider and pair probes = %q, error=%v", data, err)
	}
}

func TestSelectedLocalPreflightRejectsUnsupportedOfferedPair(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	logPath := filepath.Join(t.TempDir(), "codex-calls")
	script := strings.Replace(preflightCodexCLI, "#!/bin/sh\n", "#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$2\" >> "+shellQuote(logPath)+"\n", 1)
	fakeOnPath(t, "codex", script)
	for _, selectedRole := range []string{"main", "critic"} {
		t.Run(selectedRole, func(t *testing.T) {
			selected := WorkerSelection{Commands: map[string]config.CommandMapping{
				"comment": {Action: string(command.Comment), Skills: []string{testReviewSkillURL}},
			}, Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}}
			alternate := []config.RoleModelConfig{{Model: "main-1", Effort: "medium"}}
			if selectedRole == "main" {
				selected.MainChoices = alternate
			} else {
				selected.Critic = &RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"}
				selected.CriticChoices = alternate
			}
			runner, err := NewSelectedRunner(Runner{WorkRoot: t.TempDir()}, selected)
			if err != nil {
				t.Fatal(err)
			}
			requirePreflightMessage(t, runner.PreflightSelection(t.Context()), preflightCodexInvocationUnsupported)
		})
	}
	calls, err := os.ReadFile(logPath)
	if err != nil || strings.Count(string(calls), "login status\n") != 2 {
		t.Fatalf("offered alternates were not checked before pull: calls=%q err=%v", calls, err)
	}
}

func TestSelectedLocalPreflightSkipsWithdrawnFrozenPair(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	fakeOnPath(t, "codex", preflightCodexCLI)
	selected := WorkerSelection{Commands: map[string]config.CommandMapping{
		"comment": {Action: string(command.Comment), Skills: []string{testReviewSkillURL}},
	}, Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"},
		RetainedMainChoices: []config.RoleModelConfig{{Model: "main-1", Effort: "medium"}}}
	runner, err := NewSelectedRunner(Runner{WorkRoot: t.TempDir()}, selected)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.PreflightSelection(t.Context()); err != nil {
		t.Fatalf("withdrawn frozen pair blocked startup: %v", err)
	}
	job := testJob(command.Comment)
	job.Main.Model, job.Main.Effort, job.Main.Skills = "main-1", "medium", []string{testReviewSkillURL}
	if _, err := runner.Run(t.Context(), job); !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), preflightCodexInvocationUnsupported) {
		t.Fatalf("unsupported frozen pair did not fail terminally: %v", err)
	}
}

func TestRunHTTPExcludesSelectedCLIFromInactivity(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	script := strings.Replace(preflightCodexCLI, "'login status') exit 0 ;;", "'login status') /bin/sleep 3; exit 0 ;;", 1)
	fakeOnPath(t, "codex", script)
	selected := WorkerSelection{Commands: map[string]config.CommandMapping{
		"comment": {Action: string(command.Comment), Skills: []string{testReviewSkillURL}},
	}, Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"},
		MainChoices: []config.RoleModelConfig{{Model: "main-1", Effort: "medium"}}}
	runner, err := NewSelectedRunner(Runner{WorkRoot: t.TempDir()}, selected)
	if err != nil {
		t.Fatal(err)
	}
	job := testJob(command.Comment)
	job.Main.Model, job.Main.Effort, job.Main.Skills = "main-1", "medium", []string{testReviewSkillURL}
	job.InactivityTimeout = 2 * time.Second
	parent := newHTTPServerTest(t, t.Context(), job, runnerExecutionCheckpoints{}, nil)
	client := parent.server.Client()
	client.Timeout = 10 * time.Second
	err = RunHTTP(t.Context(), client, parent.server.URL, job.Identity.ExecutorID, runner)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), preflightCodexInvocationUnsupported) ||
		errors.Is(err, errExecutorInactivity) {
		t.Fatalf("HTTP selected CLI check consumed model inactivity: %v", err)
	}
}

func TestSelectedRunnerSupportedAlternateReachesModelCall(t *testing.T) {
	installActionTestSkills(t, testReviewSkillURL)
	logPath := filepath.Join(t.TempDir(), "codex-calls")
	script := strings.Replace(preflightCodexCLI, "#!/bin/sh\n", "#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$2\" >> "+shellQuote(logPath)+"\n", 1)
	script = strings.Replace(script, `"supportedReasoningEfforts":[{"reasoningEffort":"high"}]`,
		`"supportedReasoningEfforts":[{"reasoningEffort":"high"},{"reasoningEffort":"medium"}]`, 1)
	fakeOnPath(t, "codex", script)
	selected := WorkerSelection{Commands: map[string]config.CommandMapping{
		"comment": {Action: string(command.Comment), Skills: []string{testReviewSkillURL}},
	}, Main: RoleSettings{Provider: "codex", Model: "main-1", Effort: "high"},
		MainChoices: []config.RoleModelConfig{{Model: "main-1", Effort: "medium"}}}
	modelCalls := 0
	runner, err := NewSelectedRunner(Runner{WorkRoot: t.TempDir(), remote: true,
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			modelCalls++
			if request.model != "main-1" || request.effort != "medium" {
				t.Fatalf("model received wrong approved pair: %s/%s", request.model, request.effort)
			}
			return aicli.Result{Answer: proposalJSON("Approved alternate ran.", "")}, nil
		}}, selected)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.PreflightSelection(t.Context()); err != nil {
		t.Fatalf("supported offered alternate failed before pull: %v", err)
	}
	job := testJob(command.Comment)
	job.Main.Model, job.Main.Effort, job.Main.Skills = "main-1", "medium", []string{testReviewSkillURL}
	result, err := runner.Run(t.Context(), job)
	if err != nil || modelCalls != 1 || result.Answer == "" {
		t.Fatalf("supported alternate did not reach model: result=%+v calls=%d err=%v", result, modelCalls, err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(calls), "app-server") {
		t.Fatalf("selected alternate skipped CLI validation: calls=%q err=%v", calls, err)
	}
}
