package claudecli

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/usage"
)

func writeChunks(t *testing.T, output *jsonlOutput, data string) {
	t.Helper()
	for len(data) > 0 {
		n := min(7, len(data))
		if wrote, err := output.Write([]byte(data[:n])); wrote != n || err != nil {
			t.Fatalf("Write=%d,%v; want %d bytes", wrote, err, n)
		}
		data = data[n:]
	}
}

func TestDiagnosticRedactionAfterBounding(t *testing.T) {
	raw := "capacity reached " + strings.Repeat("A", 8192) + strings.Repeat("B", 8192) + " session resets"
	boundedText := aicli.DiagnosticText(raw)
	join := strings.Index(boundedText, "...")
	if join < 7 || join+10 > len(boundedText) {
		t.Fatalf("bounded diagnostic has no head/tail join: length=%d", len(boundedText))
	}
	key := boundedText[join-7 : join+10]
	if strings.Contains(raw, key) {
		t.Fatalf("test key already appears before bounding: %q", key)
	}
	o := jsonlOutput{apiKey: key}
	o.observe("task_stopped", raw)
	if len(o.result.Diagnostics) != 1 || o.result.Diagnostics[0].Code != "task_stopped" || strings.Contains(o.result.Diagnostics[0].Message, key) ||
		!strings.Contains(o.result.Diagnostics[0].Message, "capacity reached") || !strings.Contains(o.result.Diagnostics[0].Message, "session resets") || len(o.result.Diagnostics[0].Message) > bounded.DiagnosticBytes {
		t.Fatalf("bounded native observation exposed key or lost evidence: %+v", o.result.Diagnostics)
	}
	detail := o.safeResultDetail(raw)
	if strings.Contains(detail, key) || !strings.Contains(detail, "capacity reached") || !strings.Contains(detail, "session resets") || len(detail) > bounded.DiagnosticBytes || len(o.result.Diagnostics) != 1 {
		t.Fatalf("bounded result detail exposed key or lost evidence: detail=%q diagnostics=%+v", detail, o.result.Diagnostics)
	}
}

func TestUnsafeResultDetailMarksDiagnosticGap(t *testing.T) {
	o := jsonlOutput{apiKey: `"x"`}
	if detail := o.safeResultDetail(`{"x":1}`); detail != "native result detail could not be safely displayed" ||
		len(o.result.Diagnostics) != 1 || o.result.Diagnostics[0].Code != "diagnostics_truncated" {
		t.Fatalf("discarded result detail failed to mark incomplete diagnostics: detail=%q diagnostics=%+v", detail, o.result.Diagnostics)
	}
	o = jsonlOutput{apiKey: `"x"`}
	o.observe("task_stopped", `{"x":1}`)
	if len(o.result.Diagnostics) != 2 || o.result.Diagnostics[0].Code != "diagnostics_truncated" ||
		o.result.Diagnostics[1] != (aicli.ProviderDiagnostic{Code: "task_stopped", Message: "native diagnostic could not be safely displayed"}) {
		t.Fatalf("discarded native observation failed to mark incomplete diagnostics: %+v", o.result.Diagnostics)
	}
}

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
			writeChunks(t, &o, data)
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

func TestOutputRecordsNativeStopSignalsWithoutChangingRecovery(t *testing.T) {
	const quota = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792200000}}`
	const overage = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","isUsingOverage":true}}`
	const stopped = `{"type":"system","subtype":"task_notification","status":"stopped","summary":"worker stopped"}`
	const root = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"fresh"}}`
	const failed = `{"type":"result","subtype":"error_during_execution","is_error":true,"permission_denials":[{}],"terminal_reason":"aborted_tools","api_error":"usage_limit_reached","api_error_status":429}`
	for _, tc := range []struct {
		name, events string
		codes        []string
		fails        bool
	}{
		{"rejected quota and failed result", quota + "\n" + failed, []string{"rate_limit_rejected", "permission_denied", "terminal_reason", "result_error"}, true},
		{"paid overage", overage + "\n" + successfulResult, nil, false},
		{"stopped task assessed by fresh root", stopped + "\n" + root + "\n" + successfulResult, []string{"task_stopped"}, false},
		{"terminal reason abort", strings.TrimSuffix(successfulResult, "}") + `,"terminal_reason":"aborted_tools"}`, []string{"terminal_reason"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.events+"\n")
			o.finish()
			if (o.err != nil) != tc.fails || len(o.result.Diagnostics) != len(tc.codes) {
				t.Fatalf("result=%+v error=%v", o.result, o.err)
			}
			for i, code := range tc.codes {
				if got := o.result.Diagnostics[i]; got.Code != code || got.Message == "" {
					t.Fatalf("diagnostic %d=%+v, want %q", i, got, code)
				}
			}
			if tc.name == "rejected quota and failed result" && (!strings.Contains(o.result.Diagnostics[0].Message, "resets at 2026-10-17T01:20:00Z") || !strings.Contains(o.result.Diagnostics[3].Message, "HTTP status=429")) {
				t.Fatalf("native quota fields lost: %+v", o.result.Diagnostics)
			}
		})
	}
}

func TestOutputKeepsAllStructuredFailuresAfterAnotherFailure(t *testing.T) {
	quota := errors.New("provider usage limit reached")
	pressure := errors.New("background work stopped under provider memory pressure")
	for _, first := range []error{nil, errors.New("invalid JSONL event"), aicli.ErrOutputLimit, fmt.Errorf("wrapped: %w", quota), fmt.Errorf("wrapped: %w", pressure)} {
		t.Run(fmt.Sprint(first), func(t *testing.T) {
			cancels := 0
			o := jsonlOutput{usageLimit: quota, taskFailure: pressure, cancel: func() { cancels++ }}
			if first != nil {
				o.fail(first)
			}
			o.finish()
			if !errors.Is(o.err, quota) || !errors.Is(o.err, pressure) || first != nil && !errors.Is(o.err, first) || cancels != 1 || o.result.Answer != "" || o.result.UsageComplete {
				t.Fatalf("structured cause or original failure lost: error=%v cancels=%d result=%+v", o.err, cancels, o.result)
			}
			if strings.Count(o.err.Error(), quota.Error()) != 1 || strings.Count(o.err.Error(), pressure.Error()) != 1 {
				t.Fatalf("structured cause duplicated: %v", o.err)
			}
		})
	}
}

func TestOutputTaskListsAreScopedAndRequired(t *testing.T) {
	for _, test := range []struct {
		name, event string
		invalid     bool
	}{
		{"unrelated tasks", `{"type":"user","tasks":["unrelated"]}`, false},
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

func TestOutputBackgroundTasks(t *testing.T) {
	const ambient = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"housekeeping","ambient":true}]}`
	const worker = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"housekeeping","ambient":true},{"task_id":"worker"}]}`
	const pending = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	for _, tc := range []struct {
		name, before, after string
		fails               bool
	}{
		{name: "ambient after final", after: ambient},
		{name: "mixed pending", after: worker, fails: true},
		{name: "pending removed needs new result", after: worker + "\n" + ambient, fails: true},
		{name: "new result after worker completes", before: successfulResult + "\n" + worker + "\n" + ambient},
		{name: "unfinished before result", before: pending, fails: true},
		{name: "unfinished after result", after: pending, fails: true},
		{name: "completion before result", before: pending + "\n" + empty},
		{name: "completion after result needs new answer", before: pending, after: empty, fails: true},
		{name: "answer after completion", before: pending + "\n" + successfulResult + "\n" + empty},
		{name: "work after result needs new answer", after: pending + "\n" + empty, fails: true},
		{name: "answer after new work", before: successfulResult + "\n" + pending + "\n" + empty},
		{name: "failed command recovered", before: `{"type":"system","subtype":"task_notification","task_id":"test","status":"failed"}`},
		{name: "user text is not task state", after: `{"type":"user","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}`},
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
	const structured = `{"answer":"published","refutations":"private"}`
	waiting := strings.Replace(successfulResult, "final answer", "waiting", 1)
	empty := strings.Replace(successfulResult, "final answer", "", 1)
	formatted := strings.TrimSuffix(empty, "}") + `,"structured_output":` + structured + `}`
	denied := strings.TrimSuffix(empty, "}") + `,"permission_denials":[{"tool_name":"Bash"}]}`
	failed := strings.Replace(successfulResult, `"subtype":"success"`, `"subtype":"error_during_execution"`, 1)
	for _, tc := range []struct {
		name, data, answer, failure string
		structured, unterminated    bool
	}{
		{name: "deferred turns after task completion", data: numberResultEvents(pending + "\n" + clear + "\n" + waiting + "\n" + waiting + "\n" + empty + "\n" + successfulResult), answer: "final answer"},
		{name: "latest empty never falls back", data: numberResultEvents(waiting + "\n" + empty), failure: "missing successful Claude result"},
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
		{name: "single unindexed result", data: successfulResult, answer: "final answer", unterminated: true},
		{name: "multiple unindexed results", data: successfulResult + "\n" + successfulResult, failure: "require native result_index"},
		{name: "indexed then unindexed", data: numberResultEvents(successfulResult) + "\n" + successfulResult, failure: "require native result_index"},
		{name: "unindexed then indexed", data: successfulResult + "\n" + strings.TrimSuffix(successfulResult, "}") + `,"result_index":1}`, failure: "require native result_index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cancelled := false
			o := jsonlOutput{structured: tc.structured, cancel: func() { cancelled = true }}
			data := tc.data
			if !tc.unterminated {
				data += "\n"
			}
			writeChunks(t, &o, data)
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
		{"cached input overflow", eventUsage{Input: &zero, Created: &maximum, Cached: &one, Output: &zero}, false},
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
		{"zero aggregate with current root usage", root, `{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}`, fallback, false},
		{"zero aggregate with zero root usage", `{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}`, `{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}`, usage.Tokens{}, true},
		{"partial aggregate keeps root counters", root, `{"main":{"inputTokens":20,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}`, usage.Tokens{Input: 20, CachedInput: 7, Output: 11}, false},
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
			writeChunks(t, &o, data)
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
