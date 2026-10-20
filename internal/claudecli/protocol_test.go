package claudecli

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

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

func TestOutputInvalidAssistantMessageAfterResult(t *testing.T) {
	for _, tc := range []struct{ name, message string }{
		{"missing ID", `{}`},
		{"malformed message", `"invalid"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			data := successfulResult + "\n" + `{"type":"assistant","parent_tool_use_id":null,"message":` + tc.message + `}` + "\n"
			writeChunks(t, &o, data)
			if o.completed || o.result.Answer != "" || o.result.UsageComplete || o.hasCumulative {
				t.Fatalf("invalid assistant kept completed result: %+v completed=%t cumulative=%t", o.result, o.completed, o.hasCumulative)
			}
			o.finish()
			if o.err == nil || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
				t.Fatalf("invalid assistant lost accounting or returned success: %+v error=%v", o.result, o.err)
			}
		})
	}
}

func TestOutputLateInvalidUsageForCoveredAssistant(t *testing.T) {
	const known = `{"type":"result","subtype":"success","result":"answer","modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":11}}}`
	for _, tc := range []struct {
		name, message string
	}{
		{"malformed usage", `{"id":"old","usage":{"input_tokens":"bad"}}`},
		{"missing usage", `{"id":"old"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, recovered := range []bool{false, true} {
				t.Run(fmt.Sprintf("recovered=%t", recovered), func(t *testing.T) {
					o := jsonlOutput{}
					data := assistantUsageEvent("old", 3, 5, 7, 2) + "\n" + known + "\n" +
						`{"type":"assistant","parent_tool_use_id":null,"message":` + tc.message + `}` + "\n"
					writeChunks(t, &o, numberResultEvents(data))
					if o.result.UsageComplete || o.hasCumulative {
						t.Fatalf("invalid late usage retained exact coverage: %+v cumulative=%t", o.result, o.hasCumulative)
					}
					if recovered {
						writeChunks(t, &o, strings.TrimSuffix(known, "}")+`,"result_index":1}`+"\n")
					}
					o.finish()
					if o.err != nil || o.result.Answer != "answer" || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) || o.result.UsageComplete != recovered || o.hasCumulative != recovered {
						t.Fatalf("late usage recovery=%t result=%+v cumulative=%t error=%v", recovered, o.result, o.hasCumulative, o.err)
					}
				})
			}
		})
	}
}

func TestOutputLateOverflowForCoveredAssistant(t *testing.T) {
	for _, tc := range []struct {
		name, initial, late string
		output              int64
	}{
		{
			name:    "component maxima overflow",
			initial: assistantUsageEvent("old", math.MaxInt64-20, 0, 0, 1),
			late:    assistantUsageEvent("old", 1, 0, 0, math.MaxInt64-2),
			output:  1,
		},
		{
			name:    "assistant sum overflow",
			initial: assistantUsageEvent("other", math.MaxInt64-20, 0, 0, 1) + "\n" + assistantUsageEvent("old", 0, 0, 0, 9),
			late:    assistantUsageEvent("old", 0, 0, 0, 30),
			output:  10,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := usage.Tokens{Input: math.MaxInt64 - 20, Output: tc.output}
			result := fmt.Sprintf(`{"type":"result","subtype":"success","result":"answer","modelUsage":{"main":{"inputTokens":%d,"outputTokens":%d}}}`, want.Input, want.Output)
			o := jsonlOutput{}
			writeChunks(t, &o, numberResultEvents(tc.initial+"\n"+result+"\n"+tc.late+"\n"))
			if o.result.UsageComplete || o.hasCumulative {
				t.Fatalf("overflow retained exact coverage: %+v cumulative=%t", o.result, o.hasCumulative)
			}
			o.finish()
			if o.err != nil || o.result.Answer != "answer" || o.result.Tokens != want || o.result.UsageComplete {
				t.Fatalf("overflow lost known usage or retained exact coverage: %+v error=%v", o.result, o.err)
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
			t.Fatalf("valid user content %s refused: %+v error=%v", content, o.result, o.err)
		}
	}
}

const waitCeilingDiagnostic = "Background tasks still running 10m after the last turn (worker); stopping them. Set CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 to wait indefinitely."

func TestWaitCeilingDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{waitCeilingDiagnostic, true}, {waitCeilingDiagnostic + " \t\r", true},
		{"prefix " + waitCeilingDiagnostic, false}, {waitCeilingDiagnostic + " extra", false},
		{"Background tasks still running; ordinary progress", false}, {"", false},
	} {
		if got := isWaitCeilingStop([]byte(tc.line)); got != tc.want {
			t.Errorf("waitCeilingDiagnostic %q matched=%t want=%t", tc.line, got, tc.want)
		}
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

func TestEmptyPromptHasCompleteZeroUsage(t *testing.T) {
	result, err := Run(context.Background(), t.TempDir(), "", false, "", config.ClaudeConfig{})
	if err == nil || !result.UsageComplete || result.Tokens != (usage.Tokens{}) || result.Answer != "" {
		t.Fatalf("prelaunch failure=%+v error=%v", result, err)
	}
}
