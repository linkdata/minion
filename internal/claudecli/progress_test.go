package claudecli

import (
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestProgressFollowsFreshExecutionEvents(t *testing.T) {
	var got int
	o := jsonlOutput{sessionID: "native-id", onProgress: func() { got++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"system","subtype":"init","session_id":"native-id"}`, 0},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"message_start","message":{"id":"message-1"}}}`, 0},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"working"}}}`, 1},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"message_delta","delta":{"usage":{"output_tokens":1}}}}`, 1},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool-1"}}}`, 2},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool-1"}}}`, 2},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}}`, 3},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_stop","index":1}}`, 4},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_stop","index":1}}`, 4},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"message_stop"}}`, 4},
		{`{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"message-1","content":[{"type":"text","text":"working"},{"type":"tool_use","id":"tool-1"}]}}`, 4},
		{`{"type":"user","session_id":"native-id","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"done"}]}}`, 5},
		{`{"type":"user","session_id":"native-id","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"done"}]}}`, 5},
		{`{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"usage-only","usage":{"output_tokens":2}}}`, 5},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"message_start","message":{"id":"message-2"}}}`, 5},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning"}}}`, 6},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"metadata"}}}`, 6},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"message_stop"}}`, 6},
		{`{"type":"assistant","session_id":"native-id","parent_tool_use_id":"delegate","agent_id":"helper-1","message":{"id":"helper-message","content":[{"type":"text","text":"helper work"}]}}`, 7},
		{`{"type":"assistant","session_id":"native-id","parent_tool_use_id":"delegate","agent_id":"helper-1","message":{"id":"helper-message","content":[{"type":"text","text":"helper work"}]}}`, 7},
		{`{"type":"user","session_id":"native-id","parent_tool_use_id":"delegate","agent_id":"helper-1","message":{"content":[{"type":"tool_result","tool_use_id":"helper-tool","content":"output"}]}}`, 8},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"},{"task_id":"ambient","ambient":true}]}`, 9},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"},{"task_id":"ambient","ambient":true}]}`, 9},
		{`{"type":"system","subtype":"task_notification","task_id":"worker","status":"completed"}`, 10},
		{`{"type":"system","subtype":"task_notification","task_id":"worker","status":"completed"}`, 10},
		{`{"type":"system","subtype":"task_notification","task_id":"ambient","status":"completed","ambient":true}`, 10},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`, 11},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`, 11},
		{`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`, 11},
		{successfulResult, 11},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"message_start","message":{"id":"message-1"}}}`, 11},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"working"}}}`, 11},
		{`{"type":"assistant","session_id":"foreign","parent_tool_use_id":null,"message":{"id":"foreign","content":[{"type":"text","text":"foreign work"}]}}`, 11},
	} {
		writeChunks(t, &o, step.line+"\n")
		if o.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, o.err)
		}
	}
}

func TestProgressDeduplicatesResumedMessagesAndSyntheticTaskNotices(t *testing.T) {
	var got int
	first := jsonlOutput{sessionID: "native-id", onProgress: func() { got++ }}
	const assistant = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"message-1","content":[{"type":"text","text":"answer"}]}}`
	writeChunks(t, &first, assistant+"\n")
	if got != 1 {
		t.Fatalf("first message progress=%d, want 1", got)
	}
	resumed := jsonlOutput{sessionID: "native-id", onProgress: first.onProgress, progressSeen: first.progressSeen}
	writeChunks(t, &resumed, assistant+"\n")
	writeChunks(t, &resumed, `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>worker</task-id><status>completed</status></task-notification>"}}`+"\n")
	writeChunks(t, &resumed, `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>worker</task-id><status>completed</status></task-notification>"}}`+"\n")
	if resumed.err != nil || got != 2 {
		t.Fatalf("replay or repeated task notice renewed progress: got=%d error=%v", got, resumed.err)
	}
	const worker = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"}]}`
	const clear = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	writeChunks(t, &resumed, worker+"\n"+clear+"\n")
	replayed := jsonlOutput{sessionID: "native-id", onProgress: first.onProgress, progressSeen: resumed.progressSeen}
	writeChunks(t, &replayed, worker+"\n"+clear+"\n")
	if got != 4 || replayed.err != nil {
		t.Fatalf("replayed task list renewed progress: count=%d error=%v", got, replayed.err)
	}
	var session Session
	session.progressSeen = replayed.progressSeen
	session.NewConversation()
	if session.progressSeen != nil {
		t.Fatal("new conversation retained replay IDs")
	}
}

func TestProgressRequiresNativeContentIdentity(t *testing.T) {
	var got int
	o := jsonlOutput{sessionID: "native-id", onProgress: func() { got++ }}
	for _, line := range []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"unbound"}}}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"unbound"}}}`,
		`{"type":"assistant","message":{"id":"empty-tool","content":[{"type":"tool_use"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"no ID"}]}}`,
		`{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>worker</task-id><status>completed"}}`,
	} {
		writeChunks(t, &o, line+"\n")
	}
	if got != 0 || o.err != nil {
		t.Fatalf("unbound content renewed progress: count=%d error=%v", got, o.err)
	}
	writeChunks(t, &o, `{"type":"stream_event","agent_id":"helper-1","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"helper-message"}}}`+"\n")
	writeChunks(t, &o, `{"type":"stream_event","agent_id":"helper-1","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"helper work"}}}`+"\n")
	writeChunks(t, &o, `{"type":"assistant","agent_id":"helper-1","parent_tool_use_id":"delegate","message":{"id":"helper-message","content":[{"type":"text","text":"helper work"}]}}`+"\n")
	if got != 1 || o.err != nil {
		t.Fatalf("helper delta was missed or full message replayed: count=%d error=%v", got, o.err)
	}
}

func TestSessionRunDeliversNativeProgress(t *testing.T) {
	fakeExecutable(t, "claude", "cat >/dev/null\nprintf '%s\\n' "+
		`'{"type":"stream_event","event":{"type":"message_start","message":{"id":"message-1"}}}' `+
		`'{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"working"}}}' `+
		"'"+successfulResult+"'\n")
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	var progress int
	session.OnProgress = func() { progress++ }
	result, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if err != nil || result.Answer != "final answer" || progress != 1 {
		t.Fatalf("Run=%+v, %v; progress=%d, want 1", result, err, progress)
	}
}
