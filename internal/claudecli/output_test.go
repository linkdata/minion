package claudecli

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/usage"
)

func TestOutputNativeUsageLimit(t *testing.T) {
	// Native events: overage rejection can accompany an allowed warning.
	const warning = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1792200000,"rateLimitType":"five_hour","utilization":0.99,"isUsingOverage":false,"overageStatus":"rejected","overageDisabledReason":"org_level_disabled","overageEnabled":false,"surpassedThreshold":0.9,"unifiedWindows":{"five_hour":{"utilization":0.99,"resetsAt":1792200000},"seven_day":{"utilization":0.24,"resetsAt":1792342800}}}}`
	const rejected = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792200000,"rateLimitType":"five_hour","isUsingOverage":false,"overageStatus":"rejected","overageDisabledReason":"org_level_disabled","unifiedWindows":{"five_hour":{"utilization":1,"resetsAt":1792200000},"seven_day":{"utilization":0.24,"resetsAt":1792342800}}}}`
	// A quota failure can carry subtype=success and cumulative helper usage.
	const failed = `{"type":"result","subtype":"success","is_error":true,"result_index":0,"result":"PRIVATE_RESULT\nforged journal line","errors":["PRIVATE_ERROR"],"modelUsage":{"claude-opus-5-5":{"inputTokens":36,"cacheCreationInputTokens":160092,"cacheReadInputTokens":435540,"outputTokens":11489}}}`
	const limit = "provider usage limit reached; resets at 2026-10-17T01:20:00Z"
	known := usage.Tokens{Input: 15, CachedInput: 7, Output: 11}
	for _, test := range []struct {
		name, data   string
		want         usage.Tokens
		failsAtWrite bool
		failure      string
	}{
		{"warning permits completion", warning + "\n" + successfulResult, known, false, ""},
		{"paid overage permits completion", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","isUsingOverage":true,"overageStatus":"allowed"}}` + "\n" + successfulResult, known, false, ""},
		{"active overage permits completion", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","overageInUse":true}}` + "\n" + successfulResult, known, false, ""},
		{"native failure keeps helper counters", warning + "\n" + rejected + "\n" + failed, usage.Tokens{Input: 595668, CachedInput: 435540, Output: 11489}, true, limit},
		{"rejection at EOF", rejected, usage.Tokens{}, false, limit},
		{"later counters parsed before cancellation", rejected + "\n" + successfulResult, known, true, limit},
		{"warning cannot clear rejection", rejected + "\n" + warning + "\n" + successfulResult, known, true, limit},
		{"rejection invalidates earlier answer", successfulResult + "\n" + rejected, known, false, limit},
		{"limit takes precedence over denials", rejected + "\n" + strings.TrimSuffix(successfulResult, "}") + `,"permission_denials":[{"tool_name":"Bash"}]}`, known, true, limit},
		{"limit takes precedence over invalid index", rejected + "\n" + strings.TrimSuffix(successfulResult, "}") + `,"result_index":9}`, known, true, limit},
		{"limit takes precedence over unfinished tasks", rejected + "\n" + `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}`, usage.Tokens{}, false, limit},
	} {
		t.Run(test.name, func(t *testing.T) {
			cancelled := false
			o := jsonlOutput{cancel: func() { cancelled = true }}
			data := test.data + "\n"
			for len(data) > 0 {
				n := min(7, len(data))
				_, _ = o.Write([]byte(data[:n]))
				data = data[n:]
			}
			if (o.err != nil) != test.failsAtWrite || cancelled != test.failsAtWrite || o.result.Tokens != test.want {
				t.Fatalf("usage limit interrupted counter collection: tokens=%+v error=%v cancelled=%t", o.result.Tokens, o.err, cancelled)
			}
			o.finish()
			if test.failure == "" {
				if o.err != nil || cancelled || !o.result.UsageComplete || o.result.Answer != "final answer" {
					t.Fatalf("warning refused successful result: %+v error=%v", o.result, o.err)
				}
			} else if o.err == nil || o.err.Error() != test.failure || !cancelled || o.result.UsageComplete || o.result.Answer != "" {
				t.Fatalf("unsafe or missing usage limit failure: tokens=%+v error=%v complete=%t answer=%q", o.result.Tokens, o.err, o.result.UsageComplete, o.result.Answer)
			}
		})
	}
}

func TestOutputTaskListsAreScopedAndRequired(t *testing.T) {
	for _, test := range []struct {
		name, event string
		invalid     bool
	}{
		{"unrelated tasks", `{"type":"assistant","tasks":["unrelated"]}`, false},
		{"unrelated errors", `{"type":"system","errors":{"unrelated":true}}`, false},
		{"missing", `{"type":"system","subtype":"background_tasks_changed"}`, true},
		{"null", `{"type":"system","subtype":"background_tasks_changed","tasks":null}`, true},
		{"object", `{"type":"system","subtype":"background_tasks_changed","tasks":{}}`, true},
		{"string entries", `{"type":"system","subtype":"background_tasks_changed","tasks":["worker"]}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := jsonlOutput{}
			_, _ = o.Write([]byte(successfulResult + "\n" + test.event + "\n"))
			o.finish()
			if (o.err != nil) != test.invalid || (o.result.Answer == "") != test.invalid {
				t.Fatalf("result=%+v error=%v", o.result, o.err)
			}
		})
	}
}

func TestOutputUsageLimitResetValidation(t *testing.T) {
	for _, test := range []struct{ name, reset, suffix string }{
		{"missing", "", ""}, {"zero", "0", ""}, {"negative", "-1", ""}, {"fractional", "1.5", ""},
		{"string", `"PRIVATE_RESET"`, ""}, {"null", "null", ""}, {"object", `{"private":"reset"}`, ""},
		{"integer overflow", "9223372036854775808", ""}, {"time overflow", "9223372036854775807", ""},
		{"outside RFC3339", "253402300800", ""}, {"first positive", "1", "; resets at 1970-01-01T00:00:01Z"},
		{"last RFC3339 second", "253402300799", "; resets at 9999-12-31T23:59:59Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reset := ""
			if test.reset != "" {
				reset = `,"resetsAt":` + test.reset
			}
			data := fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"PRIVATE_TYPE\nforged journal line"%s}}`, reset)
			o := jsonlOutput{}
			_, _ = o.Write([]byte(data))
			o.finish()
			if o.err == nil || o.err.Error() != "provider usage limit reached"+test.suffix || o.result.UsageComplete {
				t.Fatalf("reset was not safely bounded: %v", o.err)
			}
		})
	}
}

// numberResultEvents models the native CLI's contiguous per-turn delivery indices.
func numberResultEvents(data string) string {
	lines := strings.Split(data, "\n")
	index := 0
	for i, line := range lines {
		if strings.HasPrefix(line, `{"type":"result"`) {
			lines[i] = strings.TrimSuffix(line, "}") + `,"result_index":` + strconv.Itoa(index) + `}`
			index++
		}
	}
	return strings.Join(lines, "\n")
}

func TestOutputFragments(t *testing.T) {
	for _, chunk := range []int{1, 7, 1000} {
		o := jsonlOutput{}
		data := "{\"type\":\"system\",\"message\":\"status text\"}\n{\"type\":\"assistant\",\"result\":\"interim\"}\n" + successfulResult
		for len(data) > 0 {
			n := min(chunk, len(data))
			if _, err := o.Write([]byte(data[:n])); err != nil {
				t.Fatal(err)
			}
			data = data[n:]
		}
		o.finish()
		if o.err != nil || !o.completed || o.result.Answer != "final answer" || !o.result.UsageComplete {
			t.Fatalf("output = %+v", o)
		}
	}
}

func TestOutputMissingFinalResult(t *testing.T) {
	o := jsonlOutput{}
	_, _ = o.Write([]byte("{\"type\":\"system\",\"subtype\":\"init\"}\n"))
	o.finish()
	if o.err == nil || !strings.Contains(o.err.Error(), "missing successful final result from Claude") {
		t.Fatalf("missing final result diagnostic = %v", o.err)
	}
}

func TestOutputBackgroundTasks(t *testing.T) {
	const pending = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	for _, tc := range []struct {
		name, before, after string
		fails               bool
	}{
		{name: "unfinished before result", before: pending, fails: true},
		{name: "unfinished after result", after: pending, fails: true},
		{name: "completion before result", before: pending + "\n" + empty},
		{name: "completion after result needs new answer", before: pending, after: empty, fails: true},
		{name: "answer after completion", before: pending + "\n" + successfulResult + "\n" + empty},
		{name: "work after result needs new answer", after: pending + "\n" + empty, fails: true},
		{name: "answer after new work", before: successfulResult + "\n" + pending + "\n" + empty},
		{name: "unassessed task stopped", after: `{"type":"system","subtype":"task_notification","task_id":"server","status":"stopped"}`, fails: true},
		{name: "failed command recovered", before: `{"type":"system","subtype":"task_notification","task_id":"test","status":"failed"}`},
		{name: "assistant text is not task state", after: `{"type":"assistant","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cancelled := false
			o := jsonlOutput{cancel: func() { cancelled = true }}
			if _, err := o.Write([]byte(numberResultEvents(tc.before + "\n" + successfulResult + "\n" + tc.after + "\n"))); err != nil {
				t.Fatal(err)
			}
			o.finish()
			if (o.err != nil || !o.completed) != tc.fails || cancelled != (o.err != nil) || o.result.UsageComplete == tc.fails || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
				t.Fatalf("output = %+v, error %v, cancelled %v", o.result, o.err, cancelled)
			}
		})
	}
}

func TestOutputNativeTurnResults(t *testing.T) {
	const pending = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"}]}`
	const clear = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	const structured = `{"answer":"published","review_notes":"private"}`
	waiting := strings.Replace(successfulResult, "final answer", "waiting", 1)
	empty := strings.Replace(successfulResult, "final answer", "", 1)
	formatted := strings.TrimSuffix(empty, "}") + `,"structured_output":` + structured + `}`
	denied := strings.TrimSuffix(empty, "}") + `,"permission_denials":[{"tool_name":"Bash"}]}`
	failed := strings.Replace(successfulResult, `"subtype":"success"`, `"subtype":"error_during_execution"`, 1)
	for _, tc := range []struct {
		name, data, answer, failure string
		structured                  bool
	}{
		{name: "deferred turns after task completion", data: numberResultEvents(pending + "\n" + clear + "\n" + waiting + "\n" + waiting + "\n" + empty + "\n" + successfulResult), answer: "final answer"},
		{name: "latest empty never falls back", data: numberResultEvents(waiting + "\n" + empty), failure: "missing successful Claude result"},
		{name: "new work invalidates candidate", data: numberResultEvents(successfulResult + "\n" + pending + "\n" + clear), failure: "missing successful final result"},
		{name: "fresh indexed answer after new work", data: numberResultEvents(successfulResult + "\n" + pending + "\n" + clear + "\n" + successfulResult), answer: "final answer"},
		{name: "only final structured schema required", data: numberResultEvents(waiting + "\n" + empty + "\n" + formatted), structured: true, answer: structured},
		{name: "latest missing schema never falls back", data: numberResultEvents(formatted + "\n" + waiting), structured: true, failure: "missing Claude structured output"},
		{name: "first error cannot be replaced", data: numberResultEvents(failed + "\n" + successfulResult), failure: "result failed:"},
		{name: "later error cannot be replaced", data: numberResultEvents(waiting + "\n" + failed + "\n" + successfulResult), failure: "result failed:"},
		{name: "first denial cannot be replaced", data: numberResultEvents(denied + "\n" + successfulResult), failure: "denied tool permissions"},
		{name: "empty turn denial cannot be replaced", data: numberResultEvents(waiting + "\n" + denied + "\n" + successfulResult), failure: "denied tool permissions"},
		{name: "duplicate indices", data: numberResultEvents(successfulResult) + "\n" + numberResultEvents(successfulResult), failure: "result_index 0, want 1"},
		{name: "gap in indices", data: numberResultEvents(successfulResult) + "\n" + strings.TrimSuffix(successfulResult, "}") + `,"result_index":2}`, failure: "result_index 2, want 1"},
		{name: "nonzero initial index", data: strings.TrimSuffix(successfulResult, "}") + `,"result_index":1}`, failure: "result_index 1, want 0"},
		{name: "negative index", data: strings.TrimSuffix(successfulResult, "}") + `,"result_index":-1}`, failure: "result_index -1, want 0"},
		{name: "single unindexed result", data: successfulResult, answer: "final answer"},
		{name: "multiple unindexed results", data: successfulResult + "\n" + successfulResult, failure: "require native result_index"},
		{name: "indexed then unindexed", data: numberResultEvents(successfulResult) + "\n" + successfulResult, failure: "require native result_index"},
		{name: "unindexed then indexed", data: successfulResult + "\n" + strings.TrimSuffix(successfulResult, "}") + `,"result_index":1}`, failure: "require native result_index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cancelled := false
			o := jsonlOutput{structured: tc.structured, cancel: func() { cancelled = true }}
			data := tc.data + "\n"
			for len(data) != 0 {
				n := min(7, len(data))
				if _, err := o.Write([]byte(data[:n])); err != nil {
					t.Fatal(err)
				}
				data = data[n:]
			}
			o.finish()
			if tc.failure != "" {
				if o.err == nil || !strings.Contains(o.err.Error(), tc.failure) || !cancelled || o.result.UsageComplete {
					t.Fatalf("invalid result stream accepted: error=%v cancelled=%v complete=%v", o.err, cancelled, o.result.UsageComplete)
				}
			} else if o.err != nil || cancelled || !o.completed || !o.result.UsageComplete || o.result.Answer != tc.answer {
				t.Fatalf("valid result stream rejected: result=%+v error=%v cancelled=%v", o.result, o.err, cancelled)
			}
		})
	}
}

func TestOutputLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		init func(*jsonlOutput)
		data string
	}{
		{"line", func(*jsonlOutput) {}, strings.Repeat("x", maxEventBytes+1)},
		{"total", func(o *jsonlOutput) { o.bytes = maxOutputBytes }, "x"},
		{"events", func(o *jsonlOutput) { o.events = maxEvents }, "{}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cancelled := false
			o := jsonlOutput{cancel: func() { cancelled = true }}
			tc.init(&o)
			if _, err := o.Write([]byte(tc.data)); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(o.err, aicli.ErrOutputLimit) || !cancelled {
				t.Fatalf("limit not enforced: %v, %v", o.err, cancelled)
			}
		})
	}
}

func TestSandboxStartupFailureAborts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message any
		fails   bool
	}{
		{"seccomp", "Exit code 1\napply-seccomp: write /proc/self/setgroups: Permission denied", true},
		{"bubblewrap", "Exit code 1\nbwrap: No permissions to create a new namespace", true},
		{"initialization", "Sandbox is required but failed to initialize: Failed to create bridge sockets after 5 attempts.", true},
		{"test failure", "Exit code 1\nFAIL expected 2, got 1", false},
		{"text blocks", []map[string]string{{"type": "text", "text": "Exit code 1"}, {"type": "text", "text": "apply-seccomp: namespace unavailable"}}, true},
		{"test failure blocks", []map[string]string{{"type": "text", "text": "Exit code 1\nFAIL expected 2, got 1"}}, false},
		{"nontext block", []map[string]string{{"type": "other", "text": "apply-seccomp: not a text block"}}, false},
		{"empty content", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cancelled := false
			o := jsonlOutput{cancel: func() { cancelled = true }}
			data, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result", "is_error": true, "content": tc.message}}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := o.Write(append(data, '\n')); err != nil {
				t.Fatal(err)
			}
			if _, err := o.Write([]byte(successfulResult + "\n")); err != nil {
				t.Fatal(err)
			}
			if cancelled != tc.fails || (o.err != nil) != tc.fails || tc.fails && o.result.Answer != "" {
				t.Fatalf("sandbox failure accepted: %+v", o)
			}
		})
	}
}

func TestUsageValidation(t *testing.T) {
	zero, one, negative, maximum := int64(0), int64(1), int64(-1), int64(math.MaxInt64)
	for _, tc := range []struct {
		name  string
		usage eventUsage
		valid bool
	}{
		{"missing", eventUsage{}, false},
		{"zero", eventUsage{Input: &zero, Created: &zero, Cached: &zero, Output: &zero}, true},
		{"negative", eventUsage{Input: &negative, Created: &zero, Cached: &zero, Output: &zero}, false},
		{"input overflow", eventUsage{Input: &maximum, Created: &one, Cached: &zero, Output: &zero}, false},
		{"total overflow", eventUsage{Input: &maximum, Created: &zero, Cached: &zero, Output: &one}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, valid := tc.usage.tokens(); valid != tc.valid {
				t.Fatalf("valid = %v, want %v", valid, tc.valid)
			}
		})
	}
}

func TestOutputWholeTreeUsage(t *testing.T) {
	const root = `{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}`
	const models = `{"claude-main":{"inputTokens":13,"cacheCreationInputTokens":17,"cacheReadInputTokens":19,"outputTokens":23},"claude-helper":{"inputTokens":29,"cacheCreationInputTokens":31,"cacheReadInputTokens":37,"outputTokens":41}}`
	fallback := usage.Tokens{Input: 15, CachedInput: 7, Output: 11}
	for _, tc := range []struct {
		name, root, models string
		want               usage.Tokens
		complete           bool
	}{
		{"root and helper models", root, models, usage.Tokens{Input: 146, CachedInput: 56, Output: 64}, true},
		{"aggregate ignores malformed root", `"bad"`, models, usage.Tokens{Input: 146, CachedInput: 56, Output: 64}, true},
		{"zero aggregate", root, `{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}`, usage.Tokens{}, true},
		{"absent aggregate", root, "", fallback, false},
		{"empty aggregate", root, "{}", fallback, false},
		{"malformed aggregate", root, `"bad"`, fallback, false},
		{"partially decoded aggregate", root, `{"main":{"inputTokens":"bad","cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}`, fallback, false},
		{"missing counter", root, `{"main":{"inputTokens":1,"outputTokens":2}}`, fallback, false},
		{"aggregate total overflow", root, `{"main":{"inputTokens":9223372036854775806,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0},"helper":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}`, fallback, false},
		{"malformed fallback", `"bad"`, "null", usage.Tokens{}, false},
		{"invalid fallback", `{"input_tokens":9223372036854775807,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}`, "null", usage.Tokens{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := `{"type":"result","subtype":"success","result":"answer"`
			if tc.root != "" {
				data += `,"usage":` + tc.root
			}
			if tc.models != "" {
				data += `,"modelUsage":` + tc.models
			}
			o := jsonlOutput{}
			if _, err := o.Write([]byte(data + "}\n")); err != nil {
				t.Fatal(err)
			}
			o.finish()
			if o.err != nil || o.result.Answer != "answer" || o.result.Tokens != tc.want || o.result.UsageComplete != tc.complete {
				t.Fatalf("output = %+v, %v; want tokens %+v, complete %v", o.result, o.err, tc.want, tc.complete)
			}
		})
	}
}

func TestOutputPreservesRecordedUsage(t *testing.T) {
	const pending = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}`
	const clear = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	const models = `"modelUsage":{"main":{"inputTokens":13,"cacheCreationInputTokens":17,"cacheReadInputTokens":19,"outputTokens":23},"helper":{"inputTokens":29,"cacheCreationInputTokens":31,"cacheReadInputTokens":37,"outputTokens":41}}`
	const aggregate = `{"type":"result","subtype":"success","result":"answer",` + models + `}`
	const fallback = `"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}`
	const partial = `{"type":"result","subtype":"success","result":"provisional",` + fallback + `}`
	known := usage.Tokens{Input: 146, CachedInput: 56, Output: 64}
	for _, tc := range []struct {
		name, first, intermediate, final string
		want                             usage.Tokens
		failed, complete                 bool
	}{
		{name: "error without usage", final: `{"type":"result","subtype":"error_during_execution","is_error":true}`, want: known, failed: true},
		{name: "error with smaller root fallback", final: `{"type":"result","subtype":"error_during_execution","is_error":true,` + fallback + `}`, want: known, failed: true},
		{name: "success without usage", final: `{"type":"result","subtype":"success","result":"final"}`, want: known},
		{name: "success with invalid aggregate and root fallback", final: `{"type":"result","subtype":"success","result":"final","modelUsage":{"main":{"inputTokens":"invalid"}},` + fallback + `}`, want: known},
		{name: "incomparable aggregate keeps each known counter", final: `{"type":"result","subtype":"success","result":"final","modelUsage":{"main":{"inputTokens":100,"cacheCreationInputTokens":0,"cacheReadInputTokens":40,"outputTokens":100}}}`, want: usage.Tokens{Input: 146, CachedInput: 56, Output: 100}},
		{name: "identical cumulative aggregate not summed", intermediate: aggregate, final: aggregate, want: known, complete: true},
		{name: "dominating aggregate restores completeness", intermediate: partial, final: `{"type":"result","subtype":"success","result":"final","modelUsage":{"main":{"inputTokens":150,"cacheCreationInputTokens":10,"cacheReadInputTokens":60,"outputTokens":100}}}`, want: usage.Tokens{Input: 220, CachedInput: 60, Output: 100}, complete: true},
		{name: "component merge refuses total overflow", first: `{"type":"result","subtype":"success","result":"provisional","modelUsage":{"main":{"inputTokens":9223372036854775797,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":9}}}`, final: `{"type":"result","subtype":"success","result":"final","modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":9223372036854775805}}}`, want: usage.Tokens{Input: 9223372036854775797, Output: 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			first := tc.first
			if first == "" {
				first = aggregate
			}
			data := numberResultEvents(pending + "\n" + first + "\n" + tc.intermediate + "\n" + clear + "\n" + tc.final + "\n")
			for len(data) != 0 {
				n := min(7, len(data))
				if _, err := o.Write([]byte(data[:n])); err != nil {
					t.Fatal(err)
				}
				data = data[n:]
			}
			o.finish()
			if o.result.Tokens != tc.want || o.result.UsageComplete != tc.complete || (o.err != nil) != tc.failed {
				t.Fatalf("known counters not preserved: got tokens=%+v complete=%v error=%v; want tokens=%+v complete=%v failed=%v", o.result.Tokens, o.result.UsageComplete, o.err, tc.want, tc.complete, tc.failed)
			}
			if tc.failed && !strings.Contains(o.err.Error(), "result failed:") {
				t.Fatalf("unexpected failure: %v", o.err)
			}
		})
	}
}
