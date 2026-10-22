package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func criticFeedbackJSON(hasBlockers bool, feedback string) string {
	data, _ := json.Marshal(map[string]any{"approved": false, "has_blockers": hasBlockers, "review_complete": true, "feedback": feedback})
	return string(data)
}

func TestRunnerRejectsCriticFeedbackBlankAfterSanitization(t *testing.T) {
	for _, phase := range []string{reviewFull, reviewVerification} {
		for _, feedback := range []struct {
			name, text, codexKey, claudeKey string
		}{
			{name: "NUL only", text: "\x00"},
			{name: "whitespace and NUL", text: " \t\x00\n "},
			{name: "fail closed redaction", text: "private # and redacted", codexKey: "#", claudeKey: "redacted"},
		} {
			for _, blockers := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/blockers=%t", phase, feedback.name, blockers), func(t *testing.T) {
					job := withClaudeCritic(testJob(command.Comment))
					job.CriticMaxRounds, job.CriticNonblockingRounds, job.CriticConsecutiveNoBlockerRounds = 3, 1, 0
					if phase == reviewVerification {
						job.CriticNonblockingRounds = 2
					}
					var sequence []string
					runner := Runner{WorkRoot: t.TempDir()}
					runner.Codex.APIKey, runner.Claude.APIKey = feedback.codexKey, feedback.claudeKey
					runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
						sequence = append(sequence, fmt.Sprintf("%s/%d/%s", request.role, request.round, request.reviewPhase))
						if request.role == roleMain {
							return aicli.Result{Answer: proposalJSON("Candidate", "")}, nil
						}
						if phase == reviewVerification && request.round == 1 {
							return aicli.Result{Answer: criticFeedbackJSON(false, "Revise candidate.")}, nil
						}
						return aicli.Result{Answer: criticFeedbackJSON(blockers, feedback.text)}, nil
					}
					result, err := runner.Run(context.Background(), job)
					wantSequence := []string{"main/1/", "critic/1/full"}
					if phase == reviewVerification {
						wantSequence = append(wantSequence, "main/2/", "critic/2/verification")
					}
					if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "invalid critic verdict: rejection feedback is blank after redaction") || !slices.Equal(sequence, wantSequence) || len(result.Calls) != len(wantSequence) || result.Answer != "" || len(result.Changes) != 0 || len(result.Artifacts) != 0 || result.CriticStopReason != "" {
						t.Fatalf("blank sanitized verdict continued or published: sequence=%v result=%+v error=%v", sequence, result, err)
					}
					last := result.Calls[len(result.Calls)-1]
					if last.ReviewPhase != phase || last.Approved || last.NoBlockers || last.HasBlockers || last.ReviewComplete {
						t.Fatalf("invalid sanitized verdict acquired dispatch flags: %+v", last)
					}
					if err := result.validate(job, false); err != nil {
						t.Fatalf("invalid partial receipts: %v", err)
					}
					publishable := result
					publishable.Answer = "Candidate"
					if err := publishable.Validate(job); err == nil {
						t.Fatal("blank sanitized verdict permitted a complete publication result")
					}
					var journal bytes.Buffer
					orchestrator := Orchestrator{WorkRoot: runner.WorkRoot, Logger: log.New(&journal, "", 0)}
					orchestrator.logResult(job, result)
					wantLast := "review_phase=" + phase + ` approved=false no_blockers=false has_blockers=false review_complete=false verdict_allows_full=false no_blocker_verdict_streak=0 policy_met=false stop_reason=""`
					lines := strings.Split(strings.TrimSpace(journal.String()), "\n")
					if len(lines) != len(wantSequence) || !strings.Contains(lines[len(lines)-1], wantLast) || strings.Contains(journal.String(), "policy_met=true") {
						t.Fatalf("journal promoted invalid sanitized feedback: %s", journal.String())
					}
					if phase == reviewVerification && !strings.Contains(lines[1], `no_blocker_verdict_streak=1 policy_met=false stop_reason=""`) {
						t.Fatalf("verification did not reset an existing no-blocker verdict streak: %s", journal.String())
					}
					if err := orchestrator.retainReviewEvidence(job, result); err != nil {
						t.Fatal(err)
					}
					if saved := readPrivateReview(t, runner.WorkRoot); saved.CriticStopReason != "" || !slices.Equal(saved.Calls, result.Calls) {
						t.Fatalf("retained review promoted invalid sanitized feedback: %+v", saved)
					}
				})
			}
		}
	}
}

func TestRunnerPreservesSanitizedCriticFeedbackIntoRevision(t *testing.T) {
	const codexKey, claudeKey = "main-key", "critic-key"
	const feedback = "Repair the de\x00fect using main-\x00key and critic-\x00key."
	const sanitized = "Repair the defect using [redacted] and [redacted]."
	for _, phase := range []string{reviewFull, reviewVerification} {
		t.Run(phase, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			job.CriticMaxRounds = 3
			targetRound := 1
			if phase == reviewVerification {
				targetRound = 2
			}
			var sequence []string
			revised := false
			runner := Runner{WorkRoot: t.TempDir()}
			runner.Codex.APIKey, runner.Claude.APIKey = codexKey, claudeKey
			runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
				sequence = append(sequence, fmt.Sprintf("%s/%d/%s", request.role, request.round, request.reviewPhase))
				if request.role == roleMain {
					if request.round == targetRound+1 {
						revised = true
						if !strings.Contains(request.prompt, quotedContext(phase+" review:\n"+sanitized)) || strings.Contains(request.prompt, codexKey) || strings.Contains(request.prompt, claudeKey) || strings.Contains(request.prompt, "\x00") {
							t.Fatalf("revision lost sanitized feedback or received secrets: %q", request.prompt)
						}
					}
					return aicli.Result{Answer: proposalJSON(fmt.Sprintf("Candidate %d", request.round), "")}, nil
				}
				if request.round == targetRound && request.reviewPhase == phase {
					return aicli.Result{Answer: criticFeedbackJSON(true, feedback)}, nil
				}
				if request.round == 1 {
					return aicli.Result{Answer: criticFeedbackJSON(true, "Revise candidate.")}, nil
				}
				return aicli.Result{Answer: approvedCritiqueJSON}, nil
			}
			result, err := runner.Run(context.Background(), job)
			wantSequence := []string{"main/1/", "critic/1/full", "main/2/", "critic/2/verification", "critic/2/full"}
			if phase == reviewVerification {
				wantSequence = []string{"main/1/", "critic/1/full", "main/2/", "critic/2/verification", "main/3/", "critic/3/verification", "critic/3/full"}
			}
			if err != nil || !revised || !slices.Equal(sequence, wantSequence) || len(result.Calls) != len(wantSequence) || result.CriticStopReason != "approved" || result.Answer != fmt.Sprintf("Candidate %d", targetRound+1) {
				t.Fatalf("sanitized feedback did not reach an approved revision: sequence=%v result=%+v error=%v", sequence, result, err)
			}
			if err := result.Validate(job); err != nil {
				t.Fatal(err)
			}
			verdict := result.Calls[2*targetRound-1]
			if verdict.Approved || verdict.NoBlockers || !verdict.HasBlockers || !verdict.ReviewComplete {
				t.Fatalf("valid sanitized rejection lost its blocker verdict: %+v", verdict)
			}
			var journal bytes.Buffer
			orchestrator := Orchestrator{WorkRoot: runner.WorkRoot, Logger: log.New(&journal, "", 0)}
			orchestrator.logResult(job, result)
			lines := strings.Split(strings.TrimSpace(journal.String()), "\n")
			if len(lines) != len(wantSequence) || strings.Count(journal.String(), `policy_met=true stop_reason="approved"`) != 1 || !strings.Contains(lines[len(lines)-1], `policy_met=true stop_reason="approved"`) {
				t.Fatalf("journal lost the final approved decision: %s", journal.String())
			}
			if err := orchestrator.retainReviewEvidence(job, result); err != nil {
				t.Fatal(err)
			}
			if saved := readPrivateReview(t, runner.WorkRoot); saved.CriticStopReason != "approved" || !slices.Equal(saved.Calls, result.Calls) {
				t.Fatalf("retained review lost sanitized verdict evidence: %+v", saved)
			}
		})
	}
}
