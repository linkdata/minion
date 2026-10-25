package action

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workroot"
	"github.com/linkdata/minion/internal/workspace"
)

func TestRunnerNativeCleanupKeepsAcceptedAnswer(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires effective POSIX directory permissions and a non-root user")
	}
	for _, provider := range []string{"codex", "claude"} {
		for _, recovers := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recover=%t", provider, recovers), func(t *testing.T) {
				installActionTestSkills(t)
				t.Setenv("CODEX_HOME", t.TempDir())
				for _, key := range []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
					t.Setenv(key, "")
				}
				root := t.TempDir()
				t.Cleanup(func() {
					if err := os.Chmod(root, 0700); err != nil {
						t.Error(err)
					}
				})
				event := `printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"Done."}}' '{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":2}}'`
				if provider == "claude" {
					event = `previous=""; session=""
for arg do
 case "$previous" in --session-id|--resume) session="$arg";; esac
 previous="$arg"
done
printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$session"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","result":"Done.","modelUsage":{"fixture":{"inputTokens":3,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}'`
				}
				fakeOnPath(t, provider, "#!/bin/sh\nset -eu\ncat >/dev/null\n"+event+"\n/bin/chmod 0500 "+shellQuote(root)+"\n")
				job := testJob(command.Comment)
				job.Main.Provider = provider
				runner := Runner{WorkRoot: root, Claude: config.ClaudeConfig{APIKey: "inert-test-key"}}
				journal := &cleanupRetryLog{root: root}
				removed := 0
				if !recovers {
					journal.onRetry = func() (err error) {
						if err = os.Chmod(root, 0700); err != nil {
							return err
						}
						defer func() { err = errors.Join(err, os.Chmod(root, 0500)) }()
						entries, err := os.ReadDir(root)
						if err != nil {
							return err
						}
						// Remove ordinary paths here so only the second failed
						// native-session close can produce cleanup_failed.
						for _, entry := range entries {
							if strings.HasPrefix(entry.Name(), "minion-") || strings.HasPrefix(entry.Name(), ".minion-context-") {
								path := filepath.Join(root, entry.Name())
								if err := os.RemoveAll(path); err != nil {
									return err
								}
								removed++
							}
						}
						if removed != 2 {
							return fmt.Errorf("expected source and context directories, removed %d", removed)
						}
						return nil
					}
				}
				runner.Logger = log.New(journal, "", 0)
				result, err := runner.Run(t.Context(), job)
				if err != nil || result.Answer != "Done." || len(result.Calls) != 1 || result.Validate(job) != nil {
					t.Fatalf("session cleanup discarded accepted result: %+v,%v", result, err)
				}
				if journal.err != nil || strings.Count(journal.String(), "model session cleanup retry scheduled") != 1 {
					t.Fatalf("session cleanup retry was not exercised: %v, %q", journal.err, journal.String())
				}
				if recovers {
					if len(result.Diagnostics) != 0 {
						t.Fatalf("cleanup retry did not recover: %+v,%v", result, journal.err)
					}
					if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
						t.Fatalf("recovered cleanup left paths: %v,%v", entries, err)
					}
				} else {
					if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "cleanup_failed" {
						t.Fatalf("persistent cleanup was not reported: %+v", result.Diagnostics)
					}
					entries, err := os.ReadDir(root)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, entry := range entries {
						if strings.HasPrefix(entry.Name(), ".minion-"+provider+"-") {
							found = true
						}
					}
					if !found {
						t.Fatalf("native cleanup failure was not exercised: %v", entries)
					}
				}
			})
		}
	}
}

func TestRunnerTerminalFailureStaysTerminalWhenParentCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	root := t.TempDir()
	terminal := fmt.Errorf("%w: deterministic action refusal", monitor.ErrTerminalJob)
	runner := Runner{WorkRoot: root, runModel: func(context.Context, modelRun) (aicli.Result, error) {
		// The action has already failed terminally before shutdown arrives.
		cancel()
		return aicli.Result{}, terminal
	}}
	result, err := runner.Run(ctx, testJob(command.Comment))
	if !errors.Is(err, monitor.ErrTerminalJob) || errors.Is(err, context.Canceled) || result.Answer != "" || len(result.Calls) != 1 {
		t.Fatalf("late cancellation made a terminal attempt refundable: %+v,%v", result, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("terminal cleanup leaked: %v,%v", entries, err)
	}
}

func TestFixPreparationChecksDurablePrerequisitesBeforeGitHub(t *testing.T) {
	request := usageJob()
	request.SubjectType = "Issue"
	request.Command = command.Command{Name: "fix", Action: command.Fix}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("local prerequisite reached GitHub: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := testGitHub(server, "fixture")
	// Missing durable state must fail before any GitHub request.
	_, err := (&Orchestrator{GitHub: client}).prepareFix(t.Context(), &preparedAction{request: request})
	if err == nil || !strings.Contains(err.Error(), "no durable state or valid ID") {
		t.Fatalf("fix prerequisites: %v", err)
	}
	request.ID = "short"
	orchestrator := Orchestrator{Store: openState(t, time.Now()), GitHub: client}
	_, err = orchestrator.prepareFix(t.Context(), &preparedAction{request: request})
	if err == nil || !strings.Contains(err.Error(), "no durable state or valid ID") {
		t.Fatalf("short fix ID reached GitHub: %v", err)
	}
}

func TestAuditResultRejectsDuplicateAllowedArtifact(t *testing.T) {
	job := testJob(command.SecurityAudit)
	result := completeAuditResult(job)
	if err := result.Validate(job); err != nil {
		t.Fatal(err)
	}
	for i := range result.Artifacts {
		if result.Artifacts[i].Path == "FINDINGS-DETAIL.md" {
			result.Artifacts[i] = workspace.File{Path: "REPORT.md", Mode: workspace.Regular, Data: []byte(result.Answer)}
		}
	}
	for _, complete := range []bool{false, true} {
		if err := result.validate(job, complete); err == nil || !strings.Contains(err.Error(), "invalid action report artifact") {
			t.Fatalf("duplicate artifact complete=%t: %v", complete, err)
		}
	}
}

func TestRunnerScratchCreationUsesPreparationFailureClass(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires effective POSIX directory permissions and a non-root user")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(root, 0700); err != nil {
			t.Error(err)
		}
	})
	result, err := (&Runner{WorkRoot: root, runModel: forbidModel(t)}).Run(t.Context(), testJob(command.Comment))
	if !errors.Is(err, os.ErrPermission) || !errors.Is(err, workroot.ErrUnsafe) || errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "create source workspace") || len(result.Calls) != 0 {
		t.Fatalf("scratch creation classification: %+v,%v", result, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("failed creation left paths: %v,%v", entries, err)
	}
}

func TestRevisionInstructionsEvaluateProposedChanges(t *testing.T) {
	for _, test := range []struct{ prompt, rule string }{
		{revisionPrompt("Task", "Answer", "Finding"), "Evaluate each proposed change against the current code, connected callers, tests and task constraints"},
		{criticPrompt(reviewFull, "Task", "Answer", "", "", ""), "Evaluate the candidate's changes and the main model's refutations against the current code, connected callers, tests and task constraints"},
		{criticPrompt(reviewVerification, "Task", "Answer", "Earlier", "Finding", ""), "Evaluate the candidate's changes and the main model's refutations against the current code, connected callers, tests and task constraints"},
	} {
		if !strings.Contains(test.prompt, test.rule) {
			t.Fatalf("missing change-evaluation instruction: %s", test.prompt)
		}
	}
}

func TestRunnerCleansPartiallyCreatedCriticSnapshot(t *testing.T) {
	job := withClaudeCritic(testJob(command.Fix))
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "partial-snapshot")
	binary, err := aicli.AbsoluteExecutable("git")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$PWD\" in */minion-review-*) printf '%s' \"$PWD\" > " + shellQuote(marker) + "; exit 77;; esac\nexec " + shellQuote(binary) + " \"$@\"\n"
	fakeOnPath(t, "git", script)
	calls := 0
	runner := Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		calls++
		if request.role != roleMain {
			t.Fatal("critic ran after failed freeze")
		}
		writeFixtureFile(t, request.dir, "source.txt", []byte("candidate"))
		return aicli.Result{Answer: proposalJSON("Done.", "")}, nil
	}}
	result, err := runner.Run(t.Context(), job)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "freeze critic input") || calls != 1 || len(result.Calls) != 1 {
		t.Fatalf("partial freeze refusal: %+v,%v", result, err)
	}
	dir, err := os.ReadFile(marker)
	if err != nil || len(dir) == 0 {
		t.Fatalf("partial snapshot was never created: %q,%v", dir, err)
	}
	if _, err := os.Lstat(string(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial snapshot leaked: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("failed freeze leaked workspace: %v,%v", entries, err)
	}
}

func TestRunnerUsesTheProbedNodeAfterPathChanges(t *testing.T) {
	installActionTestSkills(t, testAuditSkillURL)
	first, second := filepath.Join(t.TempDir(), "first-node"), filepath.Join(t.TempDir(), "second-node")
	fakeOnPath(t, "node", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+shellQuote(first)+"\n")
	job := testJob(command.SecurityAudit)
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		fakeOnPath(t, "node", "#!/bin/sh\nprintf called > "+shellQuote(second)+"\nexit 77\n")
		for _, file := range auditArtifactsFixture(job, request.dir, "Audit report\n") {
			writeFixtureFile(t, request.outputDir, file.Path, file.Data)
		}
		return aicli.Result{Answer: "Audit report\n"}, nil
	}}
	result, err := runner.Run(t.Context(), job)
	if err != nil || result.Answer != "" {
		t.Fatalf("probed Node was not retained: %+v,%v", result, err)
	}
	data, err := os.ReadFile(first)
	if err != nil || !strings.Contains(string(data), " -e ") || !strings.Contains(string(data), "validate-findings.cjs") || !strings.Contains(string(data), "validate-coverage-ledger.cjs") {
		t.Fatalf("probed Node did not validate: %q,%v", data, err)
	}
	if _, err := os.Stat(second); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unprobed replacement Node ran: %v", err)
	}
}

func TestRunnerDeadlineDuringSessionCleanupClearsValidatedResult(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires effective POSIX directory permissions and a non-root user")
	}
	for _, test := range []struct {
		name   string
		action command.Action
		expire bool
	}{
		{name: "comment deadline", action: command.Comment, expire: true},
		{name: "fix before deadline", action: command.Fix},
		{name: "fix deadline", action: command.Fix, expire: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			action := test.action
			installActionTestSkills(t)
			t.Setenv("CODEX_HOME", t.TempDir())
			root := t.TempDir()
			t.Cleanup(func() {
				if err := os.Chmod(root, 0700); err != nil {
					t.Error(err)
				}
			})
			fakeOnPath(t, "codex", `#!/bin/sh
set -eu
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"Done."}}' '{"type":"turn.completed"}'
/bin/chmod 0500 `+shellQuote(root)+"\n")
			job := testJob(action)
			job.RuntimeBudget = 5 * time.Second
			if test.expire {
				job.RuntimeBudget = time.Second
			}
			var modelContext context.Context
			var changedSource string
			journal := &cleanupRetryLog{root: root, onRetry: func() error {
				if modelContext == nil {
					return errors.New("cleanup ran before a model context was captured")
				}
				if action == command.Fix {
					data, err := os.ReadFile(filepath.Join(changedSource, "source.txt"))
					if err != nil || string(data) != "fixed source" {
						return fmt.Errorf("cleanup ran without a changed fix source: %q, %v", data, err)
					}
				}
				// This retry runs only after Run assigned and validated its result.
				// Expire the action here so only the final deadline defer can clear it.
				if test.expire {
					<-modelContext.Done()
				}
				return os.Chmod(root, 0700)
			}}
			runner := Runner{WorkRoot: root, Logger: log.New(journal, "", 0), aroundModel: func(ctx context.Context, request modelRun, next modelCall) (aicli.Result, error) {
				modelContext = ctx
				if action == command.Fix {
					changedSource = request.dir
					writeFixtureFile(t, changedSource, "source.txt", []byte("fixed source"))
				}
				return next(ctx, request)
			}}
			result, err := runner.Run(t.Context(), job)
			if !test.expire {
				if err != nil || result.Answer != "Done." || len(result.Changes) != 1 || string(result.Changes[0].Data) != "fixed source" || journal.err != nil {
					t.Fatalf("changed fix did not produce a publishable result before the deadline: %+v, %v; cleanup=%v", result, err, journal.err)
				}
				assertEmptyDir(t, root)
				return
			}
			wantError := errors.Join(monitor.ErrTerminalJob, context.DeadlineExceeded).Error()
			if err == nil || err.Error() != wantError || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, monitor.ErrTerminalJob) || result.Answer != "" || len(result.Changes) != 0 || len(result.Calls) != 1 || len(result.Diagnostics) != 0 || journal.err != nil || strings.Count(journal.String(), "model session cleanup retry scheduled") != 1 {
				t.Fatalf("deadline after result validation was publishable: %+v, %v; cleanup=%v log=%q", result, err, journal.err, journal.String())
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
				t.Fatalf("deadline cleanup leaked: %v,%v", entries, err)
			}
		})
	}
}
