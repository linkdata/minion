package claudecli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func assistantUsageEvent(id string, input, created, cached, output int64) string {
	return fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":null,"message":{"id":%q,"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d}}}`, id, input, created, cached, output)
}

func TestOutputRetainsDeduplicatedAssistantUsageBeforeResult(t *testing.T) {
	first := assistantUsageEvent("a", 3, 5, 7, 2)
	later := assistantUsageEvent("a", 3, 5, 7, 11)
	other := assistantUsageEvent("b", 2, 0, 3, 1)
	rootOnly := `{"type":"result","subtype":"success","result":"answer","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}`
	for _, scenario := range []struct {
		name, data string
		want       usage.Tokens
		fails      bool
	}{
		{"partial EOF", first + "\n" + later + "\n" + later + "\n" + other, usage.Tokens{Input: 20, CachedInput: 10, Output: 12}, true},
		{"missing ID", assistantUsageEvent("", 3, 5, 7, 11), usage.Tokens{}, true},
		{"invalid counters", assistantUsageEvent("a", -1, 5, 7, 11), usage.Tokens{}, true},
		{"sum overflow", assistantUsageEvent("a", math.MaxInt64-2, 0, 0, 1) + "\n" + assistantUsageEvent("b", 3, 0, 0, 1), usage.Tokens{Input: math.MaxInt64 - 2, Output: 1}, true},
		{"component maxima overflow", assistantUsageEvent("a", math.MaxInt64-10, 0, 0, 1) + "\n" + assistantUsageEvent("a", 1, 0, 0, math.MaxInt64-5), usage.Tokens{Input: math.MaxInt64 - 10, Output: 1}, true},
		{"success cannot erase known lower bound", first + "\n" + other + "\n" + successfulResult, usage.Tokens{Input: 20, CachedInput: 10, Output: 11}, false},
		{"failed aggregate cannot erase known lower bound", first + "\n" + other + "\n" + strings.Replace(successfulResult, `"is_error":false`, `"is_error":true`, 1), usage.Tokens{Input: 20, CachedInput: 10, Output: 11}, true},
		{"failed result lacks usage", first + "\n" + `{"type":"result","is_error":true,"subtype":"error_during_execution"}`, usage.Tokens{Input: 15, CachedInput: 7, Output: 2}, true},
		{"parser failure", first + "\n" + "{invalid", usage.Tokens{Input: 15, CachedInput: 7, Output: 2}, true},
		{"root fallback cannot erase helper counters", assistantUsageEvent("a", 80, 10, 10, 7) + "\n" + rootOnly, usage.Tokens{Input: 100, CachedInput: 10, Output: 7}, false},
		{"new turn after root-only result", first + "\n" + rootOnly + "\n" + other, usage.Tokens{Input: 20, CachedInput: 10, Output: 3}, true},
		{"failed root fallback cannot erase helper counters", assistantUsageEvent("a", 80, 10, 10, 7) + "\n" + strings.TrimSuffix(rootOnly, "}") + `,"is_error":true}`, usage.Tokens{Input: 100, CachedInput: 10, Output: 7}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, scenario.data+"\n")
			o.finish()
			if o.result.Tokens != scenario.want || (o.err != nil) != scenario.fails || scenario.fails && (o.result.UsageComplete || o.result.Answer != "") {
				t.Fatalf("partial usage=%+v error=%v want=%+v failure=%t", o.result, o.err, scenario.want, scenario.fails)
			}
		})
	}
}

func TestOutputRetainsNewCallsAfterProvisionalAggregate(t *testing.T) {
	const pending = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"}]}`
	const clear = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	const rootOnly = `{"type":"result","subtype":"success","result":"answer","usage":{"input_tokens":900,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":400}}`
	full := func(input, cached, output int64) string {
		return fmt.Sprintf(`{"type":"result","subtype":"success","result":"answer","modelUsage":{"main":{"inputTokens":%d,"cacheCreationInputTokens":0,"cacheReadInputTokens":%d,"outputTokens":%d}}}`, input-cached, cached, output)
	}
	newCall := assistantUsageEvent("new", 1000, 0, 0, 500)
	provisional := pending + "\n" + successfulResult + "\n" + clear + "\n"
	for _, scenario := range []struct {
		name, data string
		want       usage.Tokens
		complete   bool
		fails      bool
	}{
		{"new root call interrupted", provisional + newCall, usage.Tokens{Input: 1015, CachedInput: 7, Output: 511}, false, true},
		{"new root turn invalidates final without task notification", successfulResult + "\n" + newCall, usage.Tokens{Input: 1015, CachedInput: 7, Output: 511}, false, true},
		{"new root blocks deduplicated", provisional + assistantUsageEvent("new", 1000, 0, 0, 400) + "\n" + newCall + "\n" + newCall, usage.Tokens{Input: 1015, CachedInput: 7, Output: 511}, false, true},
		{"covering aggregate replaces lower bound", provisional + newCall + "\n" + full(1015, 7, 511), usage.Tokens{Input: 1015, CachedInput: 7, Output: 511}, true, false},
		{"partial result is overlapping", provisional + newCall + "\n" + rootOnly, usage.Tokens{Input: 1015, CachedInput: 7, Output: 511}, false, false},
		{"incomparable partial result preserves larger output", provisional + newCall + "\n" + strings.Replace(rootOnly, `"output_tokens":400`, `"output_tokens":2000`, 1), usage.Tokens{Input: 1015, CachedInput: 7, Output: 2000}, false, false},
		{"stale aggregate keeps new counters", provisional + newCall + "\n" + successfulResult, usage.Tokens{Input: 1015, CachedInput: 7, Output: 511}, false, false},
		{"later aggregate restores complete coverage", provisional + newCall + "\n" + successfulResult + "\n" + full(1015, 7, 511), usage.Tokens{Input: 1015, CachedInput: 7, Output: 511}, true, false},
		{"late covered block is not added", assistantUsageEvent("old", 2, 0, 1, 2) + "\n" + successfulResult + "\n" + assistantUsageEvent("old", 7, 0, 1, 5), usage.Tokens{Input: 15, CachedInput: 7, Output: 11}, true, false},
		{"late covered block raises independent lower bound", assistantUsageEvent("old", 3, 5, 7, 2) + "\n" + successfulResult + "\n" + assistantUsageEvent("old", 1000, 0, 0, 500), usage.Tokens{Input: 1000, CachedInput: 7, Output: 500}, false, false},
		{"ID without initial usage remains covered", `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old"}}` + "\n" + successfulResult + "\n" + assistantUsageEvent("old", 1000, 0, 0, 500), usage.Tokens{Input: 1000, CachedInput: 7, Output: 500}, false, false},
		{"ID with malformed initial usage remains covered", `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","usage":{"input_tokens":"bad"}}}` + "\n" + successfulResult + "\n" + assistantUsageEvent("old", 1000, 0, 0, 500), usage.Tokens{Input: 1000, CachedInput: 7, Output: 500}, false, false},
		{"delayed delegate overlaps prior aggregate", provisional + strings.Replace(newCall, `"parent_tool_use_id":null`, `"parent_tool_use_id":"tool"`, 1), usage.Tokens{Input: 1000, CachedInput: 7, Output: 500}, false, true},
		{"unknown parent overlaps prior aggregate", provisional + strings.Replace(newCall, `"parent_tool_use_id":null,`, "", 1), usage.Tokens{Input: 1000, CachedInput: 7, Output: 500}, false, true},
		{"unknown counters cannot be erased by stale aggregate", provisional + strings.Replace(newCall, `"parent_tool_use_id":null,`, "", 1) + "\n" + successfulResult, usage.Tokens{Input: 1000, CachedInput: 7, Output: 500}, false, false},
		{"addition overflow preserves known aggregate", provisional + assistantUsageEvent("new", math.MaxInt64-2, 0, 0, 1), usage.Tokens{Input: 15, CachedInput: 7, Output: 11}, false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			o := jsonlOutput{}
			data := numberResultEvents(scenario.data) + "\n"
			writeChunks(t, &o, data)
			o.finish()
			if o.result.Tokens != scenario.want || o.result.UsageComplete != scenario.complete || (o.err != nil) != scenario.fails || scenario.fails && o.result.Answer != "" {
				t.Fatalf("result=%+v error=%v want=%+v complete=%t failure=%t", o.result, o.err, scenario.want, scenario.complete, scenario.fails)
			}
		})
	}
}

func TestOutputAcceptsNativeUserStringContent(t *testing.T) {
	for _, content := range []string{`"<task-notification>ordinary notification</task-notification>"`, `"tool_result: bwrap: quoted user text"`, ` [{"type":"text","text":"ordinary notification"}]`} {
		o := jsonlOutput{}
		_, _ = o.Write([]byte(`{"type":"user","isSynthetic":true,"message":{"content":` + content + "}}\n" + successfulResult + "\n"))
		o.finish()
		if o.err != nil || o.result.Answer != "final answer" {
			t.Fatalf("valid user content refused: %+v error=%v", o.result, o.err)
		}
	}
}

func TestOutputIgnoresAmbientTasksAndStillAwaitsRequestedWork(t *testing.T) {
	const ambient = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"housekeeping","ambient":true}]}`
	const worker = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"housekeeping","ambient":true},{"task_id":"worker"}]}`
	for _, scenario := range []struct {
		name, data string
		fails      bool
	}{
		{"ambient after final", successfulResult + "\n" + ambient, false},
		{"mixed pending", successfulResult + "\n" + worker, true},
		{"pending removed needs new result", successfulResult + "\n" + worker + "\n" + ambient, true},
		{"new result after worker completes", successfulResult + "\n" + worker + "\n" + ambient + "\n" + successfulResult, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			o := jsonlOutput{}
			_, _ = o.Write([]byte(numberResultEvents(scenario.data) + "\n"))
			o.finish()
			if (o.err != nil) != scenario.fails {
				t.Fatalf("result=%+v error=%v", o.result, o.err)
			}
		})
	}
}

func TestOutputRejectsNativeMemoryPressureStops(t *testing.T) {
	const summary = `Background command "tests" was stopped because the system is running low on memory`
	notice := `<task-notification><task-id>worker</task-id><status>killed</status><summary>Background command &quot;tests&quot; was stopped because the system is running low on memory</summary></task-notification>`
	data, _ := json.Marshal(notice)
	system := fmt.Sprintf(`{"type":"system","subtype":"task_notification","status":"stopped","summary":%q}`, summary)
	synthetic := `{"type":"user","isSynthetic":true,"message":{"content":` + string(data) + `}}`
	later := strings.Replace(successfulResult, `"outputTokens":11`, `"outputTokens":22`, 1)
	for _, scenario := range []struct {
		name, event string
		fails       bool
	}{
		{"native system", system, true},
		{"native synthetic string", synthetic, true},
		{"native synthetic text block", `{"type":"user","isSynthetic":true,"message":{"content":[{"type":"text","text":` + string(data) + `}]}}`, true},
		{"ordinary user quote", strings.Replace(synthetic, `"isSynthetic":true`, `"isSynthetic":false`, 1), false},
		{"ambient server stopped", `{"type":"system","subtype":"task_notification","status":"stopped","ambient":true,"summary":"Background server deliberately stopped"}`, false},
		{"ordinary completed task", strings.Replace(system, `"stopped"`, `"completed"`, 1), false},
		{"unrelated assistant summary", strings.Replace(system, `"type":"system"`, `"type":"assistant"`, 1), false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			o := jsonlOutput{}
			_, _ = o.Write([]byte(numberResultEvents(successfulResult+"\n"+scenario.event+"\n"+later) + "\n"))
			o.finish()
			if (o.err != nil) != scenario.fails || scenario.fails && (o.result.Answer != "" || o.result.UsageComplete || !strings.Contains(o.err.Error(), "provider memory pressure")) || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 22}) {
				t.Fatalf("result=%+v error=%v", o.result, o.err)
			}
		})
	}
}

func TestWaitCeilingDiagnostic(t *testing.T) {
	const diagnostic = "Background tasks still running 10m after the last turn (worker); stopping them. Set CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 to wait indefinitely."
	for _, tc := range []struct {
		line string
		want bool
	}{
		{diagnostic, true}, {diagnostic + " \t\r", true},
		{"prefix " + diagnostic, false}, {diagnostic + " extra", false},
		{"Background tasks still running; ordinary progress", false}, {"", false},
	} {
		if got := isWaitCeilingStop([]byte(tc.line)); got != tc.want {
			t.Errorf("diagnostic %q matched=%t want=%t", tc.line, got, tc.want)
		}
	}
}

func TestRunCancelsAfterKnownFatalResult(t *testing.T) {
	for _, scenario := range []struct{ name, event, failure string }{
		{"quota", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`, "provider usage limit reached"},
		{"pressure", `{"type":"system","subtype":"task_notification","status":"stopped","summary":"Background command \"tests\" was stopped because the system is running low on memory"}`, "provider memory pressure"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fakeExecutable(t, "claude", "cat >/dev/null\nsleep 30 &\ncat <<'EVENTS'\n"+scenario.event+"\n"+`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"}]}`+"\n"+successfulResult+"\nEVENTS\nwait\n")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := Run(ctx, t.TempDir(), "task", false, "", config.ClaudeConfig{})
			if err == nil || !strings.Contains(err.Error(), scenario.failure) || ctx.Err() != nil || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
				t.Fatalf("known fatal result did not cancel promptly: result=%+v error=%v parent=%v", result, err, ctx.Err())
			}
		})
	}
}

func TestOutputReportsSafeNativeAPIErrorFields(t *testing.T) {
	for _, scenario := range []struct{ status, kind, want string }{
		{"429", "usage_limit_reached", "HTTP status=429"}, {"402", "model_requires_usage_credits", "HTTP status=402"},
		{"null", "long_context_credits_required", "API error=long_context_credits_required"},
		{"-1", "PRIVATE_KIND", ""}, {"600", "PRIVATE_KIND", ""}, {"1.5", "PRIVATE_KIND", ""}, {`"PRIVATE_STATUS"`, "PRIVATE_KIND", ""},
	} {
		t.Run(scenario.status, func(t *testing.T) {
			failed := strings.TrimSuffix(strings.Replace(successfulResult, `"is_error":false`, `"is_error":true`, 1), "}") + fmt.Sprintf(`,"api_error_status":%s,"api_error":%q,"errors":["PRIVATE_ERROR"]}`, scenario.status, scenario.kind)
			o := jsonlOutput{}
			_, _ = o.Write([]byte(failed + "\n"))
			o.finish()
			if o.err == nil || strings.Contains(o.err.Error(), "PRIVATE") || scenario.want != "" && !strings.Contains(o.err.Error(), scenario.want) || o.result.UsageComplete || o.result.Answer != "" {
				t.Fatalf("unsafe API failure: result=%+v error=%v", o.result, o.err)
			}
		})
	}
}

func TestRunPrelaunchFailuresHaveCompleteZeroUsage(t *testing.T) {
	binary := fakeExecutable(t, "claude", "touch \"$0.called\"\nexit 99\n")
	for _, scenario := range []string{"empty prompt", "credential environment"} {
		t.Run(scenario, func(t *testing.T) {
			prompt := "task"
			if scenario == "empty prompt" {
				prompt = ""
			} else {
				t.Setenv("ANTHROPIC_API_KEY", "dummy-secret")
			}
			result, err := Run(context.Background(), t.TempDir(), prompt, false, "", config.ClaudeConfig{})
			if err == nil || !result.UsageComplete || result.Tokens != (usage.Tokens{}) || result.Answer != "" {
				t.Fatalf("prelaunch failure=%+v error=%v", result, err)
			}
		})
	}
	if _, err := os.Stat(binary + ".called"); !os.IsNotExist(err) {
		t.Fatalf("prelaunch failure started CLI: %v", err)
	}
}

func TestRunRecognizesEarlierWaitCeilingAfterLaterStderr(t *testing.T) {
	const ceiling = "Background tasks still running 10m after the last turn (worker); stopping them. Set CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 to wait indefinitely."
	fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'DIAG' >&2\n"+ceiling+"\n"+strings.Repeat("later diagnostic\n", 2000)+"DIAG\ncat <<'RESULT'\n"+successfulResult+"\nRESULT\n")
	result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if err == nil || !strings.Contains(err.Error(), "wait ceiling") || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
		t.Fatalf("earlier wait stop lost: result=%+v error=%v", result, err)
	}
}
