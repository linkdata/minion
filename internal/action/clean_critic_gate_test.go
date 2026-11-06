package action

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
)

func TestCleanCriticGateReviewsSameCandidate(t *testing.T) {
	job := withClaudeCritic(testJob(command.Fix))
	job.CriticMaxRounds = 1
	job.RequiredCleanCriticRounds = 3
	root := t.TempDir()
	mainCalls, fullCalls := 0, 0
	firstPrompt := ""
	runner := Runner{WorkRoot: root, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == roleMain {
			mainCalls++
			return aicli.Result{Answer: proposalJSON("Candidate", "")}, nil
		}
		fullCalls++
		if request.round != 1 || request.reviewPhase != reviewFull || !request.freshConversation || request.proposedAnswer != "Candidate" {
			t.Fatalf("clean pass changed review scope or candidate: %+v", request)
		}
		prompt := request.criticPrompt("", "")
		if firstPrompt != "" && prompt != firstPrompt {
			t.Fatal("clean pass prompt changed for unchanged candidate")
		}
		firstPrompt = prompt
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}}
	result, err := runner.Run(t.Context(), job)
	if err != nil || mainCalls != 1 || fullCalls != 3 || result.CriticStopReason != "approved" || result.Answer != "Candidate" {
		t.Fatalf("clean review gate: main=%d full=%d stop=%q answer=%q err=%v", mainCalls, fullCalls, result.CriticStopReason, result.Answer, err)
	}
	if err := result.Validate(job); err != nil {
		t.Fatalf("clean review evidence is invalid: %v", err)
	}
	if err := (&Orchestrator{WorkRoot: root}).retainReviewEvidence(job, result); err != nil {
		t.Fatal(err)
	}
	if saved := readPrivateReview(t, root); saved.RequiredCleanCriticRounds != 3 || len(saved.Calls) != 4 {
		t.Fatalf("private review lost clean pass policy: %+v", saved)
	}
	missing := result
	missing.Calls = missing.Calls[:len(missing.Calls)-1]
	if err := missing.Validate(job); err == nil {
		t.Fatal("accepted a result that omitted the required third clean pass")
	}
}

func TestCleanCriticGateRejectionRevises(t *testing.T) {
	job := withClaudeCritic(testJob(command.Fix))
	job.CriticMaxRounds = 2
	job.RequiredCleanCriticRounds = 2
	mainCalls, fullCalls, verificationCalls := 0, 0, 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == roleMain {
			mainCalls++
			if request.round == 2 && !strings.Contains(request.prompt, "Second full review found a defect") {
				t.Fatalf("revision omitted the rejected clean pass: %q", request.prompt)
			}
			return aicli.Result{Answer: proposalJSON(fmt.Sprintf("Candidate %d", request.round), "")}, nil
		}
		if request.reviewPhase == reviewVerification {
			verificationCalls++
			return aicli.Result{Answer: approvedCritiqueJSON}, nil
		}
		fullCalls++
		if fullCalls == 2 {
			return aicli.Result{Answer: `{"review_complete":true,"approved":false,"has_blockers":true,"feedback":"Second full review found a defect"}`}, nil
		}
		if request.round == 2 && (!request.freshConversation || request.proposedAnswer != "Candidate 2") {
			t.Fatalf("revised candidate did not receive a fresh full review: %+v", request)
		}
		return aicli.Result{Answer: approvedCritiqueJSON}, nil
	}}
	result, err := runner.Run(t.Context(), job)
	if err != nil || mainCalls != 2 || verificationCalls != 1 || fullCalls != 4 || result.Answer != "Candidate 2" || result.CriticStopReason != "approved" {
		t.Fatalf("revised clean gate: main=%d verification=%d full=%d result=%+v err=%v", mainCalls, verificationCalls, fullCalls, result, err)
	}
	if err := result.Validate(job); err != nil {
		t.Fatalf("revised clean review evidence is invalid: %v", err)
	}
}

func TestCleanCriticGateCapDoesNotPublish(t *testing.T) {
	job := withClaudeCritic(testJob(command.Fix))
	job.CriticMaxRounds = 1
	job.RequiredCleanCriticRounds = 2
	fullCalls := 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		if request.role == roleMain {
			return aicli.Result{Answer: proposalJSON("Candidate", "")}, nil
		}
		fullCalls++
		if fullCalls == 1 {
			return aicli.Result{Answer: approvedCritiqueJSON}, nil
		}
		return aicli.Result{Answer: `{"review_complete":true,"approved":false,"has_blockers":true,"feedback":"A defect"}`}, nil
	}}
	result, err := runner.Run(t.Context(), job)
	if !errors.Is(err, monitor.ErrTerminalJob) || result.CriticStopReason != "" || fullCalls != 2 {
		t.Fatalf("unmet clean gate did not fail terminally: full=%d result=%+v err=%v", fullCalls, result, err)
	}
	if err := result.Validate(job); err == nil {
		t.Fatal("accepted a capped result without the required clean passes")
	}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("failed attempt lost valid review evidence: %v", err)
	}
}

func TestCleanCriticGateRejectsSkippedPass(t *testing.T) {
	job := withClaudeCritic(testJob(command.Fix))
	job.CriticMaxRounds = 2
	job.RequiredCleanCriticRounds = 2
	result := privateReviewResult(job)
	// The first approved full review cannot be followed by a new proposal without
	// the second independent full review of that same candidate.
	approved := result.Calls[4]
	approved.Round = 1
	result.Calls = []CallUsage{result.Calls[0], approved, result.Calls[2]}
	result.CriticStopReason = ""
	if err := result.validateCalls(job, false); err == nil {
		t.Fatal("accepted a new proposal before the second clean full review")
	}
}

func TestJobCleanCriticGateAndInstructionsValidation(t *testing.T) {
	base := withClaudeCritic(testJob(command.Fix))
	for _, test := range []struct {
		name   string
		change func(*Job)
		valid  bool
	}{
		{"default", func(*Job) {}, true},
		{"one clean pass", func(j *Job) { j.RequiredCleanCriticRounds = 1 }, true},
		{"twenty clean passes", func(j *Job) { j.RequiredCleanCriticRounds = 20 }, true},
		{"negative clean passes", func(j *Job) { j.RequiredCleanCriticRounds = -1 }, false},
		{"too many clean passes", func(j *Job) { j.RequiredCleanCriticRounds = 21 }, false},
		{"without critic", func(j *Job) { j.Critic = nil; j.CriticMaxRounds = 0; j.RequiredCleanCriticRounds = 1 }, false},
		{"non-fix action", func(j *Job) { j.Action = command.Comment; j.RequiredCleanCriticRounds = 1 }, false},
		{"instructions at limit", func(j *Job) { j.AIInstructions = strings.Repeat("x", config.MaxAIInstructionsBytes) }, true},
		{"instructions over limit", func(j *Job) { j.AIInstructions = strings.Repeat("x", config.MaxAIInstructionsBytes+1) }, false},
		{"invalid instruction UTF-8", func(j *Job) { j.AIInstructions = "\xff" }, false},
		{"instruction NUL", func(j *Job) { j.AIInstructions = "a\x00b" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := base
			test.change(&job)
			if err := job.Validate(); (err == nil) != test.valid {
				t.Fatalf("valid=%t err=%v", test.valid, err)
			}
		})
	}
}
