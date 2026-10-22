package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

const incompleteCritiqueJSON = `{"approved":false,"has_blockers":true,"review_complete":false,"feedback":"Assigned review remains unfinished."}`

func TestRunnerStopsOnIncompleteCriticReview(t *testing.T) {
	for _, position := range []struct {
		name, phase string
		round       int
		threshold   int
		sequence    []string
	}{
		{name: "first full", phase: reviewFull, round: 1, threshold: 1, sequence: []string{"main/1/", "critic/1/full"}},
		{name: "verification", phase: reviewVerification, round: 2, threshold: 2, sequence: []string{"main/1/", "critic/1/full", "main/2/", "critic/2/verification"}},
		{name: "full after verification", phase: reviewFull, round: 2, threshold: 2, sequence: []string{"main/1/", "critic/1/full", "main/2/", "critic/2/verification", "critic/2/full"}},
	} {
		for _, usageComplete := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/accounting=%t", position.name, usageComplete), func(t *testing.T) {
				job := withClaudeCritic(testJob(command.Comment))
				job.CriticMaxRounds, job.CriticNonblockingRounds, job.CriticConsecutiveNoBlockerRounds = 3, position.threshold, 0
				tokens := usage.Tokens{Input: 13, CachedInput: 3, Output: 5}
				var sequence []string
				var lastCheckpoint []CallUsage
				finalCheckpoint := false
				runner := Runner{WorkRoot: t.TempDir(), recordReview: func(calls []CallUsage, refs []Refutation, reason string) error {
					if reason != "" || len(refs) != 0 {
						t.Fatalf("incomplete review acquired a decision or refutation: reason=%q refs=%+v", reason, refs)
					}
					lastCheckpoint = append([]CallUsage(nil), calls...)
					if len(calls) == len(position.sequence) {
						last := calls[len(calls)-1]
						if !last.HasBlockers || last.Approved || last.NoBlockers || last.ReviewComplete {
							t.Fatalf("final checkpoint lost parsed incomplete verdict: %+v", last)
						}
						finalCheckpoint = true
					}
					return nil
				}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					sequence = append(sequence, fmt.Sprintf("%s/%d/%s", request.role, request.round, request.reviewPhase))
					if len(sequence) > len(position.sequence) {
						t.Fatalf("dispatched after incomplete review: %v", sequence)
					}
					answer := approvedCritiqueJSON
					switch {
					case request.role == roleMain:
						answer = proposalJSON("Candidate", "")
					case request.round == position.round && request.reviewPhase == position.phase:
						answer = incompleteCritiqueJSON
					case request.round == 1:
						answer = criticFeedbackJSON(false, "Correct the remaining detail.")
					}
					return aicli.Result{Answer: answer, Tokens: tokens, UsageComplete: usageComplete}, nil
				}}
				result, err := runner.Run(context.Background(), job)
				if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "critic review incomplete") || !slices.Equal(sequence, position.sequence) || len(result.Calls) != len(position.sequence) || result.Answer != "" || len(result.Changes) != 0 || len(result.Artifacts) != 0 || result.CriticStopReason != "" {
					t.Fatalf("incomplete review continued or published: sequence=%v result=%+v error=%v", sequence, result, err)
				}
				wantLast := CallUsage{Provider: job.Critic.Provider, Role: roleCritic, Round: position.round, ReviewPhase: position.phase, Tokens: tokens, HasBlockers: true, Complete: usageComplete}
				if last := result.Calls[len(result.Calls)-1]; last != wantLast || !finalCheckpoint || !slices.Equal(lastCheckpoint, result.Calls) {
					t.Fatalf("incomplete verdict or final checkpoint lost: last=%+v want=%+v checkpoint=%+v", last, wantLast, lastCheckpoint)
				}
				if err := result.validate(job, false); err != nil {
					t.Fatalf("invalid partial incomplete-review evidence: %v", err)
				}
				data, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				var replay Result
				if err := json.Unmarshal(data, &replay); err != nil || !reflect.DeepEqual(replay, result) || replay.validate(job, false) != nil {
					t.Fatalf("portable round trip lost incomplete review evidence: result=%+v error=%v", replay, err)
				}
				replay.Answer = "Forged complete answer"
				if err := replay.Validate(job); err == nil {
					t.Fatal("incomplete coverage authorized a complete publication result")
				}
				var journal bytes.Buffer
				orchestrator := Orchestrator{WorkRoot: runner.WorkRoot, Logger: log.New(&journal, "", 0)}
				orchestrator.logResult(job, result)
				lines := strings.Split(strings.TrimSpace(journal.String()), "\n")
				last := lines[len(lines)-1]
				wantVerdict := "review_phase=" + position.phase + ` approved=false no_blockers=false has_blockers=true review_complete=false verdict_allows_full=false no_blocker_verdict_streak=0 policy_met=false stop_reason=""`
				if len(lines) != len(position.sequence) || !strings.Contains(last, wantVerdict) || !strings.HasSuffix(last, fmt.Sprintf(" complete=%t", usageComplete)) || strings.Contains(journal.String(), "policy_met=true") {
					t.Fatalf("journal confused incomplete coverage with accounting or acceptance: %s", journal.String())
				}
				if position.round == 2 && (!strings.Contains(lines[1], "has_blockers=false review_complete=true") || !strings.Contains(lines[1], `no_blocker_verdict_streak=1 policy_met=false stop_reason=""`)) {
					t.Fatalf("round-two incomplete review did not reset an existing verdict streak: %s", journal.String())
				}
				if err := orchestrator.retainReviewEvidence(job, result); err != nil {
					t.Fatal(err)
				}
				if saved := readPrivateReview(t, runner.WorkRoot); saved.CriticStopReason != "" || !slices.Equal(saved.Calls, result.Calls) {
					t.Fatalf("retained ledger changed incomplete review evidence: %+v", saved)
				}
			})
		}
	}
}

func TestRunnerCompletedReviewDoesNotRequireUsageCompletion(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	calls := 0
	tokens := usage.Tokens{Input: 11, Output: 2}
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		calls++
		answer := approvedCritiqueJSON
		if request.role == roleMain {
			answer = proposalJSON("Complete answer", "")
		}
		return aicli.Result{Answer: answer, Tokens: tokens, UsageComplete: false}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if err != nil || calls != 2 || len(result.Calls) != 2 || result.Answer != "Complete answer" || result.CriticStopReason != "approved" {
		t.Fatalf("accounting incompleteness prevented completed review approval: result=%+v calls=%d error=%v", result, calls, err)
	}
	if last := result.Calls[1]; !last.Approved || !last.NoBlockers || last.HasBlockers || !last.ReviewComplete || last.Complete || last.Tokens != tokens || result.Calls[0].ReviewComplete {
		t.Fatalf("completed coverage was confused with usage completion: %+v", result.Calls)
	}
	if err := result.Validate(job); err != nil {
		t.Fatalf("completed review did not produce a publishable result: %v", err)
	}
}

func TestRunnerProviderErrorDoesNotAttestReviewCompletion(t *testing.T) {
	failure := errors.New("native provider stopped")
	for _, verdict := range []struct{ name, answer string }{
		{name: "completed shape", answer: approvedCritiqueJSON},
		{name: "incomplete shape", answer: incompleteCritiqueJSON},
	} {
		t.Run(verdict.name, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			calls := 0
			tokens := usage.Tokens{Input: 17, CachedInput: 3, Output: 7}
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				if request.role == roleMain {
					return aicli.Result{Answer: proposalJSON("Candidate", ""), UsageComplete: true}, nil
				}
				return aicli.Result{Answer: verdict.answer, Tokens: tokens, UsageComplete: true}, failure
			}}
			result, err := runner.Run(context.Background(), job)
			if !errors.Is(err, failure) || errors.Is(err, monitor.ErrTerminalJob) || calls != 2 || len(result.Calls) != 2 || result.Answer != "" || len(result.Changes) != 0 || len(result.Artifacts) != 0 || result.CriticStopReason != "" {
				t.Fatalf("failed provider output continued or became a review decision: result=%+v calls=%d error=%v", result, calls, err)
			}
			want := CallUsage{Provider: job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Tokens: tokens, Complete: true}
			if result.Calls[1] != want || result.validate(job, false) != nil {
				t.Fatalf("provider error acquired verdict flags or lost paid usage: result=%+v want=%+v", result, want)
			}
			var journal bytes.Buffer
			orchestrator := Orchestrator{WorkRoot: runner.WorkRoot, Logger: log.New(&journal, "", 0)}
			orchestrator.logResult(job, result)
			if !strings.Contains(journal.String(), `approved=false no_blockers=false has_blockers=false review_complete=false verdict_allows_full=false no_blocker_verdict_streak=0 policy_met=false stop_reason=""`) || !strings.HasSuffix(strings.TrimSpace(journal.String()), " complete=true") {
				t.Fatalf("journal promoted failed provider output: %s", journal.String())
			}
			if err := orchestrator.retainReviewEvidence(job, result); err != nil {
				t.Fatal(err)
			}
			if saved := readPrivateReview(t, runner.WorkRoot); saved.CriticStopReason != "" || !slices.Equal(saved.Calls, result.Calls) {
				t.Fatalf("retained ledger promoted failed provider output: %+v", saved)
			}
		})
	}
}
