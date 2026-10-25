package action

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
)

func TestRunnerCriticStoppingReplaysFullReviewStreak(t *testing.T) {
	verdicts := map[string]string{
		"approved": approvedCritiqueJSON,
		"nits":     `{"review_complete":true,"approved":false,"has_blockers":false,"feedback":"Nonblocking nit."}`,
		"blockers": `{"review_complete":true,"approved":false,"has_blockers":true,"feedback":"Blocking defect."}`,
	}
	for _, test := range []struct {
		name                               string
		maximum, after, consecutive        int
		full                               map[int]string
		verification                       map[int]string
		lastRound, skippedFull, wantStreak int
		stopReason                         string
	}{
		{name: "streak starts before threshold", maximum: 5, after: 4, consecutive: 2, lastRound: 4, wantStreak: 4, stopReason: "nonblocking"},
		{name: "threshold waits for second full", maximum: 3, after: 1, consecutive: 2, lastRound: 2, wantStreak: 2, stopReason: "nonblocking"},
		{name: "verification does not count", maximum: 2, after: 1, consecutive: 2, full: map[int]string{1: "blockers"}, lastRound: 2, wantStreak: 1, stopReason: "round_cap"},
		{name: "full blockers reset", maximum: 5, after: 2, consecutive: 2, full: map[int]string{2: "blockers"}, lastRound: 4, wantStreak: 2, stopReason: "nonblocking"},
		{name: "blocking verification skips full and resets", maximum: 5, after: 2, consecutive: 2, verification: map[int]string{2: "blockers"}, lastRound: 4, skippedFull: 1, wantStreak: 2, stopReason: "nonblocking"},
		{name: "zero bypasses streak", maximum: 1, after: 1, lastRound: 1, wantStreak: 1, stopReason: "nonblocking"},
		{name: "streak above budget publishes at cap", maximum: 2, after: 1, consecutive: 3, lastRound: 2, wantStreak: 2, stopReason: "round_cap"},
		{name: "single blocked full publishes at cap", maximum: 1, full: map[int]string{1: "blockers"}, lastRound: 1, stopReason: "round_cap"},
		{name: "single strict nit publishes at cap", maximum: 1, lastRound: 1, wantStreak: 1, stopReason: "round_cap"},
		{name: "approval bypasses threshold and streak", maximum: 2, after: 2, consecutive: 20, full: map[int]string{1: "approved"}, lastRound: 1, wantStreak: 1, stopReason: "approved"},
		{name: "strict verification nits survive full approval", maximum: 2, full: map[int]string{1: "blockers", 2: "approved"}, lastRound: 2, wantStreak: 1, stopReason: "round_cap"},
		{name: "final verification blockers force blocked full", maximum: 2, after: 1, verification: map[int]string{2: "blockers"}, full: map[int]string{1: "blockers", 2: "blockers"}, lastRound: 2, stopReason: "round_cap"},
		{name: "final verification blockers prevent full approval acceptance", maximum: 2, after: 1, verification: map[int]string{2: "blockers"}, full: map[int]string{1: "blockers", 2: "approved"}, lastRound: 2, wantStreak: 1, stopReason: "round_cap"},
		{name: "final verification blockers prevent threshold acceptance", maximum: 2, after: 1, verification: map[int]string{2: "blockers"}, full: map[int]string{1: "blockers"}, lastRound: 2, wantStreak: 1, stopReason: "round_cap"},
		{name: "cleared verification permits approval", maximum: 3, consecutive: 20, full: map[int]string{1: "blockers", 2: "approved", 3: "approved"}, verification: map[int]string{3: "approved"}, lastRound: 3, wantStreak: 2, stopReason: "approved"},
		{name: "streak permits verification nits", maximum: 3, after: 2, consecutive: 2, full: map[int]string{1: "blockers", 2: "approved", 3: "approved"}, lastRound: 3, wantStreak: 2, stopReason: "nonblocking"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			job.CriticMaxRounds, job.CriticNonblockingRounds, job.CriticConsecutiveNoBlockerRounds = test.maximum, test.after, test.consecutive
			calls := 0
			root := t.TempDir()
			runner := Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				if request.role == roleMain {
					return aicli.Result{Answer: proposalJSON(fmt.Sprintf("Candidate %d", request.round), "")}, nil
				}
				verdict := test.verification[request.round]
				if request.reviewPhase == reviewFull {
					if request.round > 1 && request.round < test.maximum && verdict == "blockers" {
						t.Fatal("blocking verification advanced to full review")
					}
					verdict = test.full[request.round]
				}
				if verdict == "" {
					verdict = "nits"
				}
				return aicli.Result{Answer: verdicts[verdict]}, nil
			}}
			result, err := runner.Run(t.Context(), job)
			wantCalls := 3*test.lastRound - 1 - test.skippedFull
			if err != nil || result.CriticStopReason != test.stopReason || calls != wantCalls || len(result.Calls) != wantCalls {
				t.Fatalf("stopping result: calls=%d result=%+v error=%v; want calls=%d", calls, result, err, wantCalls)
			}
			if result.Answer != fmt.Sprintf("Candidate %d", test.lastRound) {
				t.Fatalf("accepted wrong proposal: %+v", result)
			}
			if err := result.validate(job, false); err != nil {
				t.Fatalf("runtime produced invalid replay evidence: %v", err)
			}
			replay := result
			replay.Answer = "Complete answer"
			if err := replay.Validate(job); err != nil {
				t.Fatalf("replayed stopping result invalid: %v", err)
			}
			if test.stopReason == "nonblocking" {
				strict := job
				strict.CriticNonblockingRounds = 0
				if err := replay.Validate(strict); err == nil {
					t.Fatal("replay accepted nits after changing retained policy to strict")
				}
			}
			var journal bytes.Buffer
			orchestrator := Orchestrator{WorkRoot: root, Logger: log.New(&journal, "", 0)}
			orchestrator.logResult(job, result)
			lines := strings.Split(strings.TrimSpace(journal.String()), "\n")
			last := lines[len(lines)-1]
			policyMet := test.stopReason == "approved" || test.stopReason == "nonblocking"
			wantFinal := fmt.Sprintf("no_blocker_verdict_streak=%d policy_met=%t", test.wantStreak, policyMet)
			wantStops := 0
			if policyMet {
				wantStops = 1
			}
			if len(lines) != wantCalls || !strings.Contains(last, wantFinal) || !strings.Contains(last, fmt.Sprintf("stop_reason=%q", test.stopReason)) || strings.Count(journal.String(), "policy_met=true") != wantStops {
				t.Fatalf("journal did not replay stopping evidence: %s; want final %s", journal.String(), wantFinal)
			}
			if test.stopReason == "round_cap" && !strings.Contains(last, "capped=true") {
				t.Fatalf("journal hid capped publication eligibility: %s", last)
			}
			if err := orchestrator.retainReviewEvidence(job, result); err != nil {
				t.Fatal(err)
			}
			saved := readPrivateReview(t, root)
			if saved.CriticStopReason != test.stopReason || saved.CriticMaxRounds != test.maximum || saved.CriticNonblockingRounds != test.after || saved.CriticConsecutiveNoBlockerRounds != test.consecutive || !reflect.DeepEqual(saved.Calls, result.Calls) {
				t.Fatalf("retained evidence lost replay policy or verdicts: %+v", saved)
			}
		})
	}
}

func TestRunAIPreservesRejectedVerificationAndFullFeedback(t *testing.T) {
	const verificationFinding = "Earlier nonblocking nit still applies."
	const fullFinding = "Fresh full review found another defect."
	const combined = "verification review:\n" + verificationFinding + "\n\nfull review:\n" + fullFinding
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticMaxRounds = 3
	calls := 0
	runner := Runner{runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		calls++
		if request.role == roleMain {
			if request.round == 3 && request.prompt != revisionPrompt("Complete the task", "Candidate 2", combined) {
				t.Fatalf("revision lost phase-labelled feedback: %q", request.prompt)
			}
			return aicli.Result{Answer: proposalJSON(fmt.Sprintf("Candidate %d", request.round), "")}, nil
		}
		if request.round == 1 {
			return aicli.Result{Answer: `{"review_complete":true,"approved":false,"has_blockers":true,"feedback":"Initial defect."}`}, nil
		}
		if request.round == 2 {
			feedback, blockers := verificationFinding, false
			if request.reviewPhase == reviewFull {
				feedback, blockers = fullFinding, true
			}
			return aicli.Result{Answer: fmt.Sprintf(`{"review_complete":true,"approved":false,"has_blockers":%t,"feedback":%q}`, blockers, feedback)}, nil
		}
		if request.reviewPhase == reviewVerification && request.criticPrompt("", "") != criticPrompt(reviewVerification, "Complete the task", "Candidate 3", "Candidate 2", combined, "") {
			t.Fatal("verification lost either previous phase's rejection")
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}}
	answer, reason, err := runner.runAI(t.Context(), job, t.TempDir(), "Complete the task", "", nil, &modelSessions{})
	if err != nil || reason != "approved" || answer != "Candidate 3" || calls != 8 {
		t.Fatalf("dual feedback revision: answer=%q calls=%d error=%v", answer, calls, err)
	}
}

func TestResultCriticStopReasonRequiresMatchingDecisionEvidence(t *testing.T) {
	for _, test := range []struct {
		name, reason      string
		nits, noCritic    bool
		verificationOnly  bool
		complete, partial bool
	}{
		{name: "approved decision", reason: "approved", complete: true, partial: true},
		{name: "approval flags without decision", partial: true},
		{name: "wrong approved reason", reason: "nonblocking"},
		{name: "unknown reason", reason: "approved\nforged journal"},
		{name: "nonblocking decision", reason: "nonblocking", nits: true, complete: true, partial: true},
		{name: "nonblocking flags without decision", nits: true, partial: true},
		{name: "wrong nonblocking reason", reason: "approved", nits: true},
		{name: "verification is not a decision", reason: "approved", verificationOnly: true},
		{name: "no critic cannot decide", reason: "approved", noCritic: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			result := privateReviewResult(job)
			result.CriticStopReason = test.reason
			if test.nits {
				job.CriticNonblockingRounds = 1
				result.Calls[len(result.Calls)-1].Approved = false
			}
			if test.verificationOnly {
				result.Calls = result.Calls[:4]
			}
			if test.noCritic {
				job.Critic, job.CriticMaxRounds = nil, 0
				result.Calls, result.Refutations = result.Calls[:1], nil
			}
			if err := result.Validate(job); (err == nil) != test.complete {
				t.Fatalf("complete decision validation=%v; want valid=%t", err, test.complete)
			}
			if err := result.validate(job, false); (err == nil) != test.partial {
				t.Fatalf("partial decision validation=%v; want valid=%t", err, test.partial)
			}
			var journal bytes.Buffer
			(&Orchestrator{Logger: log.New(&journal, "", 0)}).logResult(job, result)
			if !test.partial && journal.Len() != 0 {
				t.Fatalf("unvalidated decision reached journal: %s", journal.String())
			}
			if test.partial && test.reason == "" && (strings.Contains(journal.String(), "policy_met=true") || strings.Contains(journal.String(), `stop_reason="approved"`) || strings.Contains(journal.String(), `stop_reason="nonblocking"`)) {
				t.Fatalf("journal inferred a decision from verdict flags: %s", journal.String())
			}
		})
	}
}

func TestCriticPolicyRequiresReviewCoverage(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticNonblockingRounds = 1
	for _, phase := range []string{reviewFull, reviewVerification} {
		for _, verdict := range []CallUsage{{HasBlockers: true}, {NoBlockers: true}, {Approved: true, NoBlockers: true}} {
			t.Run(fmt.Sprintf("%s/approved=%t/no-blockers=%t", phase, verdict.Approved, verdict.NoBlockers), func(t *testing.T) {
				// Coverage gates the policy independently of the parser's tuple validation.
				policy := criticPolicy{job: job, verificationApproved: true, noBlockerRounds: 2}
				verdict.Role, verdict.Round, verdict.ReviewPhase, verdict.Complete = roleCritic, 2, phase, true
				advance, reason := policy.observe(verdict)
				if advance || reason != "" || policy.noBlockerRounds != 0 {
					t.Fatalf("incomplete coverage advanced or stopped policy: advance=%t reason=%q streak=%d", advance, reason, policy.noBlockerRounds)
				}
			})
		}
	}
}
