package codexcli

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

func TestJSONLOutputStreamsOnlyCompleteKnownItems(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }}
	lines := []string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"before ID"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"before ID"}}`,
		`{"type":"thread.started","thread_id":"native-root"}`,
		`{"type":"item.started","item":{"type":"agent_message","text":"partial"}}`,
		`{"type":"item.updated","item":{"type":"command_execution","aggregated_output":"partial"}}`,
		`{"type":"item.completed","item":{"type":"reasoning","text":"visible summary"}}`,
		`{"type":"item.completed","item":{"type":"reasoning","content":"hidden content"}}`,
		`{"type":"item.completed","item":{"type":"todo_list","items":["not a tool"]}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"complete message"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./...","aggregated_output":"PASS","exit_code":0}}`,
		`{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"a.go","kind":"update"}]}}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"example","tool":"read","result":"done"}}`,
		`{"type":"item.completed","item":{"type":"dynamic_tool_call","tool":"custom","result":"done"}}`,
		`{"type":"item.completed","item":{"type":"web_search","query":"example","results":[]}}`,
		`{"type":"item.completed","item":{"type":"collab_tool_call","tool":"wait","receiver_thread_ids":["child"]}}`,
		`{"type":"turn.completed"}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	for len(data) != 0 {
		chunk := min(13, len(data))
		_, _ = output.Write(data[:chunk])
		data = data[chunk:]
	}
	output.stream.Finish()
	if len(got) != 10 || got[0] != (aicli.StreamEvent{Kind: "assistant"}) ||
		got[1] != (aicli.StreamEvent{NativeID: "native-root", Kind: "identity"}) ||
		got[2] != (aicli.StreamEvent{NativeID: "native-root", Kind: "reasoning_summary", Text: "visible summary"}) ||
		got[3] != (aicli.StreamEvent{NativeID: "native-root", Kind: "assistant", Text: "complete message"}) {
		t.Fatalf("stream identity or filters: %+v", got)
	}
	for _, event := range got[4:] {
		if event.Kind != "tool" || event.NativeID != "native-root" || !json.Valid([]byte(event.Text)) {
			t.Fatalf("tool was not complete native item JSON: %+v", event)
		}
	}
	if !strings.Contains(got[4].Text, `"command":"go test ./..."`) || !strings.Contains(got[4].Text, `"aggregated_output":"PASS"`) ||
		!strings.Contains(got[5].Text, `"changes":[`) || !strings.Contains(got[6].Text, `"result":"done"`) {
		t.Fatalf("tool payload fields lost: %+v", got)
	}
	if output.threadID != "native-root" || !output.completed || output.finalMessage != "complete message" {
		t.Fatalf("streaming changed native accounting: %+v", output)
	}
}

func TestJSONLOutputReasoningSummaryRedactsOnlyEmittedSummary(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) },
		redact: func(text string) string { return strings.ReplaceAll(text, "private-key", "[redacted]") }}
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"native-root"}`,
		`{"type":"item.completed","item":{"type":"reasoning","text":"visible private-key summary"}}`,
		`{"type":"item.completed","item":{"type":"reasoning","content":"unexposed private-key"}}`,
	}, "\n") + "\n"
	_, _ = output.Write([]byte(stream))
	if !slices.Equal(got, []aicli.StreamEvent{
		{NativeID: "native-root", Kind: "identity"},
		{NativeID: "native-root", Kind: "reasoning_summary", Text: "visible [redacted] summary"},
	}) {
		t.Fatalf("reasoning summary was not isolated and redacted: %+v", got)
	}
}

func TestJSONLOutputStreamGapOnceAndStopsAfterIdentityConflict(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{expectedID: "expected", onEvent: func(event aicli.StreamEvent) { got = append(got, event) },
		redact: func(text string) string {
			if strings.Contains(text, "DROP") {
				return ""
			}
			return text
		}}
	_, _ = output.Write([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"pre-ID"}}` + "\n" +
		`{"type":"thread.started","thread_id":"expected"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"DROP"}}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"kept"}}` + "\n" +
		`{"type":"thread.started","thread_id":"foreign"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"after conflict"}}` + "\n"))
	if !slices.Equal(got, []aicli.StreamEvent{{Kind: "assistant"}, {NativeID: "expected", Kind: "identity"}, {NativeID: "expected", Kind: "assistant", Text: "kept"}}) || output.stopErr == nil {
		t.Fatalf("gap repeated, redaction escaped, or conflict emitted: got=%+v error=%v", got, output.stopErr)
	}
}

func TestJSONLOutputIdentityConflictAfterValidStartMarksGap(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }}
	_, _ = output.Write([]byte(`{"type":"thread.started","thread_id":"root"}` + "\n" +
		`{"type":"thread.started","thread_id":"foreign"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"discarded"}}` + "\n"))
	if output.stopErr == nil || !slices.Equal(got, []aicli.StreamEvent{
		{NativeID: "root", Kind: "identity"}, {Kind: "output_stop"},
	}) {
		t.Fatalf("identity conflict did not mark incomplete capture: events=%+v error=%v", got, output.stopErr)
	}
}

func TestJSONLOutputMalformedEventAfterValidStartMarksGap(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }}
	_, _ = output.Write([]byte(`{"type":"thread.started","thread_id":"root"}` + "\n" +
		`{malformed}` + "\n"))
	if output.parseErr == nil || !slices.Equal(got, []aicli.StreamEvent{
		{NativeID: "root", Kind: "identity"}, {Kind: "invalid_json"},
	}) {
		t.Fatalf("malformed event did not mark incomplete capture: events=%+v error=%v", got, output.parseErr)
	}
}

func TestJSONLOutputUnknownCompletedItemLeavesCoverageUnproved(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }}
	_, _ = output.Write([]byte(`{"type":"thread.started","thread_id":"root"}` + "\n" +
		`{"type":"item.completed","item":{"type":"new_tool","result":"visible"}}` + "\n"))
	if !slices.Equal(got, []aicli.StreamEvent{{NativeID: "root", Kind: "identity"}, {Kind: "unknown_item"}}) {
		t.Fatalf("unknown native item falsely treated as covered: %+v", got)
	}
}

func TestJSONLOutputMissingCompletedItemLeavesCoverageUnproved(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }}
	_, _ = output.Write([]byte(`{"type":"thread.started","thread_id":"root"}` + "\n" +
		`{"type":"item.completed"}` + "\n" +
		`{"type":"item.completed","item":null}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"known"}}` + "\n" +
		`{"type":"turn.completed"}` + "\n"))
	if !slices.Equal(got, []aicli.StreamEvent{{NativeID: "root", Kind: "identity"}, {Kind: "missing_item"}, {NativeID: "root", Kind: "assistant", Text: "known"}}) || !output.completed {
		t.Fatalf("missing item falsely treated as covered: events=%+v complete=%t", got, output.completed)
	}
}

func TestSessionStreamUsesLiveNativeIdentityAcrossResume(t *testing.T) {
	binary := installFakeCodex(t, `#!/bin/sh
cat >/dev/null
n=1
if [ -f "$0.count" ]; then n=$(( $(cat "$0.count") + 1 )); fi
printf '%s' "$n" > "$0.count"
id=root
if [ "$n" -eq 3 ]; then id=fresh; fi
printf '{"type":"thread.started","thread_id":"%s"}\n' "$id"
printf '{"type":"item.completed","item":{"type":"agent_message","text":"message-%s"}}\n' "$n"
while [ ! -f "$0.continue.$n" ]; do sleep 0.01; done
printf '%s\n' '{"type":"turn.completed"}'
`)
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	var got []aicli.StreamEvent
	var callbackErr error
	session.OnEvent = func(event aicli.StreamEvent) {
		got = append(got, event)
		if event.Kind == "assistant" {
			call := strings.TrimPrefix(event.Text, "message-")
			callbackErr = os.WriteFile(binary+".continue."+call, nil, 0600)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	source := t.TempDir()
	for call := 1; call <= 3; call++ {
		if call == 3 {
			session.NewConversation()
		}
		result, err := session.Run(ctx, source, "task", false, "", config.CodexConfig{})
		if err != nil || callbackErr != nil || result.Answer != "message-"+strconv.Itoa(call) {
			t.Fatalf("call %d: result=%+v error=%v callback=%v", call, result, err, callbackErr)
		}
	}
	want := []aicli.StreamEvent{
		{NativeID: "root", Kind: "identity"}, {NativeID: "root", Kind: "assistant", Text: "message-1"}, {NativeID: "root", Kind: "final", Text: "message-1"},
		{NativeID: "root", Kind: "identity"}, {NativeID: "root", Kind: "assistant", Text: "message-2"}, {NativeID: "root", Kind: "final", Text: "message-2"},
		{NativeID: "fresh", Kind: "identity"}, {NativeID: "fresh", Kind: "assistant", Text: "message-3"}, {NativeID: "fresh", Kind: "final", Text: "message-3"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("native identities or final events = %+v, want %+v", got, want)
	}
}

func TestSessionStreamRedactsConfiguredCredentialsAndRequiresCompletion(t *testing.T) {
	for _, test := range []struct {
		name, suffix string
		wantFinal    bool
	}{
		{"complete", `{"type":"turn.completed"}`, true},
		{"unfinished", `{"type":"turn.started"}`, false},
		{"malformed tail", `{damaged`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			installFakeCodex(t, `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"command_execution","command":"echo secret-key","aggregated_output":"secret-key"}}' '{"type":"item.completed","item":{"type":"agent_message","text":"secret-key"}}' '`+test.suffix+`'
`)
			var session Session
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			var got []aicli.StreamEvent
			session.OnEvent = func(event aicli.StreamEvent) { got = append(got, event) }
			_, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{APIKey: "secret-key"})
			if (err == nil) != test.wantFinal {
				t.Fatalf("completion error = %v", err)
			}
			finals, gaps := 0, 0
			for _, event := range got {
				if event.Kind == "final" {
					finals++
				}
				if event.Kind == "invalid_json" {
					gaps++
					if test.name != "malformed tail" || event.NativeID != "" || event.Text != "" {
						t.Fatalf("unexpected malformed-stream gap: %+v", event)
					}
					continue
				}
				if event.NativeID != "root" || strings.Contains(event.Text, "secret-key") ||
					event.Kind != "identity" && !strings.Contains(event.Text, "[redacted]") {
					t.Fatalf("stream credential escaped or content lost: %+v", event)
				}
			}
			wantGaps := 0
			if test.name == "malformed tail" {
				wantGaps = 1
			}
			if (finals == 1) != test.wantFinal || gaps != wantGaps || len(got) != 3+finals+gaps {
				t.Fatalf("false final event or missing completed item: %+v", got)
			}
		})
	}
}

func TestSessionStreamDoesNotAcceptFinalWithUnverifiedHelper(t *testing.T) {
	installFakeCodex(t, `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"collab_tool_call","tool":"spawn_agent","receiver_thread_ids":["missing-child"]}}' '{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}' '{"type":"turn.completed"}'
`)
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	var got []aicli.StreamEvent
	session.OnEvent = func(event aicli.StreamEvent) { got = append(got, event) }
	result, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
	if err == nil || result.Answer != "" {
		t.Fatalf("unverified helper accepted: result=%+v error=%v", result, err)
	}
	for _, event := range got {
		if event.Kind == "final" {
			t.Fatalf("unverified helper emitted final: %+v", got)
		}
	}
}
