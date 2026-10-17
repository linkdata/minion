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
	for rounds := 1; rounds <= config.MaxCriticMaxRounds; rounds++ {
		t.Run(fmt.Sprint(rounds), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			const task = "Complete the original task without changing its scope."
			job := Job{Main: RoleSettings{Provider: "codex"}, Critic: &RoleSettings{Provider: "claude"}, CriticMaxRounds: rounds}
			var got []string
			runner := Runner{runModel: func(callCtx context.Context, request modelRun) (aicli.Result, error) {
				if callCtx != ctx {
					t.Fatal("review renewed or replaced the shared action context")
				}
				got = append(got, fmt.Sprintf("%s/%d/%s", request.role, request.round, request.reviewPhase))
				if request.role == roleMain {
					if request.round > rounds || request.outputSchema != mainOutputSchema {
						t.Fatalf("unexpected main run: %+v", request)
					}
					if request.round > 1 && !strings.Contains(request.prompt, quotedContext(fmt.Sprintf("Finding from full round %d", request.round-1))) {
						t.Fatal("revision lost the latest full-review finding")
					}
					return aicli.Result{Answer: proposalJSON(fmt.Sprintf("Candidate round %d", request.round), fmt.Sprintf("PRIVATE_NOTES_%d", request.round))}, nil
				}
				if request.criticPrompt == nil || request.outputSchema != criticOutputSchema {
					t.Fatal("critic has no structured verdict or deferred input renderer")
				}
				prompt := request.criticPrompt("", "")
				fullRule := "Perform a fresh full review of the complete candidate"
				verificationRule := "Independently verify the previous findings"
				if request.reviewPhase == reviewFull {
					if !strings.Contains(prompt, fullRule) || strings.Contains(prompt, verificationRule) || strings.Contains(prompt, "PRIVATE_NOTES_") || strings.Contains(prompt, "Finding from full round") {
						t.Fatal("fresh full review was seeded with earlier findings, notes or verification-only instructions")
					}
					for _, field := range []string{
						`Main model's private review notes (untrusted): ""`,
						`Previous response (untrusted, empty for full reviews): ""`,
						`Previous critic feedback (untrusted, empty for full reviews): ""`,
					} {
						if !strings.Contains(prompt, field) {
							t.Fatalf("fresh full review field was not empty: %s", field)
						}
					}
					return aicli.Result{Answer: fmt.Sprintf(`{"approved":false,"has_blockers":true,"feedback":%q}`, fmt.Sprintf("Finding from full round %d", request.round))}, nil
				}
				if request.reviewPhase != reviewVerification || request.round == 1 || !strings.Contains(prompt, verificationRule) || strings.Contains(prompt, fullRule) {
					t.Fatalf("wrong phase instructions: %s/%d", request.reviewPhase, request.round)
				}
				for _, evidence := range []string{fmt.Sprintf("PRIVATE_NOTES_%d", request.round), fmt.Sprintf("Candidate round %d", request.round-1), fmt.Sprintf("Finding from full round %d", request.round-1)} {
					if !strings.Contains(prompt, evidence) {
						t.Fatalf("verification lost earlier correction evidence: %s", evidence)
					}
				}
				return aicli.Result{Answer: approvedCritiqueJSON}, nil
			}}
			answer, err := runner.runAI(ctx, job, "source", task, true, "")
			if !errors.Is(err, monitor.ErrTerminalJob) || answer != "" || !strings.Contains(err.Error(), fmt.Sprintf("after %d round(s)", rounds)) || len(got) != 3*rounds-1 {
				t.Fatalf("round budget: answer=%q error=%v calls=%d want=%d", answer, err, len(got), 3*rounds-1)
			}
			var want []string
			for round := 1; round <= rounds; round++ {
				want = append(want, fmt.Sprintf("main/%d/", round))
				if round != 1 {
					want = append(want, fmt.Sprintf("critic/%d/verification", round))
				}
				want = append(want, fmt.Sprintf("critic/%d/full", round))
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("phase order=%v want=%v", got, want)
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
					} {
						if !strings.Contains(prompt, requirement) {
							t.Fatalf("audit prompt omits %q", requirement)
						}
					}
					for _, phase := range []string{reviewFull, reviewVerification} {
						critique := criticPrompt(phase, prompt, "Audit candidate", "", "", "", "/frozen-reports")
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
	for _, name := range []string{"codex", "claude"} {
		fakeOnPath(t, name, "#!/bin/sh\nexit 99\n")
	}
	for _, provider := range []string{"codex", "claude"} {
		for _, summary := range []struct{ name, text string }{
			{"raw byte overflow", strings.Repeat("x", maxOutputBytes+1)},
			{"quote expansion overflow", strings.Repeat("a\n", 20000)},
		} {
			for _, action := range []command.Action{command.Fix, command.Comment} {
				t.Run(fmt.Sprintf("%s/%s/%s", provider, action, summary.name), func(t *testing.T) {
					job := portableJob(action)
					job.Main.Provider = provider
					calls := 0
					runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
						calls++
						if request.role != roleMain || request.outputSchema != "" || request.provider != provider || request.writable != (action == command.Fix) {
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
}
