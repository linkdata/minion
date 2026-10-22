package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
)

func TestCriticEvidencePreservesBlockerVerdicts(t *testing.T) {
	failed := errors.New("provider failed")
	for _, tt := range []struct {
		name, answer                      string
		err                               error
		approved, noBlockers, hasBlockers bool
	}{
		{name: "approved", answer: approvedCritiqueJSON, approved: true, noBlockers: true},
		{name: "nonblocking rejection", answer: `{"approved":false,"has_blockers":false,"feedback":"Nonblocking nit."}`, noBlockers: true},
		{name: "blocking rejection", answer: `{"approved":false,"has_blockers":true,"feedback":"Blocking failure."}`, hasBlockers: true},
		{name: "invalid verdict", answer: `garbage`},
		{name: "failed approval", answer: approvedCritiqueJSON, err: failed},
		{name: "failed nonblocking", answer: `{"approved":false,"has_blockers":false,"feedback":"Nonblocking nit."}`, err: failed},
		{name: "failed blocking", answer: `{"approved":false,"has_blockers":true,"feedback":"Blocking failure."}`, err: failed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := testJob(command.Comment)
			job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, 1
			// Even a threshold that permits nits cannot accept failed provider output.
			if tt.err != nil {
				job.CriticNonblockingRounds = 1
			}
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				if request.role == roleMain {
					return aicli.Result{Answer: proposalJSON("Candidate answer", "")}, nil
				}
				return aicli.Result{Answer: tt.answer, Tokens: usage.Tokens{Input: 11, Output: 2}}, tt.err
			}}
			result, err := runner.Run(context.Background(), job)
			if (err == nil) != tt.approved || (tt.err != nil && !errors.Is(err, tt.err)) || len(result.Calls) != 2 || result.validate(job, false) != nil {
				t.Fatalf("invalid critic outcome: result=%+v err=%v", result, err)
			}
			if !tt.approved && tt.err == nil && !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("critic rejection or invalid verdict was retryable: %v", err)
			}
			if tt.approved {
				if result.CriticStopReason != "approved" || result.Answer != "Candidate answer" || result.Validate(job) != nil {
					t.Fatalf("approved result invalid: %+v", result)
				}
			} else if result.CriticStopReason != "" || result.Answer != "" {
				t.Fatalf("failed critic published: %+v", result)
			}
			want := CallUsage{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Tokens: usage.Tokens{Input: 11, Output: 2}, Approved: tt.approved, NoBlockers: tt.noBlockers, HasBlockers: tt.hasBlockers}
			if result.Calls[1] != want {
				t.Fatalf("native verdict lost: got=%+v want=%+v", result.Calls[1], want)
			}
		})
	}
}

func TestJournalPolicyIsNotPublication(t *testing.T) {
	job := testJob(command.Comment)
	job.Critic, job.CriticMaxRounds, job.CriticNonblockingRounds = &RoleSettings{Provider: "claude"}, 2, 2
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Complete answer", Calls: []CallUsage{
		{Provider: job.Main.Provider, Role: roleMain, Round: 1},
		{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, HasBlockers: true},
		{Provider: job.Main.Provider, Role: roleMain, Round: 2},
		{Provider: "claude", Role: roleCritic, Round: 2, ReviewPhase: reviewVerification, NoBlockers: true},
		{Provider: "claude", Role: roleCritic, Round: 2, ReviewPhase: reviewFull, HasBlockers: true},
	}}
	var output bytes.Buffer
	(&Orchestrator{Logger: log.New(&output, "", 0)}).logResult(job, result)
	text := output.String()
	if !strings.Contains(text, `review_phase=verification approved=false no_blockers=true has_blockers=false verdict_allows_full=true no_blocker_verdict_streak=0 policy_met=false stop_reason=""`) ||
		!strings.Contains(text, `review_phase=full approved=false no_blockers=false has_blockers=true verdict_allows_full=false no_blocker_verdict_streak=0 policy_met=false stop_reason=""`) || strings.Contains(text, "accepted=") {
		t.Fatalf("journal blurred verdict policy and publication: %s", text)
	}
	if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "accepted final full review") {
		t.Fatalf("verification policy was mistaken for an accepted final result: %v", err)
	}
}

func TestRunnerNonblockingThresholdRequiresFreshFullReview(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		threshold, rounds    int
		fullBlockers         bool
		verificationBlockers bool
		missingBlockers      bool
		wantCalls            int
		wantSuccess          bool
	}{
		{name: "first full at hard maximum can accept nits", threshold: 1, rounds: 1, wantCalls: 2, wantSuccess: true},
		{name: "threshold follows required revision", threshold: 2, rounds: 5, wantCalls: 5, wantSuccess: true},
		{name: "last allowed proposal can accept", threshold: 3, rounds: 3, wantCalls: 8, wantSuccess: true},
		{name: "zero remains strict", rounds: 2, wantCalls: 5},
		{name: "full blockers remain blocking", threshold: 1, rounds: 2, fullBlockers: true, wantCalls: 5},
		{name: "verification blockers require revision", threshold: 1, rounds: 2, fullBlockers: true, verificationBlockers: true, wantCalls: 4},
		{name: "missing blocker field fails closed", threshold: 1, rounds: 5, missingBlockers: true, wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := testJob(command.Comment)
			job.Critic, job.CriticMaxRounds, job.CriticNonblockingRounds = &RoleSettings{Provider: "claude"}, tt.rounds, tt.threshold
			var phases []string
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				phases = append(phases, fmt.Sprintf("%s/%d/%s", request.role, request.round, request.reviewPhase))
				if request.role == roleMain {
					if request.round > 1 && (!strings.Contains(request.prompt, "Implement every valid finding at any severity") || !strings.Contains(request.prompt, "Remaining finding")) {
						t.Fatal("revision stopped requesting all findings before acceptance")
					}
					return aicli.Result{Answer: proposalJSON("Complete answer", "PRIVATE_THRESHOLD_NOTES")}, nil
				}
				if strings.Contains(request.prompt, "critic_nonblocking_rounds") || !strings.Contains(request.prompt, "Report every actionable finding at every severity even when none is blocking") {
					t.Fatal("critic was led toward threshold acceptance or omitted lower severities")
				}
				if tt.missingBlockers {
					return aicli.Result{Answer: `{"approved":false,"feedback":"Remaining finding"}`}, nil
				}
				blocking := (tt.fullBlockers && request.reviewPhase == reviewFull) || (tt.verificationBlockers && request.reviewPhase == reviewVerification)
				return aicli.Result{Answer: fmt.Sprintf(`{"approved":false,"has_blockers":%t,"feedback":"Remaining finding"}`, blocking)}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if (err == nil) != tt.wantSuccess || len(phases) != tt.wantCalls {
				t.Fatalf("policy result=%q error=%v phases=%v", result.Answer, err, phases)
			}
			if !tt.wantSuccess {
				if result.CriticStopReason != "" || result.Answer != "" || !errors.Is(err, monitor.ErrTerminalJob) {
					t.Fatalf("failed review leaked an answer or became retryable: %+v, %v", result, err)
				}
				return
			}
			last := result.Calls[len(result.Calls)-1]
			if result.CriticStopReason != "nonblocking" || last.ReviewPhase != reviewFull || last.Approved || !last.NoBlockers || result.Answer != "Complete answer" {
				t.Fatalf("policy acceptance falsified the actual critic verdict: %+v", result)
			}
			if err := result.Validate(job); err != nil {
				t.Fatalf("accepted policy result invalid: %v", err)
			}
		})
	}
}

func TestPortableJobResolvesCriticStopping(t *testing.T) {
	gitFixtureRemote(t)
	checkout := cloneFixture(t, t.TempDir())
	zero, one, three := 0, 1, 3
	for _, tt := range []struct {
		name                                string
		stopping                            config.CriticStopping
		critic                              bool
		wantMax, wantAfter, wantConsecutive int
	}{
		{name: "default", critic: true, wantMax: 5, wantAfter: 4, wantConsecutive: 2},
		{name: "custom maximum", stopping: config.CriticStopping{MaxRounds: 3}, critic: true, wantMax: 3, wantAfter: 2, wantConsecutive: 2},
		{name: "single round", stopping: config.CriticStopping{MaxRounds: 1}, critic: true, wantMax: 1, wantConsecutive: 2},
		{name: "two round default", stopping: config.CriticStopping{MaxRounds: 2}, critic: true, wantMax: 2, wantAfter: 1, wantConsecutive: 2},
		{name: "strict zero", stopping: config.CriticStopping{NonblockingAfterRound: &zero}, critic: true, wantMax: 5, wantConsecutive: 2},
		{name: "explicit first round", stopping: config.CriticStopping{NonblockingAfterRound: &one}, critic: true, wantMax: 5, wantAfter: 1, wantConsecutive: 2},
		{name: "explicit zero streak", stopping: config.CriticStopping{ConsecutiveNoBlockerRounds: &zero}, critic: true, wantMax: 5, wantAfter: 4},
		{name: "streak above budget", stopping: config.CriticStopping{MaxRounds: 2, ConsecutiveNoBlockerRounds: &three}, critic: true, wantMax: 2, wantAfter: 1, wantConsecutive: 3},
		{name: "disabled critic", stopping: config.CriticStopping{MaxRounds: 3, NonblockingAfterRound: &one, ConsecutiveNoBlockerRounds: &three}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			orchestrator := Orchestrator{CriticStopping: tt.stopping, Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}}
			if tt.critic {
				orchestrator.Adversarial = "claude"
			}
			request := monitor.Job{ID: "job", SubjectType: "Issue", Number: 7, Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, Source: github.Item{Author: github.User{ID: 3, Login: "writer"}}, Command: command.Command{Name: "comment", Action: command.Comment}}
			job, err := attemptFixture(&orchestrator).portableJob(testActionContext(t), &preparedAction{checkout: checkout, request: request}, "title", "body")
			if err != nil || job.CriticMaxRounds != tt.wantMax || job.CriticNonblockingRounds != tt.wantAfter || job.CriticConsecutiveNoBlockerRounds != tt.wantConsecutive {
				t.Fatalf("portable max=%d threshold=%d consecutive=%d err=%v; want %d/%d/%d", job.CriticMaxRounds, job.CriticNonblockingRounds, job.CriticConsecutiveNoBlockerRounds, err, tt.wantMax, tt.wantAfter, tt.wantConsecutive)
			}
		})
	}
}
