package action

import (
	"bytes"
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
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/workspace"
)

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
					return aicli.Result{Answer: approvedCritiqueJSON}, nil
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

func TestPartialAuditReportPreservesByteLimit(t *testing.T) {
	job := portableJob(command.SecurityAudit)
	for _, size := range []int{maxOutputBytes, maxOutputBytes + 1} {
		result := Result{Version: ProtocolVersion, Identity: job.Identity, SourceRef: job.Source.Head,
			Artifacts: []workspace.File{{Path: "REPORT.md", Mode: workspace.Regular, Data: bytes.Repeat([]byte("x"), size)}},
		}
		if err := result.validate(job, false); (err == nil) != (size == maxOutputBytes) || err != nil && !strings.Contains(err.Error(), "report artifacts exceed limits") {
			t.Fatalf("partial report size %d accepted or misclassified: %v", size, err)
		}
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
