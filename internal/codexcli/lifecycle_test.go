package codexcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
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
	const message = `{"type":"item.completed","item":{"type":"agent_message","text":"progress"}}`
	const completed = `{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":10,"output_tokens":7}}`
	for _, scenario := range []struct{ name, events string }{
		{"helper count", string(event)},
		{"oversized final after progress", message + "\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"` + strings.Repeat("x", aicli.MaxEventBytes) + `"}}` + "\n" + completed},
		// This dropped event carries the only ID for an unknown unfinished helper.
		{"oversized hidden helper", `{"type":"item.completed","item":{"type":"collab_tool_call","receiver_thread_ids":["unlogged-child"],"padding":"` + strings.Repeat("x", aicli.MaxEventBytes) + `"}}` + "\n" + message + "\n" + completed},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			installFakeCodex(t, "#!/bin/sh\ncat <<'EVENTS'\n"+`{"type":"thread.started","thread_id":"root"}`+"\n"+scenario.events+"\nEVENTS\n")
			writeUsageRollout(t, os.Getenv("CODEX_HOME"), "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "response", 100, 10, 7), usageEvent("task_complete", "root-turn"))
			result, err := Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
			if !errors.Is(err, aicli.ErrOutputLimit) || strings.Count(err.Error(), aicli.ErrOutputLimit.Error()) != 1 || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 100, CachedInput: 10, Output: 7}) {
				t.Fatalf("output limit accepted or known counters lost: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestRunRequiresCompletionForLatestTurn(t *testing.T) {
	const message = `{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}`
	const started = `{"type":"turn.started"}`
	const complete = `{"type":"turn.completed"}`
	first := message + "\n" + complete + "\n"
	for _, scenario := range []struct{ name, events, failure string }{
		{"initial message without completion", message, "missing turn.completed"},
		{"initial completion without answer", complete, "without a final agent message"},
		{"new message without completion", first + message, "missing turn.completed"},
		{"new turn without completion", first + started, "missing turn.completed"},
		{"new completed turn without answer", first + started + "\n" + complete, "without a final agent message"},
		{"new turn completes", first + started + "\n" + message + "\n" + complete, ""},
		{"new message completes", first + message + "\n" + complete, ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			installFakeCodex(t, "#!/bin/sh\ncat <<'EVENTS'\n"+`{"type":"thread.started","thread_id":"root"}`+"\n"+scenario.events+"\nEVENTS\n")
			writeUsageRollout(t, os.Getenv("CODEX_HOME"), "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "response", 100, 10, 7), usageEvent("task_complete", "root-turn"))
			result, err := Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
			if result.Tokens != (usage.Tokens{Input: 100, CachedInput: 10, Output: 7}) {
				t.Fatalf("lost known usage: %+v", result)
			}
			if scenario.failure == "" {
				if err != nil || result.Answer != "answer" || !result.UsageComplete {
					t.Fatalf("valid final turn refused: result=%+v error=%v", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), scenario.failure) || result.Answer != "" || result.UsageComplete {
				t.Fatalf("unfinished latest turn accepted: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestOutputCountsUniqueHelpersAcrossRepeatedWaits(t *testing.T) {
	o := jsonlOutput{}
	for range 300 {
		for _, kind := range []string{"item.started", "item.completed"} {
			_, _ = fmt.Fprintf(&o, `{"type":%q,"item":{"type":"collab_tool_call","receiver_thread_ids":["child","child"]}}`+"\n", kind)
		}
	}
	_, _ = o.Write([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}` + "\n" + `{"type":"turn.completed"}` + "\n"))
	o.stream.Finish()
	if o.stopErr != nil || o.failed || !o.completed || len(o.children) != 1 || o.children[0] != "child" || len(o.observationOnly) != 1 || o.observationOnly["child"] {
		t.Fatalf("repeated waits consumed helper capacity: stop=%v failed=%t complete=%t children=%v", o.stopErr, o.failed, o.completed, o.children)
	}
}

func TestRunChecksLatestOwnedChildTurn(t *testing.T) {
	first := usageEvent("task_started", "first") + "\n" + usageRecord("child", "first", "response-1", 20, 0, 2)
	second := usageEvent("task_started", "second") + "\n" + usageRecord("child", "second", "response-2", 30, 0, 3)
	for _, scenario := range []struct {
		name, events string
		fails        bool
		want         usage.Tokens
	}{
		{"aborted then completed", first + "\n" + usageEvent("turn_aborted", "first") + "\n" + second + "\n" + usageEvent("task_complete", "second"), false, usage.Tokens{Input: 60, Output: 6}},
		{"completed then new open turn", first + "\n" + usageEvent("task_complete", "first") + "\n" + second, true, usage.Tokens{Input: 60, Output: 6}},
		{"earlier turn still open after latest completes", first + "\n" + second + "\n" + usageEvent("task_complete", "second"), true, usage.Tokens{Input: 60, Output: 6}},
		{"earlier turn still open without counters", usageEvent("task_started", "first") + "\n" + usageEvent("task_started", "second") + "\n" + usageEvent("task_complete", "second"), true, usage.Tokens{Input: 10, Output: 1}},
		{"aborted then completed without counters", usageEvent("task_started", "first") + "\n" + usageEvent("turn_aborted", "first") + "\n" + usageEvent("task_started", "second") + "\n" + usageEvent("task_complete", "second"), false, usage.Tokens{Input: 10, Output: 1}},
		{"old abort after newer completion", first + "\n" + second + "\n" + usageEvent("task_complete", "second") + "\n" + usageEvent("turn_aborted", "first"), false, usage.Tokens{Input: 60, Output: 6}},
		{"old completion cannot close newer turn", first + "\n" + second + "\n" + usageEvent("task_complete", "first"), true, usage.Tokens{Input: 60, Output: 6}},
		{"observed latest abort before unreadable tail", first + "\n" + usageEvent("turn_aborted", "first") + "\n{damaged", true, usage.Tokens{Input: 30, Output: 3}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			installFakeCodex(t, "#!/bin/sh\ncat <<'EVENTS'\n"+`{"type":"thread.started","thread_id":"root"}
{"type":"item.completed","item":{"type":"collab_tool_call","receiver_thread_ids":["child"]}}
{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}
{"type":"turn.completed"}`+"\nEVENTS\n")
			home := os.Getenv("CODEX_HOME")
			writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
			writeUsageRollout(t, home, "child", "root", strings.Split(scenario.events, "\n")...)
			result, err := Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
			if (err != nil) != scenario.fails || scenario.fails && (!strings.Contains(err.Error(), "helper agent work did not complete") || result.Answer != "") || !scenario.fails && result.Answer != "answer" || result.UsageComplete || result.Tokens != scenario.want {
				t.Fatalf("latest child turn misclassified: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestRunRejectsKnownUnfinishedChildrenWithoutRejectingAccountingGaps(t *testing.T) {
	for _, scenario := range []string{"complete", "unfinished", "aborted", "missing counters", "missing log", "inherited turn", "damaged log", "completed then damaged tail", "completed then oversized tail", "completed then partial tail", "completed then new response only"} {
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
				child = append(child, `{"ordinal":20,"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"child-turn","reason":"interrupted"}}`)
			case "missing counters":
				child = []string{usageEvent("task_started", "child-turn"), usageEvent("task_complete", "child-turn")}
			case "inherited turn":
				child = []string{strings.Replace(usageEvent("task_started", "inherited-turn"), `"ordinal":20`, `"ordinal":5`, 1)}
			case "damaged log":
				child = append(child, "{damaged")
			case "completed then damaged tail":
				child = append(child, usageEvent("task_complete", "child-turn"), "{damaged")
			case "completed then oversized tail":
				child = append(child, usageEvent("task_complete", "child-turn"), strings.Repeat("x", aicli.MaxEventBytes+1))
			case "completed then partial tail":
				child = append(child, usageEvent("task_complete", "child-turn"))
			case "completed then new response only":
				child = append(child, usageEvent("task_complete", "child-turn"), usageRecord("child", "new-turn", "new-response", 30, 0, 3))
			}
			if scenario != "missing log" {
				path := writeUsageRollout(t, home, "child", "root", child...)
				if scenario == "completed then partial tail" {
					file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := file.WriteString(`{"type":"event_msg"`)
					if err := errors.Join(writeErr, file.Close()); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
			fails := scenario != "complete" && scenario != "missing counters"
			if (err != nil) != fails || fails && (result.Answer != "" || result.UsageComplete || !strings.Contains(err.Error(), "helper agent work did not complete")) || !fails && result.Answer != "completed response" {
				t.Fatalf("child lifecycle conflated with accounting: result=%+v error=%v", result, err)
			}
			want := usage.Tokens{Input: 30, Output: 3}
			switch scenario {
			case "missing counters", "missing log", "inherited turn":
				want = usage.Tokens{Input: 10, Output: 1}
			case "completed then new response only":
				want = usage.Tokens{Input: 60, Output: 6}
			}
			if result.Tokens != want || result.UsageComplete != (scenario == "complete") {
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
			result, err := Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
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

func TestEventReasonDecodesWithoutClipping(t *testing.T) {
	long := strings.Repeat("x", bounded.DiagnosticBytes+1)
	for _, tc := range []struct{ name, message, raw, want string }{
		{"top level precedence", "top", `{"message":"nested"}`, "top"},
		{"object", "", `{"message":"nested"}`, "nested"},
		{"string", "", `"simple"`, "simple"},
		{"failed object decode", "", `{"message":"partial","message":42}`, ""},
		{"long decoded object", "", `{"message":"` + long + `"}`, long},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventReason(codexEvent{Message: tc.message, Error: json.RawMessage(tc.raw)}); got != tc.want {
				t.Fatalf("reason=%q want=%q", got, tc.want)
			}
		})
	}
}
