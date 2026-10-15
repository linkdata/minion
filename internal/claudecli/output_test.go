package claudecli

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
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
		usage *eventUsage
		valid bool
	}{
		{"absent", nil, false},
		{"missing", &eventUsage{}, false},
		{"zero", &eventUsage{Input: &zero, Created: &zero, Cached: &zero, Output: &zero}, true},
		{"negative", &eventUsage{Input: &negative, Created: &zero, Cached: &zero, Output: &zero}, false},
		{"input overflow", &eventUsage{Input: &maximum, Created: &one, Cached: &zero, Output: &zero}, false},
		{"total overflow", &eventUsage{Input: &maximum, Created: &zero, Cached: &zero, Output: &one}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, valid := tc.usage.tokens(); valid != tc.valid {
				t.Fatalf("valid = %v, want %v", valid, tc.valid)
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
