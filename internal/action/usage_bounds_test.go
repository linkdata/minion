package action

import (
	"bytes"
	"context"
	"log"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/usage"
)

func TestUsageAggregateBounds(t *testing.T) {
	requireActionProcessGroup(t)
	for _, scenario := range []struct {
		name          string
		first, second int64
		revise        bool
		want          int64
		complete      bool
	}{
		{"overflow", math.MaxInt64/2 + 1, math.MaxInt64/2 + 1, false, math.MaxInt64/2 + 1, false},
		{"overflow stays incomplete", math.MaxInt64/2 + 1, math.MaxInt64/2 + 1, true, math.MaxInt64/2 + 4, false},
		{"exact maximum", math.MaxInt64 / 2, math.MaxInt64/2 + 1, false, math.MaxInt64, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store := &usage.Store{Dir: filepath.Join(t.TempDir(), "usage")}
			var journal bytes.Buffer
			var native []usage.Tokens
			work := t.TempDir()
			orchestrator := attemptFixture(&Orchestrator{
				Usage: store, Main: "codex", Adversarial: "claude", WorkRoot: work, Logger: log.New(&journal, "", 0),
				Runner: Runner{WorkRoot: work, runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					input := int64(1)
					switch len(native) {
					case 0:
						input = scenario.first
					case 1:
						input = scenario.second
					}
					tokens := usage.Tokens{Input: input}
					native = append(native, tokens)
					answer := proposalJSON("No findings.", "Private evidence.")
					if request.role == roleCritic {
						answer = approvedCritiqueJSON
						if scenario.revise && request.round == 1 {
							answer = `{"approved":false,"has_blockers":true,"review_complete":true,"feedback":"Check the remaining detail."}`
						}
					}
					return aicli.Result{Answer: answer, Tokens: tokens, UsageComplete: true}, nil
				}},
			})
			finish, err := orchestrator.trackUsage(usageJob())
			if err != nil {
				t.Fatal(err)
			}
			job := withClaudeCritic(testJob(command.Comment))
			job.Identity.AttemptID = orchestrator.attemptID
			result, err := orchestrator.Runner.Execute(t.Context(), job, newExecutionCheckpoints(orchestrator, job))
			if err != nil {
				t.Fatalf("valid paid work failed on aggregate usage: %v", err)
			}
			if err := finish(nil); err != nil {
				t.Fatalf("could not finalize the usage record: %v", err)
			}
			wantCalls := 2
			if scenario.revise {
				wantCalls = 5
			}
			if result.Answer != "No findings." || len(native) != wantCalls || len(result.Calls) != wantCalls {
				t.Fatalf("native work repeated or lost: answer=%q calls=%d evidence=%d", result.Answer, len(native), len(result.Calls))
			}
			for i, call := range result.Calls {
				if call.Tokens != native[i] || !call.Complete || call.ReviewComplete != (call.Role == roleCritic) {
					t.Fatalf("native per-call evidence changed at %d: %+v, want %+v", i, call, native[i])
				}
			}
			records, err := usage.ReadAll(store.Dir)
			if err != nil || len(records) != 1 {
				t.Fatalf("invalid persisted usage: records=%d err=%v", len(records), err)
			}
			got := records[0]
			if got.Status != "succeeded" || got.FinishedAt == nil || got.Tokens != (usage.Tokens{Input: scenario.want}) || got.UsageComplete != scenario.complete || got.CodexCalls+got.ClaudeCalls != wantCalls {
				t.Fatalf("usage did not retain/finalize the representable bound: %+v", got)
			}
			const warning = "action usage counters invalid or overflowed; retaining previous reported totals for attempt "
			wantWarnings := 0
			if !scenario.complete {
				wantWarnings = 1
			}
			var warnings []string
			for _, line := range strings.Split(journal.String(), "\n") {
				if strings.HasPrefix(line, warning) {
					warnings = append(warnings, line)
				}
			}
			if len(warnings) != wantWarnings || len(strings.Join(warnings, "\n")) > 256 {
				t.Fatalf("overflow warning missing or unbounded: %q", journal.String())
			}
			if saved := readPrivateReview(t, work); saved.CriticStopReason != result.CriticStopReason || len(saved.Calls) != wantCalls {
				t.Fatalf("usage checkpoints bypassed required critic evidence: %+v", saved)
			}
		})
	}
}
