package claudecli

import (
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

func TestStreamEventsCaptureCompleteBlocks(t *testing.T) {
	const request = `{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"printf private-key"}}`
	const response = `{"type":"tool_result","tool_use_id":"tool-1","content":"stdout"}`
	var got []aicli.StreamEvent
	o := jsonlOutput{sessionID: "native-id", apiKey: "private-key", onEvent: func(e aicli.StreamEvent) { got = append(got, e) }}
	stream := strings.Join([]string{
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"early","content":[{"type":"text","text":"early"}]}}`,
		`{"type":"system","subtype":"init","session_id":"native-id"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}}`,
		`{"type":"assistant","parent_tool_use_id":"delegate","agent_id":"agent-7","session_id":"native-id","message":{"id":"helper","content":[{"type":"text","text":"helper text"},` + request + `]}}`,
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"message","content":[{"type":"thinking","thinking":"private thought"},{"type":"text","text":"hello private-key"},` + request + `,{"type":"redacted_thinking","data":"hidden"}]}}`,
		`{"type":"user","isSynthetic":true,"message":{"content":[` + response + `]}}`,
		`{"type":"user","parent_tool_use_id":"delegate","agent_id":"agent-7","session_id":"native-id","message":{"content":[` + response + `]}}`,
		`{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"text","text":"ordinary user text"},` + response + `]}}`,
		`{"type":"user","parent_tool_use_id":null,"message":{"content":"direct user text"}}`,
	}, "\n") + "\n"
	writeChunks(t, &o, stream)
	want := []aicli.StreamEvent{
		{Kind: "assistant"},
		{NativeID: "agent-7", Kind: "assistant", Text: "helper text"},
		{NativeID: "agent-7", Kind: "tool_request", Text: strings.Replace(request, "private-key", "[redacted]", 1)},
		{NativeID: "native-id", Kind: "assistant", Text: "hello [redacted]"},
		{NativeID: "native-id", Kind: "tool_request", Text: strings.Replace(request, "private-key", "[redacted]", 1)},
		{NativeID: "agent-7", Kind: "tool_result", Text: response},
		{NativeID: "native-id", Kind: "user", Text: "ordinary user text"},
		{NativeID: "native-id", Kind: "tool_result", Text: response},
		{NativeID: "native-id", Kind: "user", Text: "direct user text"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events=%+v, want %+v", got, want)
	}
}

func TestStreamEventsUnknownParentageMarksGapOnlyForContent(t *testing.T) {
	var got []aicli.StreamEvent
	o := jsonlOutput{sessionID: "native-id", identitySeen: true, onEvent: func(e aicli.StreamEvent) { got = append(got, e) }}
	writeChunks(t, &o, strings.Join([]string{
		`{"type":"assistant","message":{"id":"usage-only"}}`,
		`{"type":"assistant","message":{"id":"thinking-only","content":[{"type":"thinking","thinking":"hidden"}]}}`,
		`{"type":"assistant","message":{"id":"unknown","content":[{"type":"text","text":"unbound"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"delegate","message":{"id":"unknown-helper","content":[{"type":"text","text":"unbound helper"}]}}`,
	}, "\n")+"\n")
	if !slices.Equal(got, []aicli.StreamEvent{{Kind: "assistant"}, {Kind: "assistant"}}) {
		t.Fatalf("unknown parentage was attributed to root or content-free events made gaps: %+v", got)
	}
}

func TestStreamEventMarksDroppedRedactionIncomplete(t *testing.T) {
	var printable strings.Builder
	for marker := byte('!'); marker <= '~'; marker++ {
		printable.WriteByte(marker)
	}
	key := "]" + printable.String()
	var got []aicli.StreamEvent
	o := jsonlOutput{sessionID: "native-id", identitySeen: true, apiKey: key, onEvent: func(e aicli.StreamEvent) { got = append(got, e) }}
	o.emitEvent("assistant", "]"+key+printable.String())
	if !slices.Equal(got, []aicli.StreamEvent{{Kind: "assistant"}}) {
		t.Fatalf("dropped redaction retained a complete event: %+v", got)
	}
}

func TestSessionStreamFinalRequiresSuccessfulLifecycle(t *testing.T) {
	const pending = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"}]}`
	const clear = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	for _, tc := range []struct {
		name, stream string
		wantFinal    bool
	}{
		{"final after provisional result", numberResultEvents(pending + "\n" + successfulResult + "\n" + clear + "\n" + successfulResult), true},
		{"malformed event after result", successfulResult + "\n{", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'EVENTS'\n"+tc.stream+"\nEVENTS\n")
			var got []aicli.StreamEvent
			var session Session
			session.OnEvent = func(e aicli.StreamEvent) { got = append(got, e) }
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			result, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
			if (err == nil) != tc.wantFinal || (result.Answer == "final answer") != tc.wantFinal {
				t.Fatalf("Run=%+v, %v", result, err)
			}
			want := []aicli.StreamEvent(nil)
			if tc.wantFinal {
				want = []aicli.StreamEvent{{NativeID: session.id, Kind: "final", Text: "final answer"}}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("events=%+v, want %+v", got, want)
			}
		})
	}
}
