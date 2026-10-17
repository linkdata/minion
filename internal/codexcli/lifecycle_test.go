package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunOutputLimitPreservesRolloutUsage(t *testing.T) {
	ids := make([]string, maxCodexChildren+1)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	event, err := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "collab_tool_call", "receiver_thread_ids": ids}})
	if err != nil {
		t.Fatal(err)
	}
	installFakeCodex(t, "#!/bin/sh\ncat <<'EVENTS'\n"+`{"type":"thread.started","thread_id":"root"}`+"\n"+string(event)+"\nEVENTS\n")
	writeUsageRollout(t, os.Getenv("CODEX_HOME"), "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "response", 100, 10, 7), usageEvent("task_complete", "root-turn"))
	result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.CodexConfig{})
	if !errors.Is(err, aicli.ErrOutputLimit) || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 100, CachedInput: 10, Output: 7}) {
		t.Fatalf("output limit lost known usage: result=%+v error=%v", result, err)
	}
}

func TestRunRejectsKnownUnfinishedChildrenWithoutRejectingAccountingGaps(t *testing.T) {
	for _, scenario := range []string{"complete", "unfinished", "aborted", "missing counters", "missing log", "inherited turn", "damaged log"} {
		t.Run(scenario, func(t *testing.T) {
			installFakeCodex(t, "#!/bin/sh\ncat <<'EVENTS'\n"+`{"type":"thread.started","thread_id":"root"}
{"type":"item.completed","item":{"type":"collab_tool_call","receiver_thread_ids":["child"]}}
{"type":"item.completed","item":{"type":"agent_message","text":"completed response"}}
{"type":"turn.completed"}`+"\nEVENTS\n")
			home := os.Getenv("CODEX_HOME")
			writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
			child := []string{usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "child-response", 20, 0, 2)}
			switch scenario {
			case "complete":
				child = append(child, usageEvent("task_complete", "child-turn"))
			case "aborted":
				child = append(child, usageEvent("turn_aborted", "child-turn"))
			case "missing counters":
				child = []string{usageEvent("task_started", "child-turn"), usageEvent("task_complete", "child-turn")}
			case "inherited turn":
				child = []string{strings.Replace(usageEvent("task_started", "inherited-turn"), `"ordinal":20`, `"ordinal":5`, 1)}
			case "damaged log":
				child = append(child, "{damaged")
			}
			if scenario != "missing log" {
				writeUsageRollout(t, home, "child", "root", child...)
			}
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.CodexConfig{})
			fails := scenario == "unfinished" || scenario == "aborted"
			if (err != nil) != fails || fails && (result.Answer != "" || result.UsageComplete || !strings.Contains(err.Error(), "helper agent work did not complete")) || !fails && result.Answer != "completed response" {
				t.Fatalf("child lifecycle conflated with accounting: result=%+v error=%v", result, err)
			}
			if result.Tokens.Input < 10 || result.UsageComplete != (scenario == "complete") {
				t.Fatalf("lost accounting distinction: %+v", result)
			}
		})
	}
}

func TestRunQuotesProviderDiagnosticsAndPreservesExitCause(t *testing.T) {
	for _, scenario := range []string{"event", "stderr", "missing completion"} {
		t.Run(scenario, func(t *testing.T) {
			script := "#!/bin/sh\n"
			switch scenario {
			case "event":
				script += `printf '%s\n' '{"type":"turn.failed","error":{"message":"private\nforged journal line"}}'` + "\n"
			case "stderr":
				script += "printf 'private\\nforged journal line' >&2\nexit 7\n"
			case "missing completion":
				script += "printf 'private\\nforged journal line' >&2\n"
			}
			installFakeCodex(t, script)
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.CodexConfig{})
			if err == nil || strings.Contains(err.Error(), "\nforged journal line") || !strings.Contains(err.Error(), `private\nforged journal line`) || result.Answer != "" {
				t.Fatalf("unsafe provider diagnostic: result=%+v error=%v", result, err)
			}
			if scenario == "stderr" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 7 {
					t.Fatalf("lost native exit cause: %v", err)
				}
			}
		})
	}
}
