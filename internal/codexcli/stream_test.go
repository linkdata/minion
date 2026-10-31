package codexcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

func TestJSONLOutputStreamsCompleteItemsAndMarksUnboundProgress(t *testing.T) {
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
	if len(got) != 11 || got[0] != (aicli.StreamEvent{Kind: "assistant"}) ||
		got[1] != (aicli.StreamEvent{NativeID: "native-root", Kind: "identity"}) ||
		got[2] != (aicli.StreamEvent{NativeID: "native-root", Kind: "content_missing"}) ||
		got[3] != (aicli.StreamEvent{NativeID: "native-root", Kind: "reasoning_summary", Text: "visible summary"}) ||
		got[4] != (aicli.StreamEvent{NativeID: "native-root", Kind: "assistant", Text: "complete message"}) {
		t.Fatalf("stream identity or filters: %+v", got)
	}
	for _, event := range got[5:] {
		if event.Kind != "tool" || event.NativeID != "native-root" || !json.Valid([]byte(event.Text)) {
			t.Fatalf("tool was not complete native item JSON: %+v", event)
		}
	}
	if !strings.Contains(got[5].Text, `"command":"go test ./..."`) || !strings.Contains(got[5].Text, `"aggregated_output":"PASS"`) ||
		!strings.Contains(got[6].Text, `"changes":[`) || !strings.Contains(got[7].Text, `"result":"done"`) {
		t.Fatalf("tool payload fields lost: %+v", got)
	}
	if output.threadID != "native-root" || !output.completed || output.finalMessage != "complete message" {
		t.Fatalf("streaming changed native accounting: %+v", output)
	}
}

func writeCodexStreamEvent(t *testing.T, output *jsonlOutput, event any) {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = output.Write(append(data, '\n'))
}

func TestJSONLOutputProgressSnapshotsAndCompletedItems(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }, allowIncomplete: true}
	writeCodexStreamEvent(t, &output, map[string]any{"type": "thread.started", "thread_id": "root"})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "item": map[string]any{"id": "message", "type": "agent_message", "text": "old"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.updated", "item": map[string]any{"id": "message", "type": "agent_message", "text": "new snapshot"}})
	if output.pending["message"].text != "new snapshot" || output.pendingBytes != len("new snapshot") {
		t.Fatalf("update was appended rather than replacing the snapshot: %+v", output.pending)
	}
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "item": map[string]any{"id": "tool", "type": "command_execution", "aggregated_output": "old"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.updated", "item": map[string]any{"id": "tool", "type": "command_execution", "aggregated_output": "new"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.completed", "item": map[string]any{"id": "message", "type": "agent_message", "text": "complete"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.completed", "item": map[string]any{"id": "tool", "type": "command_execution", "aggregated_output": "complete"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.completed", "item": map[string]any{"id": "message", "type": "agent_message", "text": "duplicate"}})
	output.stream.Finish()
	output.flushPending()
	if len(output.pending) != 0 || output.pendingBytes != 0 || output.finalMessage != "complete" || len(got) != 4 ||
		got[0] != (aicli.StreamEvent{NativeID: "root", Kind: "identity"}) ||
		got[1] != (aicli.StreamEvent{NativeID: "root", Kind: "assistant", Text: "complete"}) ||
		got[2].NativeID != "root" || got[2].Kind != "tool" || !strings.Contains(got[2].Text, `"aggregated_output":"complete"`) ||
		got[3] != (aicli.StreamEvent{NativeID: "root", Kind: "content_missing"}) {
		t.Fatalf("snapshot, completion, or duplicate handling: events=%+v pending=%+v", got, output.pending)
	}
}

func TestJSONLOutputKnownCredentialSuppressesOpenAssistant(t *testing.T) {
	const key = "tail-secret"
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }}
	writeCodexStreamEvent(t, &output, map[string]any{"type": "thread.started", "thread_id": "root"})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "item": map[string]any{"id": "message", "type": "agent_message", "text": "obsolete tail-se"}})
	message := "Useful work: " + strings.Repeat("A", 200) + " credential=tail-se " + strings.Repeat("B", 200) + " " + key
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.updated", "item": map[string]any{"id": "message", "type": "agent_message", "text": message}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "turn.failed", "message": "stopped"})
	output.stream.Finish()
	output.flushPending()
	if !slices.Equal(got, []aicli.StreamEvent{{NativeID: "root", Kind: "identity"}, {NativeID: "root", Kind: "content_missing"}}) {
		t.Fatalf("known key prefix in middle of open snapshot escaped: %+v", got)
	}
}

func TestJSONLOutputUnsafeOpenItemsUseContentMissing(t *testing.T) {
	const key = "split-secret"
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }}
	writeCodexStreamEvent(t, &output, map[string]any{"type": "thread.started", "thread_id": "root"})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "item": map[string]any{"id": "message", "type": "agent_message", "text": "split-"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.updated", "item": map[string]any{"id": "message", "type": "agent_message", "text": "split-se"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "item": map[string]any{"id": "tool", "type": "command_execution", "command": "echo " + key}})
	output.stream.Finish()
	output.flushPending()
	if !slices.Equal(got, []aicli.StreamEvent{
		{NativeID: "root", Kind: "identity"}, {NativeID: "root", Kind: "content_missing"},
	}) {
		t.Fatalf("unsafe open content reached records: %+v", got)
	}
}

func TestJSONLOutputBoundsPendingProgress(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }, allowIncomplete: true}
	writeCodexStreamEvent(t, &output, map[string]any{"type": "thread.started", "thread_id": "root"})
	for i := range maxPendingItems + 1 {
		writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "item": map[string]any{"id": fmt.Sprintf("item-%d", i), "type": "agent_message", "text": "x"}})
	}
	if len(output.pending) != maxPendingItems || output.pendingBytes != maxPendingItems ||
		!slices.Equal(got, []aicli.StreamEvent{{NativeID: "root", Kind: "identity"}, {NativeID: "root", Kind: "content_missing"}}) {
		t.Fatalf("pending-item bound or coverage gap: pending=%d bytes=%d events=%+v", len(output.pending), output.pendingBytes, got)
	}
	big := strings.Repeat("x", aicli.MaxEventBytes-200)
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.updated", "item": map[string]any{"id": "item-0", "type": "agent_message", "text": big}})
	if output.pendingBytes > aicli.MaxEventBytes || output.pending["item-0"].text != "" {
		t.Fatalf("pending-byte bound lost: bytes=%d item=%+v", output.pendingBytes, output.pending["item-0"])
	}
}

func TestJSONLOutputBoundsCompletedIdentities(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{threadID: "root", onEvent: func(event aicli.StreamEvent) { got = append(got, event) }, completedItems: make(map[string]bool, maxCompletedIDs)}
	for i := range maxCompletedIDs {
		output.completedItems[fmt.Sprintf("old-%d", i)] = true
	}
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.completed", "item": map[string]any{"id": "helper", "type": "collab_tool_call", "tool": "spawn_agent", "receiver_thread_ids": []string{"child"}}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.completed", "item": map[string]any{"id": "new", "type": "agent_message", "text": "untracked"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "turn.completed"})
	if len(output.completedItems) != maxCompletedIDs || output.finalMessage != "untracked" || !output.completed ||
		!slices.Equal(output.children, []string{"child"}) || !output.spawns["child"] ||
		!slices.Equal(got, []aicli.StreamEvent{{NativeID: "root", Kind: "content_missing"}}) {
		t.Fatalf("completed-item cap lost lifecycle or coverage: count=%d final=%q children=%v events=%+v", len(output.completedItems), output.finalMessage, output.children, got)
	}
}

func TestJSONLOutputForeignItemCannotChangeNativeResultOrHelpers(t *testing.T) {
	for _, withStatus := range []bool{false, true} {
		t.Run(fmt.Sprintf("status=%t", withStatus), func(t *testing.T) {
			var got []aicli.StreamEvent
			output := jsonlOutput{}
			if withStatus {
				output.onEvent = func(event aicli.StreamEvent) { got = append(got, event) }
			}
			writeCodexStreamEvent(t, &output, map[string]any{"type": "thread.started", "thread_id": "root"})
			writeCodexStreamEvent(t, &output, map[string]any{"type": "item.completed", "item": map[string]any{"id": "good", "type": "agent_message", "text": "trusted binding"}})
			writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "thread_id": "foreign", "item": map[string]any{"id": "bad-helper", "type": "collab_tool_call", "tool": "spawn_agent", "receiver_thread_ids": []string{"foreign-child"}}})
			writeCodexStreamEvent(t, &output, map[string]any{"type": "item.completed", "thread_id": "foreign", "item": map[string]any{"id": "bad", "type": "agent_message", "text": "foreign answer"}})
			writeCodexStreamEvent(t, &output, map[string]any{"type": "turn.completed"})
			if output.finalMessage != "trusted binding" || len(output.children) != 0 || !output.completed {
				t.Fatalf("foreign item changed result or helper accounting: final=%q children=%v complete=%t", output.finalMessage, output.children, output.completed)
			}
			if withStatus && !slices.Equal(got, []aicli.StreamEvent{
				{NativeID: "root", Kind: "identity"}, {NativeID: "root", Kind: "assistant", Text: "trusted binding"}, {NativeID: "root", Kind: "content_missing"},
			}) {
				t.Fatalf("foreign item did not mark missing coverage: %+v", got)
			}
		})
	}
}

func TestJSONLOutputSkipsProgressMapsWithoutStatusSink(t *testing.T) {
	var output jsonlOutput
	writeCodexStreamEvent(t, &output, map[string]any{"type": "thread.started", "thread_id": "root"})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "item": map[string]any{"id": "one", "type": "agent_message", "text": "pending"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.updated", "item": map[string]any{"id": "one", "type": "agent_message", "text": "updated"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.completed", "item": map[string]any{"id": "one", "type": "agent_message", "text": "done"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "turn.completed"})
	output.flushPending()
	if output.pending != nil || output.completedItems != nil || output.finalMessage != "done" || !output.completed {
		t.Fatalf("optional maps changed native result: pending=%v completed=%v final=%q turn=%t", output.pending, output.completedItems, output.finalMessage, output.completed)
	}
}

func TestJSONLOutputOpenReasoningMarksGapWithoutHiddenText(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }}
	writeCodexStreamEvent(t, &output, map[string]any{"type": "thread.started", "thread_id": "root"})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "item": map[string]any{"id": "reason", "type": "reasoning", "content": "hidden reasoning"}})
	output.stream.Finish()
	output.flushPending()
	if !slices.Equal(got, []aicli.StreamEvent{{NativeID: "root", Kind: "identity"}, {NativeID: "root", Kind: "content_missing"}}) {
		t.Fatalf("open reasoning lost gap or exposed hidden text: %+v", got)
	}
}

func TestJSONLOutputFlushesPendingInFirstSeenOrder(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }, allowIncomplete: true}
	writeCodexStreamEvent(t, &output, map[string]any{"type": "thread.started", "thread_id": "root"})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "item": map[string]any{"id": "z-first", "type": "agent_message", "text": "first"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "item": map[string]any{"id": "a-second", "type": "agent_message", "text": "second"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.updated", "item": map[string]any{"id": "z-first", "type": "agent_message", "text": "first updated"}})
	output.flushPending()
	if !slices.Equal(got, []aicli.StreamEvent{
		{NativeID: "root", Kind: "identity"},
		{NativeID: "root", Kind: "assistant_incomplete", Text: "first updated"},
		{NativeID: "root", Kind: "assistant_incomplete", Text: "second"},
	}) {
		t.Fatalf("pending order changed by ID sorting or updates: %+v", got)
	}
}

func TestJSONLOutputOpenToolUpdatesReplaceSnapshot(t *testing.T) {
	var got []aicli.StreamEvent
	output := jsonlOutput{onEvent: func(event aicli.StreamEvent) { got = append(got, event) }, allowIncomplete: true}
	writeCodexStreamEvent(t, &output, map[string]any{"type": "thread.started", "thread_id": "root"})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.started", "item": map[string]any{"id": "tool", "type": "command_execution", "aggregated_output": "old"}})
	writeCodexStreamEvent(t, &output, map[string]any{"type": "item.updated", "item": map[string]any{"id": "tool", "type": "command_execution", "aggregated_output": "new"}})
	output.flushPending()
	if len(got) != 2 || got[1].NativeID != "root" || got[1].Kind != "tool_incomplete" ||
		!json.Valid([]byte(got[1].Text)) || !strings.Contains(got[1].Text, `"aggregated_output":"new"`) || strings.Contains(got[1].Text, "old") {
		t.Fatalf("open tool snapshot was appended, dropped, or malformed: %+v", got)
	}
}

func TestSessionStreamOpenToolRequiresNoKnownCredential(t *testing.T) {
	for _, test := range []struct {
		name, key, wantKind string
	}{
		{name: "no credential", wantKind: "tool_incomplete"},
		{name: "configured credential", key: "native-secret", wantKind: "content_missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CODEX_API_KEY", "")
			t.Setenv("CODEX_ACCESS_TOKEN", "")
			installFakeCodex(t, `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.started","item":{"id":"tool","type":"command_execution","command":"echo old"}}' '{"type":"item.updated","item":{"id":"tool","type":"command_execution","command":"echo new native-secret"}}' '{"type":"turn.failed","message":"stopped"}'
exit 1
`)
			var session Session
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			var got []aicli.StreamEvent
			session.OnEvent = func(event aicli.StreamEvent) { got = append(got, event) }
			_, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{APIKey: test.key})
			if err == nil || len(got) != 2 || got[1].NativeID != "root" || got[1].Kind != test.wantKind ||
				test.key != "" && strings.Contains(got[1].Text, "native-secret") || test.key == "" && !strings.Contains(got[1].Text, `"command":"echo new`) ||
				test.key != "" && got[1].Text != "" {
				t.Fatalf("known-credential tool policy: error=%v events=%+v", err, got)
			}
		})
	}
}

func TestSessionStreamOpenAssistantRequiresNoKnownCredential(t *testing.T) {
	const key = "native-secret"
	message := "Useful work: " + strings.Repeat("A", 80) + " native-se " + strings.Repeat("B", 80) + " " + key
	item, err := json.Marshal(map[string]any{"type": "item.updated", "item": map[string]any{"id": "message", "type": "agent_message", "text": message}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, key, ambient, wantKind string
	}{
		{name: "no credential", wantKind: "assistant_incomplete"},
		{name: "configured credential", key: key, wantKind: "content_missing"},
		{name: "ambient native access token", ambient: key, wantKind: "content_missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CODEX_API_KEY", "")
			t.Setenv("CODEX_ACCESS_TOKEN", test.ambient)
			installFakeCodex(t, "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}' '"+string(item)+"' '{\"type\":\"turn.failed\",\"message\":\"stopped\"}'\nexit 1\n")
			var session Session
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			var got []aicli.StreamEvent
			session.OnEvent = func(event aicli.StreamEvent) { got = append(got, event) }
			result, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{APIKey: test.key})
			if err == nil || result.Answer != "" || len(got) != 2 ||
				got[0] != (aicli.StreamEvent{NativeID: "root", Kind: "identity"}) ||
				got[1].NativeID != "root" || got[1].Kind != test.wantKind ||
				test.wantKind == "assistant_incomplete" && got[1].Text != message ||
				test.wantKind == "content_missing" && got[1].Text != "" {
				t.Fatalf("native open-assistant capture: result=%+v error=%v events=%+v", result, err, got)
			}
		})
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
