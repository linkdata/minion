package action

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

var defaultActionTestCommands = map[string]command.Action{
	"review": command.Review, "fix": command.Fix, "audit": command.Comment, "security-audit": command.SecurityAudit,
}

func TestRunnerNativeCleanupKeepsAcceptedAnswer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	for _, provider := range []string{"codex", "claude"} {
		for _, recover := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recover=%t", provider, recover), func(t *testing.T) {
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
				event := `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"Done."}}' '{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":2}}'`
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
				job := portableJob(command.Comment)
				job.Main.Provider = provider
				runner := Runner{WorkRoot: root, Claude: config.ClaudeConfig{APIKey: "inert-test-key"}}
				journal := &cleanupRetryLog{root: root}
				if recover {
					runner.Logger = log.New(journal, "", 0)
				}
				result, err := runner.Run(context.Background(), job)
				if err != nil || result.Answer != "Done." || len(result.Calls) != 1 || result.Validate(job) != nil {
					t.Fatalf("session cleanup discarded accepted result: %+v,%v", result, err)
				}
				if recover {
					if journal.err != nil || len(result.Diagnostics) != 0 {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	terminal := fmt.Errorf("%w: deterministic action refusal", monitor.ErrTerminalJob)
	runner := Runner{WorkRoot: root, runModel: func(context.Context, modelRun) (aicli.Result, error) {
		// The action has already failed terminally before shutdown arrives.
		cancel()
		return aicli.Result{}, terminal
	}}
	result, err := runner.Run(ctx, portableJob(command.Comment))
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
	// No GitHub client: any request before the local prerequisite check is an error.
	_, err := (&Orchestrator{}).prepareFix(context.Background(), &preparedAction{request: request})
	if err == nil || !strings.Contains(err.Error(), "no durable state or valid ID") {
		t.Fatalf("fix prerequisites: %v", err)
	}
}

func TestAuditResultRejectsDuplicateAllowedArtifact(t *testing.T) {
	job := portableJob(command.SecurityAudit)
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
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
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
	result, err := (&Runner{WorkRoot: root, runModel: forbidModel(t)}).Run(context.Background(), portableJob(command.Comment))
	if !errors.Is(err, os.ErrPermission) || !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "create source workspace") || len(result.Calls) != 0 {
		t.Fatalf("scratch creation classification: %+v,%v", result, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("failed creation left paths: %v,%v", entries, err)
	}
}

func TestRevisionInstructionsEvaluateProposedChanges(t *testing.T) {
	const rule = "Evaluate each proposed change against the current code, connected callers, tests and task constraints"
	for _, prompt := range []string{revisionPrompt("Task", "Answer", "Finding"), criticPrompt(reviewFull, "Task", "Answer", "", "", ""), criticPrompt(reviewVerification, "Task", "Answer", "Earlier", "Finding", "")} {
		if !strings.Contains(prompt, rule) {
			t.Fatalf("missing change-evaluation instruction: %s", prompt)
		}
	}
}

func TestRunnerCleansPartiallyCreatedCriticSnapshot(t *testing.T) {
	job := withClaudeCritic(portableJob(command.Fix))
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
	result, err := runner.Run(context.Background(), job)
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
	job := portableJob(command.SecurityAudit)
	job.Main.Skills = []string{testAuditSkillURL}
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		fakeOnPath(t, "node", "#!/bin/sh\nprintf called > "+shellQuote(second)+"\nexit 77\n")
		for _, file := range auditArtifactsFixture(job, request.dir, "Audit report\n") {
			writeFixtureFile(t, request.outputDir, file.Path, file.Data)
		}
		return aicli.Result{Answer: "Audit report\n"}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || result.Answer != "Audit report\n" {
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

func TestStatusRejectsNegativeRoundsForTerminalAndCleanupPhases(t *testing.T) {
	job := portableJob(command.Comment)
	for _, status := range []Status{
		{Version: ProtocolVersion, Identity: job.Identity, State: StatusStopping, Phase: PhaseCleanup, Round: -1},
		{Version: ProtocolVersion, Identity: job.Identity, State: StatusFailed, Phase: PhaseFinished, Round: -1},
	} {
		if err := status.Validate(job); err == nil || err.Error() != "invalid action status" {
			t.Fatalf("negative round accepted or failed earlier: %+v,%v", status, err)
		}
	}
}
