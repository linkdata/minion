package action

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
)

func TestRunnerCriticStoppingReplaysFullReviewStreak(t *testing.T) {
	verdicts := map[string]string{
		"approved": approvedCritiqueJSON,
		"nits":     `{"approved":false,"has_blockers":false,"feedback":"Nonblocking nit."}`,
		"blockers": `{"approved":false,"has_blockers":true,"feedback":"Blocking defect."}`,
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
		{name: "verification does not count", maximum: 2, after: 1, consecutive: 2, full: map[int]string{1: "blockers"}, lastRound: 2, wantStreak: 1},
		{name: "full blockers reset", maximum: 5, after: 2, consecutive: 2, full: map[int]string{2: "blockers"}, lastRound: 4, wantStreak: 2, stopReason: "nonblocking"},
		{name: "blocking verification skips full and resets", maximum: 5, after: 2, consecutive: 2, verification: map[int]string{2: "blockers"}, lastRound: 4, skippedFull: 1, wantStreak: 2, stopReason: "nonblocking"},
		{name: "zero bypasses streak", maximum: 1, after: 1, lastRound: 1, wantStreak: 1, stopReason: "nonblocking"},
		{name: "streak above budget fails at cap", maximum: 2, after: 1, consecutive: 3, lastRound: 2, wantStreak: 2},
		{name: "approval bypasses threshold and streak", maximum: 2, after: 2, consecutive: 20, full: map[int]string{1: "approved"}, lastRound: 1, wantStreak: 1, stopReason: "approved"},
		{name: "strict verification nits survive full approval", maximum: 2, full: map[int]string{1: "blockers", 2: "approved"}, lastRound: 2, wantStreak: 1},
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
					if request.round > 1 && verdict == "blockers" {
						t.Fatal("blocking verification advanced to full review")
					}
					verdict = test.full[request.round]
				}
				if verdict == "" {
					verdict = "nits"
				}
				return aicli.Result{Answer: verdicts[verdict]}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			wantSuccess := test.stopReason != ""
			wantCalls := 3*test.lastRound - 1 - test.skippedFull
			if result.CriticStopReason != test.stopReason || (err == nil) != wantSuccess || calls != wantCalls || len(result.Calls) != wantCalls {
				t.Fatalf("stopping result: calls=%d result=%+v error=%v; want calls=%d success=%t", calls, result, err, wantCalls, wantSuccess)
			}
			if wantSuccess {
				if result.Answer != fmt.Sprintf("Candidate %d", test.lastRound) {
					t.Fatalf("accepted wrong proposal: %+v", result)
				}
			} else if !errors.Is(err, monitor.ErrTerminalJob) || result.Answer != "" {
				t.Fatalf("exhausted policy was retryable or returned a proposal: %+v, %v", result, err)
			}
			if err := result.validate(job, false); err != nil {
				t.Fatalf("runtime produced invalid replay evidence: %v", err)
			}
			replay := result
			replay.Answer = "Complete answer"
			if err := replay.Validate(job); (err == nil) != wantSuccess {
				t.Fatalf("replayed policy acceptance=%v; want success=%t", err, wantSuccess)
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
			wantFinal := fmt.Sprintf("no_blocker_verdict_streak=%d policy_met=%t stop_reason=%q", test.wantStreak, wantSuccess, test.stopReason)
			wantStops := 0
			if wantSuccess {
				wantStops = 1
			}
			if len(lines) != wantCalls || !strings.Contains(last, wantFinal) || strings.Count(journal.String(), "policy_met=true") != wantStops {
				t.Fatalf("journal did not replay stopping evidence: %s; want final %s", journal.String(), wantFinal)
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
			return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"feedback":"Initial defect."}`}, nil
		}
		if request.round == 2 {
			feedback, blockers := verificationFinding, false
			if request.reviewPhase == reviewFull {
				feedback, blockers = fullFinding, true
			}
			return aicli.Result{Answer: fmt.Sprintf(`{"approved":false,"has_blockers":%t,"feedback":%q}`, blockers, feedback)}, nil
		}
		if request.reviewPhase == reviewVerification && request.criticPrompt("", "") != criticPrompt(reviewVerification, "Complete the task", "Candidate 3", "Candidate 2", combined, "") {
			t.Fatal("verification lost either previous phase's rejection")
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}}
	answer, reason, err := runner.runAI(context.Background(), job, t.TempDir(), "Complete the task", "", nil, &modelSessions{})
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
