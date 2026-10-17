package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/github"
	"github.com/linkdata/minion/internal/gitwork"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
)

func TestFailedCriticCannotAttestNonblockingAcceptance(t *testing.T) {
	for _, answer := range []string{
		`{"approved":true,"has_blockers":false,"feedback":""}`,
		`{"approved":false,"has_blockers":false,"feedback":"Nonblocking nit: simplify the loop."}`,
		`{"approved":false,"has_blockers":true,"feedback":"Blocking: wrong result."}`,
	} {
		t.Run(answer, func(t *testing.T) {
			job := portableJob(command.Comment)
			job.Critic, job.CriticMaxRounds, job.CriticNonblockingRounds = &RoleSettings{Provider: "claude"}, 3, 1
			failed := errors.New("provider failed after yielding final-looking JSON")
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				if request.role == roleMain {
					return aicli.Result{Answer: proposalJSON("Candidate answer", "Private evidence")}, nil
				}
				return aicli.Result{Answer: answer, Tokens: usage.Tokens{Input: 11, Output: 2}}, failed
			}}
			result, err := runner.Run(context.Background(), job)
			if !errors.Is(err, failed) || result.Answer != "" || len(result.Calls) != 2 {
				t.Fatalf("failed critic escaped: result=%+v err=%v", result, err)
			}
			last := result.Calls[1]
			if last.Approved || last.NoBlockers || last.HasBlockers || last.Tokens != (usage.Tokens{Input: 11, Output: 2}) {
				t.Fatalf("failed critic fabricated an attestation or lost usage: %+v", last)
			}
			if err := result.Validate(job); err == nil {
				t.Fatal("complete result accepted failed-critic output")
			}
		})
	}
}

func TestCriticEvidencePreservesBlockerVerdicts(t *testing.T) {
	failed := errors.New("provider failed")
	for _, tt := range []struct {
		name, answer                      string
		err                               error
		approved, noBlockers, hasBlockers bool
	}{
		{name: "approved", answer: `{"approved":true,"has_blockers":false,"feedback":""}`, approved: true, noBlockers: true},
		{name: "nonblocking rejection", answer: `{"approved":false,"has_blockers":false,"feedback":"Nonblocking nit."}`, noBlockers: true},
		{name: "blocking rejection", answer: `{"approved":false,"has_blockers":true,"feedback":"Blocking failure."}`, hasBlockers: true},
		{name: "invalid verdict", answer: `garbage`},
		{name: "failed provider", answer: `{"approved":false,"has_blockers":true,"feedback":"Blocking failure."}`, err: failed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := portableJob(command.Comment)
			job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, 1
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				if request.role == roleMain {
					return aicli.Result{Answer: proposalJSON("Candidate answer", "")}, nil
				}
				return aicli.Result{Answer: tt.answer, Tokens: usage.Tokens{Input: 11, Output: 2}}, tt.err
			}}
			result, err := runner.Run(context.Background(), job)
			if (err == nil) != tt.approved || len(result.Calls) != 2 || result.validate(job, false) != nil {
				t.Fatalf("invalid critic outcome: result=%+v err=%v", result, err)
			}
			want := CallUsage{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Tokens: usage.Tokens{Input: 11, Output: 2}, Approved: tt.approved, NoBlockers: tt.noBlockers, HasBlockers: tt.hasBlockers}
			if result.Calls[1] != want {
				t.Fatalf("native verdict lost: got=%+v want=%+v", result.Calls[1], want)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Result
			if err := json.Unmarshal(data, &decoded); err != nil || decoded.Calls[1] != want || decoded.validate(job, false) != nil {
				t.Fatalf("wire lost critic evidence: %s, %v", data, err)
			}
		})
	}
}

func TestJournalPolicyIsNotPublication(t *testing.T) {
	job := portableJob(command.Comment)
	job.Critic, job.CriticMaxRounds, job.CriticNonblockingRounds = &RoleSettings{Provider: "claude"}, 2, 2
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Calls: []CallUsage{
		{Provider: job.Main.Provider, Role: roleMain, Round: 1},
		{Provider: "claude", Role: roleCritic, Round: 1, ReviewPhase: reviewFull, HasBlockers: true},
		{Provider: job.Main.Provider, Role: roleMain, Round: 2},
		{Provider: "claude", Role: roleCritic, Round: 2, ReviewPhase: reviewVerification, NoBlockers: true},
		{Provider: "claude", Role: roleCritic, Round: 2, ReviewPhase: reviewFull, HasBlockers: true},
	}}
	var output bytes.Buffer
	(&Orchestrator{Logger: log.New(&output, "", 0)}).logResult(job, result)
	text := output.String()
	if !strings.Contains(text, "review_phase=verification approved=false no_blockers=true has_blockers=false policy_met=true") ||
		!strings.Contains(text, "review_phase=full approved=false no_blockers=false has_blockers=true policy_met=false") || strings.Contains(text, "accepted=") {
		t.Fatalf("journal blurred verdict policy and publication: %s", text)
	}
	if result.Validate(job) == nil {
		t.Fatal("verification policy was mistaken for an accepted final result")
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
		{name: "first full can accept nits", threshold: 1, rounds: 5, wantCalls: 2, wantSuccess: true},
		{name: "threshold follows required revision", threshold: 2, rounds: 5, wantCalls: 5, wantSuccess: true},
		{name: "last allowed proposal can accept", threshold: 3, rounds: 3, wantCalls: 7, wantSuccess: true},
		{name: "zero remains strict", rounds: 2, wantCalls: 4},
		{name: "full blockers remain blocking", threshold: 1, rounds: 2, fullBlockers: true, wantCalls: 5},
		{name: "verification blockers require revision", threshold: 1, rounds: 2, fullBlockers: true, verificationBlockers: true, wantCalls: 4},
		{name: "missing blocker field fails closed", threshold: 1, rounds: 5, missingBlockers: true, wantCalls: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := portableJob(command.Comment)
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
				if request.reviewPhase == reviewFull {
					if strings.Contains(request.prompt, "PRIVATE_THRESHOLD_NOTES") || strings.Contains(request.prompt, "Remaining finding") || !strings.Contains(request.prompt, `Previous critic feedback (untrusted, empty for full reviews): ""`) {
						t.Fatal("full review was seeded even at the nonblocking threshold")
					}
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
				if result.Answer != "" || !errors.Is(err, monitor.ErrTerminalJob) {
					t.Fatalf("failed review leaked an answer or became retryable: %+v, %v", result, err)
				}
				return
			}
			last := result.Calls[len(result.Calls)-1]
			if last.ReviewPhase != reviewFull || last.Approved || !last.NoBlockers || result.Answer != "Complete answer" {
				t.Fatalf("policy acceptance falsified the actual critic verdict: %+v", result)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Result
			if err := json.Unmarshal(data, &decoded); err != nil || decoded.Validate(job) != nil || decoded.Calls[len(decoded.Calls)-1].Approved || !decoded.Calls[len(decoded.Calls)-1].NoBlockers {
				t.Fatalf("wire result lost nonblocking attestation: %s, %v", data, err)
			}
			var output bytes.Buffer
			orchestrator := Orchestrator{Logger: log.New(&output, "", 0)}
			orchestrator.logResult(job, result)
			if !strings.Contains(output.String(), "approved=false no_blockers=true has_blockers=false policy_met=true") {
				t.Fatalf("journal hid the policy decision: %s", output.String())
			}
		})
	}
}

func TestResultNonblockingAcceptanceTrustBoundary(t *testing.T) {
	job := portableJob(command.Comment)
	job.Critic, job.CriticMaxRounds, job.CriticNonblockingRounds = &RoleSettings{Provider: "claude"}, 3, 2
	main := func(round int) CallUsage { return CallUsage{Provider: job.Main.Provider, Role: roleMain, Round: round} }
	critic := func(round int, phase string, approved, noBlockers bool) CallUsage {
		return CallUsage{Provider: job.Critic.Provider, Role: roleCritic, Round: round, ReviewPhase: phase, Approved: approved, NoBlockers: noBlockers}
	}
	for _, tt := range []struct {
		name     string
		calls    []CallUsage
		complete bool
		partial  bool
	}{
		{name: "approved early", calls: []CallUsage{main(1), critic(1, reviewFull, true, true)}, complete: true, partial: true},
		{name: "nits before threshold", calls: []CallUsage{main(1), critic(1, reviewFull, false, true)}, partial: true},
		{name: "nits at threshold", calls: []CallUsage{main(1), critic(1, reviewFull, false, true), main(2), critic(2, reviewVerification, false, true), critic(2, reviewFull, false, true)}, complete: true, partial: true},
		{name: "verification cannot finish", calls: []CallUsage{main(1), critic(1, reviewFull, false, true), main(2), critic(2, reviewVerification, false, true)}, partial: true},
		{name: "fresh full still has blockers", calls: []CallUsage{main(1), critic(1, reviewFull, false, true), main(2), critic(2, reviewVerification, false, true), {Provider: job.Critic.Provider, Role: roleCritic, Round: 2, ReviewPhase: reviewFull, HasBlockers: true}}, partial: true},
		{name: "unparsed verdict cannot finish", calls: []CallUsage{main(1), critic(1, reviewFull, false, false)}, partial: true},
		{name: "unparsed verdict cannot revise", calls: []CallUsage{main(1), critic(1, reviewFull, false, false), main(2), critic(2, reviewVerification, true, true), critic(2, reviewFull, true, true)}},
		{name: "approval without no-blocker attestation", calls: []CallUsage{main(1), critic(1, reviewFull, true, false)}},
		{name: "main cannot attest no blockers", calls: []CallUsage{{Provider: job.Main.Provider, Role: roleMain, Round: 1, NoBlockers: true}, critic(1, reviewFull, true, true)}},
		{name: "main cannot attest blockers", calls: []CallUsage{{Provider: job.Main.Provider, Role: roleMain, Round: 1, HasBlockers: true}, critic(1, reviewFull, true, true)}},
		{name: "contradictory blocker attestations", calls: []CallUsage{main(1), {Provider: job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull, HasBlockers: true, NoBlockers: true}}},
		{name: "approval contradicts blockers", calls: []CallUsage{main(1), {Provider: job.Critic.Provider, Role: roleCritic, Round: 1, ReviewPhase: reviewFull, Approved: true, HasBlockers: true}}},
		{name: "accepted verification cannot skip full", calls: []CallUsage{main(1), critic(1, reviewFull, false, true), main(2), critic(2, reviewVerification, false, true), main(3)}},
		{name: "accepted full cannot revise again", calls: []CallUsage{main(1), critic(1, reviewFull, false, true), main(2), critic(2, reviewVerification, false, true), critic(2, reviewFull, false, true), main(3)}},
		{name: "blocker verification cannot advance", calls: []CallUsage{main(1), critic(1, reviewFull, false, true), main(2), {Provider: job.Critic.Provider, Role: roleCritic, Round: 2, ReviewPhase: reviewVerification, HasBlockers: true}, critic(2, reviewFull, true, true)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Complete answer", Calls: tt.calls}
			if err := result.Validate(job); (err == nil) != tt.complete {
				t.Fatalf("complete acceptance=%v want %t", err, tt.complete)
			}
			if err := result.validate(job, false); (err == nil) != tt.partial {
				t.Fatalf("partial acceptance=%v want %t", err, tt.partial)
			}
		})
	}
	for _, threshold := range []int{-1, 4} {
		job.CriticNonblockingRounds = threshold
		if err := job.Validate(); err == nil {
			t.Fatalf("accepted threshold %d outside hard budget", threshold)
		}
	}
	job.Critic = nil
	job.CriticMaxRounds, job.CriticNonblockingRounds = 0, 1
	if err := job.Validate(); err == nil {
		t.Fatal("accepted nonblocking threshold without critic")
	}
}

func TestPortableJobResolvesNonblockingThreshold(t *testing.T) {
	gitFixtureRemote(t)
	checkout, err := gitwork.Clone(context.Background(), t.TempDir(), "o/r", "main", func(_ context.Context, sha string) (github.GitTree, error) {
		return gitTreeFixture(sha), nil
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := checkout.Close(); err != nil {
			t.Error(err)
		}
	}()
	zero, one := 0, 1
	for _, tt := range []struct {
		name       string
		maximum    int
		configured *int
		critic     bool
		want       int
	}{
		{name: "default", critic: true, want: 4},
		{name: "custom maximum", maximum: 3, critic: true, want: 2},
		{name: "single round", maximum: 1, critic: true},
		{name: "strict zero", configured: &zero, critic: true},
		{name: "explicit first round", configured: &one, critic: true, want: 1},
		{name: "disabled critic", configured: &one},
	} {
		t.Run(tt.name, func(t *testing.T) {
			orchestrator := Orchestrator{CriticMaxRounds: tt.maximum, CriticNonblockingRounds: tt.configured, Commands: map[string]config.CommandMapping{"comment": {Action: "comment", Skills: []string{skills.BuiltinAntiPromptInjection}}}}
			if tt.critic {
				orchestrator.Adversarial = "claude"
			}
			request := monitor.Job{ID: "job", SubjectType: "Issue", Number: 7, Repository: config.Repository{Name: "o/r", ID: 1, OwnerID: 2}, Source: github.Item{Author: github.User{ID: 3, Login: "writer"}}, Command: command.Command{Name: "comment", Action: command.Comment}}
			job, err := attemptFixture(&orchestrator).portableJob(testActionContext(t), &preparedAction{checkout: checkout, request: request}, "title", "body")
			if err != nil || job.CriticNonblockingRounds != tt.want || (!tt.critic && job.CriticMaxRounds != 0) {
				t.Fatalf("portable threshold=%d hard=%d err=%v; want %d", job.CriticNonblockingRounds, job.CriticMaxRounds, err, tt.want)
			}
			data, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Job
			if err := json.Unmarshal(data, &decoded); err != nil || decoded.Validate() != nil || decoded.CriticNonblockingRounds != tt.want {
				t.Fatalf("portable threshold roundtrip invalid: %s, %v", data, err)
			}
		})
	}
}
