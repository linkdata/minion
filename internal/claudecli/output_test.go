package claudecli

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/usage"
)

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
		{name: "terminal history", before: `{"type":"system","subtype":"background_tasks_changed","tasks":[{"status":"completed"},{"status":"failed"},{"status":"stopped"}]}`},
		{name: "active alongside history", after: `{"type":"system","subtype":"background_tasks_changed","tasks":[{"status":"completed"},{"status":"running"}]}`, fails: true},
		{name: "optional task stopped", after: `{"type":"system","subtype":"task_notification","task_id":"server","status":"stopped"}`},
		{name: "failed command recovered", before: `{"type":"system","subtype":"task_notification","task_id":"test","status":"failed"}`},
		{name: "assistant text is not task state", after: `{"type":"assistant","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cancelled := false
			o := jsonlOutput{cancel: func() { cancelled = true }}
			if _, err := o.Write([]byte(tc.before + "\n" + successfulResult + "\n" + tc.after + "\n")); err != nil {
				t.Fatal(err)
			}
			o.finish()
			if (o.err != nil || !o.completed) != tc.fails || cancelled != (o.err != nil) || o.result.UsageComplete == tc.fails || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
				t.Fatalf("output = %+v, error %v, cancelled %v", o.result, o.err, cancelled)
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

func TestTailBuffer(t *testing.T) {
	var buffer tailBuffer
	for _, s := range []string{strings.Repeat("a", maxDiagnosticBytes+1), "end"} {
		if _, err := buffer.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if len(buffer.String()) != maxDiagnosticBytes || !strings.HasSuffix(buffer.String(), "end") {
		t.Fatal("diagnostics were not bounded")
	}
}
