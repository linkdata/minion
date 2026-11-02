package codexcli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunFreshAndBoundaryHelpers(t *testing.T) {
	start := usageEventAt(1, "task_started", "child-turn")
	record := usageRecordAt(2, "child", "child-turn", "response", 20, 5, 2)
	done := usageEventAt(3, "task_complete", "child-turn")
	inherited := `{"ordinal":1,"type":"session_meta","payload":{"id":"root","source":"exec"}}`
	boundary := int64(1)
	for _, scenario := range []struct {
		name              string
		boundary          *int64
		events            []string
		fails, incomplete bool
		childInput        int64
	}{
		{name: "fresh", events: []string{start, record, done}, childInput: 20},
		{name: "fresh missing counters", events: []string{start, done}, incomplete: true},
		{name: "inclusive first own lifecycle", boundary: &boundary, events: []string{start, record, done}, childInput: 20},
		{name: "inclusive first own response", boundary: &boundary, events: []string{usageRecordAt(1, "child", "child-turn", "response", 20, 5, 2), done}, childInput: 20},
		{name: "fresh inherited header", events: []string{inherited, start, record, done}, fails: true, incomplete: true, childInput: 20},
		{name: "late inherited header", events: []string{start, record, done, inherited}, fails: true, incomplete: true, childInput: 20},
		{name: "fresh latest turn open", events: []string{start, record, done, usageEventAt(4, "task_started", "later")}, fails: true, incomplete: true, childInput: 20},
		{name: "inclusive nested spawn", boundary: &boundary, events: []string{`{"ordinal":1,"type":"event_msg","payload":{"type":"collab_agent_spawn_end","new_thread_id":"missing-grandchild"}}`, usageEventAt(2, "task_started", "child-turn"), usageRecordAt(3, "child", "child-turn", "response", 20, 5, 2), usageEventAt(4, "task_complete", "child-turn")}, fails: true, incomplete: true, childInput: 20},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			installFakeCodex(t, "#!/bin/sh\ncat <<'EVENTS'\n"+`{"type":"thread.started","thread_id":"root"}
{"type":"item.completed","item":{"type":"collab_tool_call","receiver_thread_ids":["child"]}}
{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}
{"type":"turn.completed"}`+"\nEVENTS\n")
			home := os.Getenv("CODEX_HOME")
			writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
			writeUsageRolloutBoundary(t, home, "child", "root", scenario.boundary, scenario.events...)
			result, err := Run(t.Context(), t.TempDir(), "synthetic helper lifecycle fixture", false, "", config.CodexConfig{})
			want := usage.Tokens{Input: 10, Output: 1}
			if scenario.childInput != 0 {
				want = usage.Tokens{Input: 30, CachedInput: 5, Output: 3}
			}
			if errors.Is(err, errUnverifiedChildren) != scenario.fails || (err != nil) != scenario.fails || result.UsageComplete == scenario.incomplete || result.Tokens != want || scenario.fails && result.Answer != "" || !scenario.fails && result.Answer != "answer" {
				t.Fatalf("fresh helper result=%+v err=%v want=%+v fail=%t incomplete=%t", result, err, want, scenario.fails, scenario.incomplete)
			}
			wantScope := "invocation"
			if scenario.incomplete {
				wantScope = "known_subset"
			}
			if coverage := result.UsageCoverage; coverage.Scope != wantScope || !coverage.MissingFinalReport || coverage.MalformedFinalReport || coverage.UnverifiedHelpers != scenario.fails || coverage.PartialReport != scenario.incomplete {
				t.Fatalf("fresh helper usage coverage=%+v, want scope=%s helper gap=%t partial=%t", coverage, wantScope, scenario.fails, scenario.incomplete)
			}
		})
	}
}

func TestRunLabelsMalformedFinalUsageDespiteCompleteRollout(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"answer\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10}}'\n")
	writeUsageRollout(t, os.Getenv("CODEX_HOME"), "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
	result, err := Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
	if err != nil || result.Answer != "answer" || !result.UsageComplete || result.Tokens != (usage.Tokens{Input: 10, Output: 1}) ||
		result.UsageCoverage != (usage.Coverage{Scope: "invocation", MalformedFinalReport: true}) {
		t.Fatalf("malformed final report discarded verified rollout usage: result=%+v err=%v", result, err)
	}
}

func TestRunCancelsMalformedOutputAndRetainsParseCause(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{damaged'\nexec sleep 30\n")
	writeUsageRollout(t, os.Getenv("CODEX_HOME"), "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := Run(ctx, t.TempDir(), "task", false, "", config.CodexConfig{})
	if err == nil || !strings.Contains(err.Error(), "invalid JSONL event") || errors.Is(err, aicli.ErrOutputLimit) || ctx.Err() != nil || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 10, Output: 1}) {
		t.Fatalf("malformed output did not fail promptly with its parse cause: result=%+v err=%v parent=%v", result, err, ctx.Err())
	}
	var native *exec.ExitError
	if !errors.As(err, &native) {
		t.Fatalf("native exit cause lost: %v", err)
	}
}

func TestRunLabelsMalformedTurnUsageAfterParseFailure(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '{\"type\":\"turn.started\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"answer\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":\"invalid\",\"cached_input_tokens\":0,\"output_tokens\":1}}'\n")
	writeUsageRollout(t, os.Getenv("CODEX_HOME"), "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
	result, err := Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
	if err == nil || !strings.Contains(err.Error(), "invalid JSONL event") || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 10, Output: 1}) {
		t.Fatalf("malformed turn usage did not fail with retained counters: result=%+v err=%v", result, err)
	}
	if coverage := result.UsageCoverage; coverage.Scope != "known_subset" || coverage.MissingFinalReport || !coverage.MalformedFinalReport || !coverage.PartialReport {
		t.Fatalf("malformed turn usage mislabeled: %+v", coverage)
	}
}

func TestRunCancelsJSONLOutputLimit(t *testing.T) {
	installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}'\nprintf '%*s\\n' "+strconv.Itoa(aicli.MaxEventBytes+1)+" ''\nexec sleep 30\n")
	writeUsageRollout(t, os.Getenv("CODEX_HOME"), "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := Run(ctx, t.TempDir(), "task", false, "", config.CodexConfig{})
	if !errors.Is(err, aicli.ErrOutputLimit) || ctx.Err() != nil || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 10, Output: 1}) {
		t.Fatalf("JSONL limit did not cancel CLI with known usage: result=%+v err=%v parent=%v", result, err, ctx.Err())
	}
}
