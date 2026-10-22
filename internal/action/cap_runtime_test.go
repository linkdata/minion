package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

// Earlier blocking verifications skip full; the final candidate always receives it.
func cappedResultFixture(job Job) Result {
	result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head, Answer: "Reviewed candidate", CriticStopReason: "round_cap"}
	for round := 1; round <= job.CriticMaxRounds; round++ {
		result.Calls = append(result.Calls, CallUsage{Provider: job.Main.Provider, Model: job.Main.Model, Effort: job.Main.Effort, Role: roleMain, Round: round})
		for _, phase := range reviewPhases(round) {
			result.Calls = append(result.Calls, CallUsage{Provider: job.Critic.Provider, Model: job.Critic.Model, Effort: job.Critic.Effort, Role: roleCritic, Round: round, ReviewPhase: phase, ReviewComplete: true, HasBlockers: true})
			if phase == reviewVerification && round < job.CriticMaxRounds {
				break
			}
		}
	}
	return result
}

func TestCappedDecisionRequiresOriginalFinalCompleteEvidence(t *testing.T) {
	for _, test := range []struct {
		name              string
		mutate            func(*Job, *Result)
		complete, partial bool
	}{
		{name: "completed blocked full", complete: true, partial: true},
		{name: "blocking verification survives full approval", complete: true, partial: true, mutate: func(_ *Job, r *Result) {
			r.Calls[4].Approved, r.Calls[4].NoBlockers, r.Calls[4].HasBlockers = true, true, false
		}},
		{name: "verdicts without actual capped decision", partial: true, mutate: func(_ *Job, r *Result) { r.CriticStopReason = "" }},
		{name: "cap before original maximum", mutate: func(j *Job, _ *Result) { j.CriticMaxRounds = 3 }},
		{name: "calls beyond changed maximum", mutate: func(j *Job, _ *Result) { j.CriticMaxRounds = 1 }},
		{name: "cap cannot claim approval", mutate: func(_ *Job, r *Result) { r.CriticStopReason = "approved" }},
		{name: "cap cannot claim nonblocking", mutate: func(_ *Job, r *Result) { r.CriticStopReason = "nonblocking" }},
		{name: "normal approval cannot claim cap", mutate: func(_ *Job, r *Result) {
			for _, i := range []int{3, 4} {
				r.Calls[i].Approved, r.Calls[i].NoBlockers, r.Calls[i].HasBlockers = true, true, false
			}
		}},
		{name: "incomplete full cannot cap", mutate: func(_ *Job, r *Result) { r.Calls[4].ReviewComplete = false }},
		{name: "honest incomplete full prefix", partial: true, mutate: func(_ *Job, r *Result) {
			r.Calls[4].ReviewComplete, r.CriticStopReason = false, ""
		}},
		{name: "unparsed full cannot cap", mutate: func(_ *Job, r *Result) {
			r.Calls[4].ReviewComplete, r.Calls[4].HasBlockers = false, false
		}},
		{name: "missing final full cannot cap", mutate: func(_ *Job, r *Result) { r.Calls = r.Calls[:4] }},
		{name: "honest verification prefix", partial: true, mutate: func(_ *Job, r *Result) {
			r.Calls, r.CriticStopReason = r.Calls[:4], ""
		}},
		{name: "missing final verification", mutate: func(_ *Job, r *Result) { r.Calls = slices.Delete(r.Calls, 3, 4) }},
		{name: "calls after capped decision", mutate: func(j *Job, r *Result) {
			r.Calls = append(r.Calls, CallUsage{Provider: j.Main.Provider, Role: roleMain, Round: 3})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			job.CriticMaxRounds, job.CriticNonblockingRounds = 2, 1
			result := cappedResultFixture(job)
			if err := result.Validate(job); err != nil {
				t.Fatalf("invalid baseline: %v", err)
			}
			if test.mutate != nil {
				test.mutate(&job, &result)
			}
			if err := result.Validate(job); (err == nil) != test.complete {
				t.Fatalf("complete cap evidence valid=%t; error=%v", test.complete, err)
			}
			if err := result.validate(job, false); (err == nil) != test.partial {
				t.Fatalf("partial cap evidence valid=%t; error=%v", test.partial, err)
			}
			var journal bytes.Buffer
			(&Orchestrator{Logger: log.New(&journal, "", 0)}).logResult(job, result)
			if !test.partial && journal.Len() != 0 {
				t.Fatalf("invalid capped evidence reached journal: %s", journal.String())
			}
			if test.partial && result.CriticStopReason == "" && strings.Contains(journal.String(), "capped=true") {
				t.Fatalf("journal inferred actual cap decision from flags: %s", journal.String())
			}
			if test.complete {
				data, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				var replay Result
				if err := json.Unmarshal(data, &replay); err != nil || !reflect.DeepEqual(replay, result) || replay.Validate(job) != nil {
					t.Fatalf("cap replay lost original evidence: %+v, %v", replay, err)
				}
			}
		})
	}
}

func TestFinalCriticFailureCannotPublishAtCap(t *testing.T) {
	providerErr := errors.New("provider interrupted at cap")
	for _, position := range []struct {
		name, phase  string
		round, calls int
	}{
		{name: "first full", phase: reviewFull, round: 1, calls: 2},
		{name: "final verification", phase: reviewVerification, round: 2, calls: 4},
		{name: "forced final full", phase: reviewFull, round: 2, calls: 5},
	} {
		for _, failure := range []string{"malformed", "incomplete", "provider", "blank sanitized feedback", "empty redaction"} {
			t.Run(position.name+"/"+failure, func(t *testing.T) {
				job := withClaudeCritic(testJob(command.Comment))
				job.CriticMaxRounds, job.CriticNonblockingRounds = position.round, 1
				calls := 0
				tokens := usage.Tokens{Input: 17, CachedInput: 3, Output: 5}
				runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					calls++
					if calls > position.calls {
						t.Fatal("dispatched after final critic failure")
					}
					if request.role == roleMain {
						return aicli.Result{Answer: proposalJSON("Candidate", "")}, nil
					}
					answer := criticFeedbackJSON(true, "PRIVATE_BLOCKER")
					if request.round == position.round && request.reviewPhase == position.phase {
						switch failure {
						case "malformed":
							answer = `{"approved":true,"has_blockers":false,"feedback":""}`
						case "incomplete":
							answer = incompleteCritiqueJSON
						case "provider":
							return aicli.Result{Answer: approvedCritiqueJSON, Tokens: tokens, UsageComplete: true}, providerErr
						case "blank sanitized feedback":
							answer = criticFeedbackJSON(true, "\x00")
						case "empty redaction":
							answer = criticFeedbackJSON(true, "private # and redacted")
						}
					}
					return aicli.Result{Answer: answer, Tokens: tokens, UsageComplete: true}, nil
				}}
				if failure == "empty redaction" {
					runner.Codex.APIKey, runner.Claude.APIKey = "#", "redacted"
				}
				result, err := runner.Run(context.Background(), job)
				if err == nil || calls != position.calls || len(result.Calls) != calls || result.Answer != "" || result.CriticStopReason != "" || len(result.Changes) != 0 || len(result.Artifacts) != 0 {
					t.Fatalf("final failure acquired publication: calls=%d result=%+v error=%v", calls, result, err)
				}
				if errors.Is(err, providerErr) != (failure == "provider") || errors.Is(err, monitor.ErrTerminalJob) != (failure != "provider") {
					t.Fatalf("final failure cause changed: %v", err)
				}
				if (failure == "blank sanitized feedback" || failure == "empty redaction") && !strings.Contains(err.Error(), "rejection feedback is blank after redaction") {
					t.Fatalf("cap masked sanitized-feedback failure: %v", err)
				}
				last := result.Calls[len(result.Calls)-1]
				if last.Approved || last.NoBlockers || last.HasBlockers != (failure == "incomplete") || last.ReviewComplete || !last.Complete || last.Tokens != tokens || result.validate(job, false) != nil {
					t.Fatalf("final failure lost honest verdict/usage prefix: %+v", result)
				}
				var journal bytes.Buffer
				orchestrator := Orchestrator{WorkRoot: runner.WorkRoot, Logger: log.New(&journal, "", 0)}
				orchestrator.logResult(job, result)
				if strings.Contains(journal.String(), "capped=true") || strings.Contains(journal.String(), "policy_met=true") || strings.Contains(journal.String(), "PRIVATE") {
					t.Fatalf("final failure became a journal decision: %s", journal.String())
				}
				if err := orchestrator.retainReviewEvidence(job, result); err != nil {
					t.Fatal(err)
				}
				if saved := readPrivateReview(t, runner.WorkRoot); saved.CriticStopReason != "" || !slices.Equal(saved.Calls, result.Calls) {
					t.Fatalf("final failure ledger lost prefix: %+v", saved)
				}
			})
		}
	}
}

func TestCappedFinalFullUsesFrozenFixAfterBlockingVerification(t *testing.T) {
	for _, tamperFrozen := range []bool{false, true} {
		t.Run(fmt.Sprintf("tamper frozen=%t", tamperFrozen), func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Fix))
			job.CriticMaxRounds, job.CriticNonblockingRounds = 2, 1
			var liveSource, verificationSource string
			var sequence, decisions []string
			runner := Runner{WorkRoot: t.TempDir(), recordReview: func(calls []CallUsage, _ []Refutation, reason string) error {
				decisions = append(decisions, fmt.Sprintf("%d/%s", len(calls), reason))
				return nil
			}, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				sequence = append(sequence, fmt.Sprintf("%s/%d/%s", request.role, request.round, request.reviewPhase))
				if request.role == roleMain {
					liveSource = request.dir
					writeFixtureFile(t, liveSource, "source.txt", []byte(fmt.Sprintf("reviewed candidate %d\n", request.round)))
					return aicli.Result{Answer: proposalJSON(fmt.Sprintf("Candidate %d", request.round), "")}, nil
				}
				if request.reviewPhase == reviewVerification {
					verificationSource = request.dir
					writeFixtureFile(t, liveSource, "source.txt", []byte("unreviewed background edit\n"))
					return aicli.Result{Answer: criticFeedbackJSON(true, "PRIVATE_VERIFICATION_BLOCKER")}, nil
				}
				if request.round == 1 {
					return aicli.Result{Answer: criticFeedbackJSON(true, "PRIVATE_FIRST_BLOCKER")}, nil
				}
				if request.dir != verificationSource || request.dir == liveSource || !request.freshConversation || len(request.refutations) != 0 || strings.Contains(request.prompt, "PRIVATE_VERIFICATION_BLOCKER") {
					t.Fatalf("forced full lost frozen bytes or independence: %+v", request)
				}
				if got, err := os.ReadFile(filepath.Join(request.dir, "source.txt")); err != nil || string(got) != "reviewed candidate 2\n" {
					t.Fatalf("forced full saw unreviewed live bytes: %q, %v", got, err)
				}
				if tamperFrozen {
					writeFixtureFile(t, request.dir, "source.txt", []byte("critic tampered\n"))
				}
				return aicli.Result{Answer: approvedCritiqueJSON}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			wantSequence := []string{"main/1/", "critic/1/full", "main/2/", "critic/2/verification", "critic/2/full"}
			if !slices.Equal(sequence, wantSequence) || len(result.Calls) != 5 || !result.Calls[3].HasBlockers || !result.Calls[4].Approved || !result.Calls[4].ReviewComplete {
				t.Fatalf("final verification/full evidence changed: calls=%v result=%+v error=%v", sequence, result, err)
			}
			if tamperFrozen {
				if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "frozen candidate source changed") || result.CriticStopReason != "" || result.Answer != "" || len(result.Changes) != 0 || slices.Contains(decisions, "5/round_cap") {
					t.Fatalf("cap bypassed frozen input integrity: %+v decisions=%v error=%v", result, decisions, err)
				}
			} else if err != nil || result.CriticStopReason != "round_cap" || result.Answer != "Candidate 2" || len(result.Changes) != 1 || string(result.Changes[0].Data) != "reviewed candidate 2\n" || !slices.Contains(decisions, "5/round_cap") || result.Validate(job) != nil {
				t.Fatalf("cap did not publish exactly reviewed candidate: %+v decisions=%v error=%v", result, decisions, err)
			}
			var journal bytes.Buffer
			orchestrator := Orchestrator{WorkRoot: runner.WorkRoot, Logger: log.New(&journal, "", 0)}
			orchestrator.logResult(job, result)
			if strings.Contains(journal.String(), "policy_met=true") || strings.Contains(journal.String(), "capped=true") == tamperFrozen || strings.Contains(journal.String(), "PRIVATE") {
				t.Fatalf("journal confused full approval, cap and integrity: %s", journal.String())
			}
			if !tamperFrozen && !strings.Contains(journal.String(), `policy_met=false stop_reason="round_cap" capped=true critic_max_rounds=2 cap_has_blockers=true`) {
				t.Fatalf("journal lost original cap or verification blockers: %s", journal.String())
			}
			if err := orchestrator.retainReviewEvidence(job, result); err != nil {
				t.Fatal(err)
			}
			if saved := readPrivateReview(t, runner.WorkRoot); saved.CriticStopReason != result.CriticStopReason || saved.CriticMaxRounds != 2 || !slices.Equal(saved.Calls, result.Calls) {
				t.Fatalf("cap ledger changed original decisions: %+v", saved)
			}
		})
	}
}

func TestPrivateReviewRecordsActualCapAfterVerdictCheckpoint(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	job.CriticMaxRounds = 2
	result := cappedResultFixture(job)
	orchestrator := Orchestrator{WorkRoot: t.TempDir()}
	beforeDecision := result
	beforeDecision.CriticStopReason = ""
	if err := orchestrator.retainReviewEvidence(job, beforeDecision); err != nil {
		t.Fatal(err)
	}
	if saved := readPrivateReview(t, orchestrator.WorkRoot); saved.CriticStopReason != "" || !slices.Equal(saved.Calls, result.Calls) {
		t.Fatalf("checkpoint inferred a cap from completed verdicts: %+v", saved)
	}
	if err := orchestrator.retainReviewEvidence(job, result); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(orchestrator.WorkRoot, "reviews", job.Identity.AttemptID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"", "approved", "nonblocking"} {
		changed := result
		changed.CriticStopReason = reason
		if err := orchestrator.retainReviewEvidence(job, changed); err == nil {
			t.Fatalf("saved capped decision changed to %q", reason)
		}
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("saved cap evidence changed on refusal: %v", err)
	}
}

func TestCappedAuditStillRequiresInstalledReportValidation(t *testing.T) {
	job := withClaudeCritic(auditRunnerJob(t))
	job.CriticMaxRounds = 1
	fakeOnPath(t, "node", "#!/bin/sh\ncase \"$*\" in *validate-findings.cjs*) exit 9;; esac\nexit 0\n")
	calls := 0
	runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
		calls++
		if request.role == roleCritic {
			return aicli.Result{Answer: criticFeedbackJSON(true, "PRIVATE_AUDIT_BLOCKER")}, nil
		}
		for _, file := range auditArtifactsFixture(job, request.dir, "Reviewed audit report.\n") {
			writeFixtureFile(t, request.outputDir, file.Path, file.Data)
		}
		return aicli.Result{Answer: proposalJSON("Audit candidate", "")}, nil
	}}
	result, err := runner.Run(context.Background(), job)
	if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "audit validate-findings.cjs failed") || calls != 2 || result.CriticStopReason != "round_cap" || result.Answer != "" || len(result.Changes) != 0 {
		t.Fatalf("cap bypassed independent report validation: calls=%d result=%+v error=%v", calls, result, err)
	}
	if result.validate(job, false) != nil || result.Validate(job) == nil {
		t.Fatalf("failed audit lost its safe evidence or became publishable: %+v", result)
	}
}
