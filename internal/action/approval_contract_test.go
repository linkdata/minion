package action

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
)

func TestRunnerRecordsOnlyParsedSuccessfulCriticApprovals(t *testing.T) {
	for _, scenario := range []string{"approval", "rejection", "malformed", "provider failure"} {
		t.Run(scenario, func(t *testing.T) {
			job := portableJob(command.Comment)
			job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, 1
			providerErr := errors.New("critic provider failed")
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				if request.role == roleMain {
					return aicli.Result{Answer: proposalJSON("Complete answer", "")}, nil
				}
				answer := `{"approved":true,"has_blockers":false,"feedback":""}`
				switch scenario {
				case "rejection":
					answer = `{"approved":false,"has_blockers":true,"feedback":"Repair the defect."}`
				case "malformed":
					answer = `{"approved":true,"has_blockers":false,"feedback":"contradictory"}`
				case "provider failure":
					return aicli.Result{Answer: answer}, providerErr
				}
				return aicli.Result{Answer: answer}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			wantApproval := scenario == "approval"
			if (err == nil) != wantApproval || len(result.Calls) != 2 || result.Calls[0].Approved || result.Calls[1].Approved != wantApproval {
				t.Fatalf("critic approval attestation: calls=%+v err=%v", result.Calls, err)
			}
			if wantApproval {
				if err := result.Validate(job); err != nil || result.Answer != "Complete answer" {
					t.Fatalf("approved result invalid: %+v,%v", result, err)
				}
			} else {
				if result.Answer != "" || result.validate(job, false) != nil {
					t.Fatalf("failed critic evidence lost: %+v,%v", result, err)
				}
				if scenario == "provider failure" && !errors.Is(err, providerErr) {
					t.Fatalf("provider failure lost: %v", err)
				}
			}
		})
	}
}

func TestRunnerRefusesRenderedOverflowBeforeCritic(t *testing.T) {
	for _, audit := range []bool{false, true} {
		t.Run(map[bool]string{false: "answer", true: "audit report"}[audit], func(t *testing.T) {
			job := portableJob(command.Comment)
			job.Critic, job.CriticMaxRounds = &RoleSettings{Provider: "claude"}, 1
			if audit {
				installActionTestSkills(t, testAuditSkillURL)
				job.Action, job.Command, job.Main.Skills = command.SecurityAudit, "security-audit", []string{testAuditSkillURL}
			}
			text := strings.Repeat("a\n", 25000)
			mainCalls, criticCalls := 0, 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				if request.role == roleCritic {
					criticCalls++
					return aicli.Result{Answer: `{"approved":true,"has_blockers":false,"feedback":""}`}, nil
				}
				mainCalls++
				if audit {
					if err := os.WriteFile(filepath.Join(request.outputDir, "REPORT.md"), []byte(text), 0600); err != nil {
						t.Fatal(err)
					}
					return aicli.Result{Answer: proposalJSON("Complete report", "")}, nil
				}
				return aicli.Result{Answer: proposalJSON(text, "")}, nil
			}}
			result, err := runner.Run(context.Background(), job)
			if !errors.Is(err, monitor.ErrTerminalJob) || !strings.Contains(err.Error(), "publication limit") || mainCalls != 1 || criticCalls != 0 || result.Answer != "" || len(result.Calls) != 1 {
				t.Fatalf("overflow reached critic or successful output: main=%d critic=%d calls=%+v err=%v", mainCalls, criticCalls, result.Calls, err)
			}
			if audit {
				if len(result.Artifacts) != 1 || result.Artifacts[0].Path != "REPORT.md" || string(result.Artifacts[0].Data) != text || result.validate(job, false) != nil {
					t.Fatalf("unpublishable report lost as partial evidence: %+v", result.Artifacts)
				}
			}
		})
	}
}

func TestAuditResultRejectsRenderedOverflowButKeepsPartialReport(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	result := completeAuditResult(job)
	result.Answer = strings.Repeat("a\u0085", 19000)
	for i := range result.Artifacts {
		if result.Artifacts[i].Path == "REPORT.md" {
			result.Artifacts[i].Data = []byte(result.Answer)
		}
	}
	if err := result.Validate(job); err == nil || !strings.Contains(err.Error(), "answer") {
		t.Fatalf("unpublishable report imported as complete: %v", err)
	}
	if err := result.validate(job, false); err != nil {
		t.Fatalf("bounded partial report cannot be retained: %v", err)
	}
}

func TestStatusBoundsRemainingRuntimeWithoutRenewingBudget(t *testing.T) {
	job := portableJob(command.Comment)
	job.RuntimeBudget = time.Minute
	for _, remaining := range []time.Duration{-1, 0, time.Nanosecond, time.Minute, time.Minute + 1} {
		status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusRunning, Phase: PhaseMain, Round: 1, RuntimeRemaining: remaining}
		valid := remaining >= 0 && remaining <= job.RuntimeBudget
		if err := status.Validate(job); (err == nil) != valid {
			t.Fatalf("remaining=%s validation=%v want valid=%t", remaining, err, valid)
		}
		if !valid {
			continue
		}
		data, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Status
		if err := json.Unmarshal(data, &decoded); err != nil || decoded.RuntimeRemaining != remaining || decoded.Validate(job) != nil || !strings.Contains(string(data), `"runtime_remaining_ns":`) {
			t.Fatalf("remaining runtime lost in transport: %s,%v", data, err)
		}
	}
	job.RuntimeBudget = time.Nanosecond
	status := Status{Version: ProtocolVersion, Identity: job.Identity, State: StatusReady, Phase: PhasePreparing, RuntimeRemaining: 2 * time.Nanosecond}
	if status.Validate(job) == nil {
		t.Fatal("status renewed the supplied runtime budget")
	}
}

func TestJobRoundBudgetIsAbsentWithoutCritic(t *testing.T) {
	job := portableJob(command.Comment)
	for _, rounds := range []int{-1, 0, 1, config.DefaultCriticMaxRounds, config.MaxCriticMaxRounds} {
		job.CriticMaxRounds = rounds
		if err := job.Validate(); (err == nil) != (rounds == 0) {
			t.Fatalf("inactive critic budget %d validation=%v", rounds, err)
		}
	}
}
