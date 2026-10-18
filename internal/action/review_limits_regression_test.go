package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

func TestReviewRoundBudgetIncludesEveryFreshFullRejection(t *testing.T) {
	for _, rounds := range []int{1, 2, config.DefaultCriticMaxRounds, config.MaxCriticMaxRounds} {
		t.Run(fmt.Sprint(rounds), func(t *testing.T) {
			job := Job{Main: RoleSettings{Provider: "codex"}, Critic: &RoleSettings{Provider: "claude"}, CriticMaxRounds: rounds}
			calls := 0
			runner := Runner{runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				answer := approvedCritiqueJSON
				if request.role == roleMain {
					answer = proposalJSON("Candidate", "")
				} else if request.reviewPhase == reviewFull {
					answer = `{"approved":false,"has_blockers":true,"feedback":"Fresh full finding."}`
				}
				return aicli.Result{Answer: answer}, nil
			}}
			answer, err := runner.runAI(context.Background(), job, "source", "Complete the task.", "", nil, nil)
			if !errors.Is(err, monitor.ErrTerminalJob) || answer != "" || calls != 3*rounds-1 || !strings.Contains(err.Error(), fmt.Sprintf("after %d round(s)", rounds)) {
				t.Fatalf("round budget: answer=%q error=%v calls=%d want=%d", answer, err, calls, 3*rounds-1)
			}
		})
	}
}

func TestEveryActionPromptDeclaresRenderedPublicationBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	for _, action := range []command.Action{command.Review, command.Fix, command.Comment, command.SecurityAudit} {
		for _, critic := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/critic=%t", action, critic), func(t *testing.T) {
				job := portableJob(action)
				if critic {
					job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, 2
				}
				prompt, err := actionPrompt(ctx, job, "/source", "/context", "/reports", "/installed/security-audit/SKILL.md")
				if err != nil {
					t.Fatal(err)
				}
				for _, requirement := range []string{
					"Final response text must fit within 59392 UTF-8 bytes",
					"after line-ending normalization, a two-byte quote prefix on every line and 12 bytes of code fences",
					"oversized published text is refused rather than truncated",
					"whole action must finish before " + deadline.UTC().Format(time.RFC3339),
				} {
					if !strings.Contains(prompt, requirement) {
						t.Fatalf("%s prompt omits %q", action, requirement)
					}
				}
				if !strings.Contains(prompt, "Await all delegated work and background checks before your final response") || !strings.Contains(prompt, "inspect their results and handle failures instead of ending with a waiting or progress message") {
					t.Fatal("action prompt omits background completion requirements")
				}
				if critic {
					if !strings.Contains(mainPrompt(prompt), "source changes") {
						t.Fatal("main prompt omits private-commentary restriction")
					}
					for _, phase := range []string{reviewFull, reviewVerification} {
						critique := criticPrompt(phase, prompt, "Candidate", "", "", "/frozen-reports")
						for _, requirement := range []string{"Await every delegated review and background check before your final verdict", "inspect their results and handle failures instead of ending with a waiting or progress message", "source changes", "The complete JSON object must fit within 65536 UTF-8 bytes, including syntax and escaping", "without omitting actionable issues or truncating the verdict"} {
							if !strings.Contains(critique, requirement) {
								t.Fatalf("%s critic omits %q", phase, requirement)
							}
						}
					}
				}
				if action == command.Comment && (!strings.Contains(prompt, "Use repository-relative paths and source identity") || strings.Contains(prompt, "/source")) {
					t.Fatal("comment prompt leaks host source path or omits path guidance")
				}
				if strings.Contains(prompt, "leave time for subsequent critic reviews") != critic {
					t.Fatal("prompt misstates whether the action needs critic time")
				}
				if action == command.Fix {
					for _, requirement := range []string{
						"complete contents of all changed files, including mode-only changes, must fit 1 MiB",
						"Git binary patch must also fit 1 MiB, including removed lines, deleted-file contents and binary encoding overhead",
						"All changes, including deletions, are limited to 100 paths",
					} {
						if !strings.Contains(prompt, requirement) {
							t.Fatalf("fix prompt omits %q", requirement)
						}
					}
				}
				if action == command.SecurityAudit {
					for _, requirement := range []string{
						"REPORT.md must fit within 59392 UTF-8 bytes after line-ending normalization, a two-byte quote prefix per line and 12 bytes of code fences",
						"Do not execute repository code, builds, tests, or fixtures, probe live services, publish, or run network commands",
						"Copy this source-only execution prohibition verbatim into every delegated agent prompt",
						"it overrides the skill's local-execution allowance",
						"selected security-audit domain companion files",
						`"When to use" conditions apply`,
						"Exclude its main SKILL.md and independently configured skills",
						"Use [] when no domain companion applies",
						"installed validation and publication",
					} {
						if !strings.Contains(prompt, requirement) {
							t.Fatalf("audit prompt omits %q", requirement)
						}
					}
					for _, phase := range []string{reviewFull, reviewVerification} {
						critique := criticPrompt(phase, prompt, "Audit candidate", "", "", "/frozen-reports")
						if !strings.Contains(critique, "Copy any source-only execution prohibition from the task verbatim into every delegated review prompt") || !strings.Contains(critique, "Do not execute repository code, builds, tests, or fixtures, probe live services, publish, or run network commands") {
							t.Fatalf("%s critic lost the audit's inherited delegate execution prohibition", phase)
						}
					}
				}
			})
		}
	}
}

func TestNoCriticRunnerKeepsChangedFixWithUnpublishedOversizedSummary(t *testing.T) {
	// The provider seam returns data directly; no native CLI or model is invoked.
	for _, summary := range []struct{ name, text string }{
		{"raw byte overflow", strings.Repeat("x", maxOutputBytes+1)},
		{"quote expansion overflow", strings.Repeat("a\n", 20000)},
	} {
		for _, action := range []command.Action{command.Fix, command.Comment} {
			t.Run(fmt.Sprintf("%s/%s", action, summary.name), func(t *testing.T) {
				job := portableJob(action)
				calls := 0
				runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					calls++
					if request.role != roleMain || request.outputSchema != "" || request.provider != job.Main.Provider || request.writable != (action == command.Fix) {
						t.Fatalf("unexpected no-critic call: %+v", request)
					}
					if action == command.Fix {
						if err := os.WriteFile(filepath.Join(request.dir, "source.txt"), []byte("changed\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return aicli.Result{Answer: summary.text, UsageComplete: true}, nil
				}}
				result, err := runner.Run(context.Background(), job)
				if calls != 1 || len(result.Calls) != 1 || result.Calls[0].Role != roleMain {
					t.Fatalf("unexpected dispatch evidence: calls=%d result=%+v err=%v", calls, result, err)
				}
				if action == command.Fix {
					if err != nil || result.Validate(job) != nil || result.Answer != "" || len(result.Changes) != 1 || result.Changes[0].Path != "source.txt" || result.Changes[0].Mode != workspace.Regular || string(result.Changes[0].Data) != "changed\n" {
						t.Fatalf("valid fix discarded with unused summary: result=%+v error=%v", result, err)
					}
				} else if !errors.Is(err, monitor.ErrTerminalJob) || result.Answer != "" || len(result.Changes) != 0 {
					t.Fatalf("unpublishable comment accepted: result=%+v error=%v", result, err)
				}
			})
		}
	}
}
