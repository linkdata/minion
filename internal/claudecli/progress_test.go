package claudecli

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestProgressHistoryIsBoundedAcrossResumedCalls(t *testing.T) {
	progress := 0
	const started = `{"type":"system","subtype":"task_started","session_id":"native-id","task_id":"helper","run_id":"run"}`
	const usage = `{"type":"system","subtype":"task_progress","session_id":"native-id","task_id":"helper","run_id":"run","usage":{"total_tokens":1}}`
	first := jsonlOutput{sessionID: "native-id", onProgress: func() { progress++ }, progressEntryLimit: 4}
	writeChunks(t, &first, started+"\n"+usage+"\n")
	if first.err != nil || progress != 2 || first.progressEntries != 2 {
		t.Fatalf("initial progress: count=%d entries=%d err=%v", progress, first.progressEntries, first.err)
	}
	resumed := jsonlOutput{sessionID: "native-id", onProgress: first.onProgress,
		progressSeen: first.progressSeen, taskProgress: first.taskProgress,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries, progressEntryLimit: 4}
	writeChunks(t, &resumed, started+"\n"+usage+"\n"+
		strings.ReplaceAll(started, `"run_id":"run"`, `"run_id":"run-2"`)+"\n"+
		strings.ReplaceAll(usage, `"run_id":"run"`, `"run_id":"run-2"`)+"\n")
	if resumed.err != nil || progress != 4 || resumed.progressEntries != 4 {
		t.Fatalf("resumed progress: count=%d entries=%d err=%v", progress, resumed.progressEntries, resumed.err)
	}
	writeChunks(t, &resumed, strings.ReplaceAll(started, `"run_id":"run"`, `"run_id":"run-3"`)+"\n")
	if !errors.Is(resumed.err, aicli.ErrOutputLimit) || progress != 4 || resumed.progressEntries != 4 {
		t.Fatalf("entry limit: count=%d entries=%d err=%v", progress, resumed.progressEntries, resumed.err)
	}

	progress = 0
	limited := jsonlOutput{onProgress: func() { progress++ }, progressByteLimit: 20}
	writeChunks(t, &limited, `{"type":"system","subtype":"task_started","task_id":"h","run_id":"r"}`+"\n")
	writeChunks(t, &limited, `{"type":"system","subtype":"task_progress","task_id":"h","run_id":"r","usage":{"total_tokens":1}}`+"\n")
	if limited.err != nil || limited.progressBytes != 17 || progress != 2 {
		t.Fatalf("initial byte budget: bytes=%d count=%d err=%v", limited.progressBytes, progress, limited.err)
	}
	resumedBytes := jsonlOutput{onProgress: limited.onProgress,
		progressSeen: limited.progressSeen, taskProgress: limited.taskProgress,
		progressBytes: limited.progressBytes, progressEntries: limited.progressEntries, progressByteLimit: 20}
	writeChunks(t, &resumedBytes, `{"type":"system","subtype":"task_progress","task_id":"h","run_id":"r","usage":{"total_tokens":1}}`+"\n")
	writeChunks(t, &resumedBytes, `{"type":"system","subtype":"task_progress","task_id":"longerid","run_id":"r","usage":{"total_tokens":1}}`+"\n")
	if !errors.Is(resumedBytes.err, aicli.ErrOutputLimit) || progress != 2 || resumedBytes.progressBytes != 17 {
		t.Fatalf("resumed byte limit: count=%d bytes=%d err=%v", progress, resumedBytes.progressBytes, resumedBytes.err)
	}
	session := Session{progressSeen: resumed.progressSeen, taskProgress: resumed.taskProgress,
		progressBytes: resumed.progressBytes, progressEntries: resumed.progressEntries}
	session.NewConversation()
	if session.progressSeen != nil || session.taskProgress != nil || session.progressBytes != 0 || session.progressEntries != 0 {
		t.Fatal("new conversation retained progress history")
	}
}

func TestStreamReplayProofIsBoundedWithoutProgressCallback(t *testing.T) {
	o := jsonlOutput{progressEntryLimit: 1}
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"old"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"work"}}}`
	writeChunks(t, &o, start+"\n"+delta+"\n")
	if !errors.Is(o.err, aicli.ErrOutputLimit) || o.progressEntries != 1 || o.progressSeen["assistant:old"] {
		t.Fatalf("unbounded stream proof: entries=%d proof=%t error=%v", o.progressEntries, o.progressSeen["assistant:old"], o.err)
	}
}

func TestTaggedRunStateAcrossResume(t *testing.T) {
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const usage = `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-2","usage":{"total_tokens":1}}`
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-3"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	const finishSecond = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`
	const notice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	progress := 0
	first := jsonlOutput{onProgress: func() { progress++ }}
	writeChunks(t, &first, started+"\n"+usage+"\n"+listed+"\n"+empty+"\n"+finishSecond+"\n")
	if first.err != nil || progress != 3 || first.pending != 2 || len(first.taskRuns) != 2 {
		t.Fatalf("tagged open runs: progress=%d pending=%d runs=%v error=%v", progress, first.pending, first.taskRuns, first.err)
	}
	resumed := jsonlOutput{onProgress: first.onProgress, progressSeen: first.progressSeen,
		taskProgress: first.taskProgress, progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	writeChunks(t, &resumed, notice+"\n"+empty+"\n"+started+"\n"+usage+"\n")
	if resumed.err != nil || progress != 3 || resumed.pending != 2 || len(resumed.taskRuns) != 2 {
		t.Fatalf("runless replay cleared tagged work: progress=%d pending=%d runs=%v error=%v", progress, resumed.pending, resumed.taskRuns, resumed.err)
	}
	for _, runID := range []string{"run-1", "run-3"} {
		writeChunks(t, &resumed, fmt.Sprintf(`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":%q,"status":"completed"}`, runID)+"\n")
	}
	if resumed.err != nil || progress != 5 || resumed.pending != 0 {
		t.Fatalf("tagged finishes: progress=%d pending=%d error=%v", progress, resumed.pending, resumed.err)
	}
	session := Session{progressSeen: resumed.progressSeen, taskProgress: resumed.taskProgress}
	session.NewConversation()
	if session.progressSeen != nil || session.taskProgress != nil {
		t.Fatal("new conversation retained task run history")
	}
}

func TestTaskStateIgnoresForeignSession(t *testing.T) {
	const init = `{"type":"system","subtype":"init","session_id":"native-id"}`
	const notice = `<task-notification><task-id>worker</task-id><status>stopped</status></task-notification>`
	for _, tc := range []struct {
		name, foreign string
	}{
		{"background task list", `{"type":"system","subtype":"background_tasks_changed","session_id":"foreign","tasks":[{"task_id":"worker"}]}`},
		{"native task notification", `{"type":"system","subtype":"task_notification","session_id":"foreign","task_id":"worker","status":"stopped"}`},
		{"synthetic string notice", `{"type":"user","isSynthetic":true,"session_id":"foreign","message":{"content":"` + notice + `"}}`},
		{"synthetic text block notice", `{"type":"user","isSynthetic":true,"session_id":"foreign","message":{"content":[{"type":"text","text":"` + notice + `"}]}}`},
	} {
		for _, session := range []struct {
			name, id string
			accept   bool
		}{
			{"foreign", "foreign", false},
			{"current", "native-id", true},
			{"legacy missing", "", true},
		} {
			t.Run(tc.name+"/"+session.name, func(t *testing.T) {
				var progress int
				o := jsonlOutput{sessionID: "native-id", onProgress: func() { progress++ }}
				event := strings.Replace(tc.foreign, `,"session_id":"foreign"`, "", 1)
				if session.id != "" {
					event = strings.Replace(tc.foreign, `"session_id":"foreign"`, `"session_id":"`+session.id+`"`, 1)
				}
				writeChunks(t, &o, init+"\n"+successfulResult+"\n"+event+"\n")
				o.finish()
				if session.accept {
					if o.err == nil || progress != 0 || o.completed || o.result.Answer != "" {
						t.Fatalf("task event was ignored: result=%+v error=%v progress=%d", o.result, o.err, progress)
					}
				} else if o.err != nil || progress != 0 || o.pending != 0 || o.stopped || !o.completed || o.result.Answer != "final answer" || !o.result.UsageComplete {
					t.Fatalf("foreign task event changed root result: result=%+v error=%v progress=%d pending=%d stopped=%v", o.result, o.err, progress, o.pending, o.stopped)
				}
			})
		}
	}
}

func TestForeignTaskListCannotClearCurrentPendingTasks(t *testing.T) {
	const init = `{"type":"system","subtype":"init","session_id":"native-id"}`
	const pending = `{"type":"system","subtype":"background_tasks_changed","session_id":"native-id","tasks":[{"task_id":"worker"}]}`
	const foreignClear = `{"type":"system","subtype":"background_tasks_changed","session_id":"foreign","tasks":[]}`
	o := jsonlOutput{sessionID: "native-id"}
	writeChunks(t, &o, init+"\n"+pending+"\n"+foreignClear+"\n"+successfulResult+"\n")
	o.finish()
	if o.pending != 1 || o.err == nil || o.completed || o.result.Answer != "" {
		t.Fatalf("foreign task list cleared pending work: pending=%d result=%+v error=%v", o.pending, o.result, o.err)
	}
}

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
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"},{"task_id":"ambient","ambient":true}]}`, 8},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"},{"task_id":"ambient","ambient":true}]}`, 8},
		{`{"type":"system","subtype":"task_notification","task_id":"worker","status":"completed"}`, 8},
		{`{"type":"system","subtype":"task_notification","task_id":"worker","status":"completed"}`, 8},
		{`{"type":"system","subtype":"task_notification","task_id":"ambient","status":"completed","ambient":true}`, 8},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`, 8},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`, 8},
		{`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`, 8},
		{successfulResult, 8},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"message_start","message":{"id":"message-1"}}}`, 8},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"working"}}}`, 8},
		{`{"type":"assistant","session_id":"foreign","parent_tool_use_id":null,"message":{"id":"foreign","content":[{"type":"text","text":"foreign work"}]}}`, 8},
	} {
		writeChunks(t, &o, step.line+"\n")
		if o.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, o.err)
		}
	}
}

func TestUUIDBearingPartialReplayDoesNotRenewProgress(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
	}{
		{"text", `"type":"text_delta","text":"first"`, `"type":"text_delta","text":"second"`},
		{"thinking", `"type":"thinking_delta","thinking":"first"`, `"type":"thinking_delta","thinking":"second"`},
		{"tool input", `"type":"input_json_delta","partial_json":"first"`, `"type":"input_json_delta","partial_json":"second"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			progress := 0
			o := jsonlOutput{sessionID: "native-id", onProgress: func() { progress++ }}
			const start = `{"type":"stream_event","session_id":"native-id","event":{"type":"message_start","message":{"id":"message-1"}}}`
			frame := func(uuid, delta string) string {
				return `{"type":"stream_event","session_id":"native-id",` + uuid + `"event":{"type":"content_block_delta","index":0,"delta":{` + delta + `}}}`
			}
			first := frame(`"uuid":"delta-1",`, tc.first)
			second := frame(`"uuid":"delta-1",`, tc.second)
			writeChunks(t, &o, start+"\n"+first+"\n"+first+"\n")
			if o.err != nil || progress != 1 {
				t.Fatalf("exact UUID replay: progress=%d error=%v", progress, o.err)
			}
			writeChunks(t, &o, second+"\n"+second+"\n")
			if o.err != nil || progress != 2 {
				t.Fatalf("distinct content sharing UUID: progress=%d error=%v", progress, o.err)
			}
			unidentified := frame("", tc.first)
			writeChunks(t, &o, unidentified+"\n"+unidentified+"\n")
			if o.err != nil || progress != 4 {
				t.Fatalf("unidentified deltas are ambiguous: progress=%d error=%v", progress, o.err)
			}
		})
	}
}

func TestProgressFromPrivateAssistantBlocks(t *testing.T) {
	var progress int
	var events []aicli.StreamEvent
	first := jsonlOutput{sessionID: "native-id", onProgress: func() { progress++ },
		onEvent: func(e aicli.StreamEvent) { events = append(events, e) }}
	const thinking = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"thinking","content":[{"type":"thinking","thinking":"private reasoning","signature":"signed"}]}}`
	const redacted = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"redacted","content":[{"type":"redacted_thinking","data":"opaque"}]}}`
	const omitted = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"omitted","content":[{"type":"thinking","thinking":"","signature":"signed"}]}}`
	for i, step := range []struct {
		line string
		want int
	}{
		{thinking, 1},
		{thinking, 1},
		{redacted, 2},
		{redacted, 2},
		{omitted, 2},
		{`{"type":"assistant","session_id":"native-id","message":{"id":"empty-thinking","content":[{"type":"thinking","thinking":""}]}}`, 2},
		{`{"type":"assistant","session_id":"native-id","message":{"id":"empty-redacted","content":[{"type":"redacted_thinking","data":""}]}}`, 2},
		{`{"type":"assistant","session_id":"foreign","message":{"id":"foreign","content":[{"type":"thinking","thinking":"private"}]}}`, 2},
	} {
		writeChunks(t, &first, step.line+"\n")
		if first.err != nil || progress != step.want || len(events) != 0 {
			t.Fatalf("step %d: progress=%d, want %d; events=%+v error=%v", i, progress, step.want, events, first.err)
		}
	}
	resumed := jsonlOutput{sessionID: "native-id", onProgress: first.onProgress, onEvent: first.onEvent,
		progressSeen: first.progressSeen, progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	writeChunks(t, &resumed, thinking+"\n"+redacted+"\n"+omitted+"\n")
	if resumed.err != nil || progress != 2 || len(events) != 0 {
		t.Fatalf("resumed private blocks: progress=%d events=%+v error=%v", progress, events, resumed.err)
	}
}

func TestThinkingSignatureReplayDoesNotRenewProgressOrRevokeResult(t *testing.T) {
	var progress int
	o := jsonlOutput{onProgress: func() { progress++ }}
	frame := func(thinking, signature string) string {
		return `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"thought","content":[{"type":"thinking","thinking":` +
			fmt.Sprintf("%q", thinking) + `,"signature":` + fmt.Sprintf("%q", signature) + `}]}}` + "\n"
	}
	writeChunks(t, &o, frame("reasoning", "first")+frame("reasoning", "second"))
	if o.err != nil || progress != 1 {
		t.Fatalf("signature-only pre-result replay: progress=%d error=%v", progress, o.err)
	}
	writeChunks(t, &o, successfulResult+"\n"+frame("reasoning", "third"))
	if o.err != nil || !o.completed || o.result.Answer != "final answer" || progress != 1 {
		t.Fatalf("signature-only post-result replay: progress=%d completed=%t answer=%q error=%v", progress, o.completed, o.result.Answer, o.err)
	}
	writeChunks(t, &o, frame("new reasoning", "third"))
	if o.err != nil || o.completed || o.result.Answer != "" || progress != 2 {
		t.Fatalf("changed thinking content: progress=%d completed=%t answer=%q error=%v", progress, o.completed, o.result.Answer, o.err)
	}
}

func TestMalformedAssistantBlockChangesDoNotRevokeResult(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, after string
	}{
		{"tool without ID", `[{"type":"tool_use","name":"Bash","input":{"command":"pwd"}}]`,
			`[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]`,
			`[{"type":"tool_use","name":"Bash","input":{"command":"date"}}]`},
		{"numeric text", `[{"type":"text","text":1}]`,
			`[{"type":"text","text":2}]`, `[{"type":"text","text":3}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			progress := 0
			o := jsonlOutput{onProgress: func() { progress++ }}
			frame := func(content string) string {
				return `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"bad","content":` + content + `}}` + "\n"
			}
			writeChunks(t, &o, frame(tc.first)+frame(tc.second)+successfulResult+"\n"+frame(tc.after))
			if o.err != nil || !o.completed || o.result.Answer != "final answer" || progress != 0 {
				t.Fatalf("malformed block changed result/progress: progress=%d completed=%t answer=%q error=%v",
					progress, o.completed, o.result.Answer, o.err)
			}
		})
	}
}

func TestProgressIgnoresAmbientFrames(t *testing.T) {
	var got int
	o := jsonlOutput{sessionID: "native-id", onProgress: func() { got++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"assistant","session_id":"native-id","ambient":true,"message":{"id":"ambient-assistant","content":[{"type":"text","text":"cleanup"}]}}`, 0},
		{`{"type":"user","session_id":"native-id","ambient":true,"message":{"content":[{"type":"tool_result","tool_use_id":"ambient-tool","content":"done"}]}}`, 0},
		{`{"type":"stream_event","session_id":"native-id","ambient":true,"event":{"type":"message_start","message":{"id":"ambient-stream"}}}`, 0},
		{`{"type":"stream_event","session_id":"native-id","ambient":true,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cleanup"}}}`, 0},
		{`{"type":"stream_event","session_id":"native-id","ambient":true,"event":{"type":"message_stop"}}`, 0},
		{`{"type":"system","subtype":"background_tasks_changed","session_id":"native-id","ambient":true,"tasks":[{"task_id":"ambient-list"}]}`, 0},
		{`{"type":"assistant","session_id":"native-id","message":{"id":"ambient-assistant","content":[{"type":"text","text":"current work"}]}}`, 1},
		{`{"type":"user","session_id":"native-id","message":{"content":[{"type":"tool_result","tool_use_id":"ambient-tool","content":"current result"}]}}`, 2},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"message_start","message":{"id":"ambient-stream"}}}`, 2},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"current work"}}}`, 3},
		{`{"type":"system","subtype":"background_tasks_changed","session_id":"native-id","tasks":[{"task_id":"ambient-list"}]}`, 3},
	} {
		writeChunks(t, &o, step.line+"\n")
		if o.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, o.err)
		}
	}
}

func TestAmbientStreamStartDoesNotReopenCompletedResult(t *testing.T) {
	o := jsonlOutput{sessionID: "native-id", completed: true, results: 1}
	o.result.Answer = "done"
	const ambient = `{"type":"stream_event","session_id":"native-id","ambient":true,"event":{"type":"message_start","message":{"id":"cleanup"}}}`
	writeChunks(t, &o, ambient+"\n")
	if o.err != nil || !o.completed || o.result.Answer != "done" {
		t.Fatalf("ambient stream reopened result: completed=%t answer=%q error=%v", o.completed, o.result.Answer, o.err)
	}
	const current = `{"type":"stream_event","session_id":"native-id","event":{"type":"message_start","message":{"id":"next-turn"}}}`
	writeChunks(t, &o, current+"\n")
	if o.err != nil || o.completed || o.result.Answer != "" {
		t.Fatalf("current stream did not reopen result: completed=%t answer=%q error=%v", o.completed, o.result.Answer, o.err)
	}
}

func TestProgressThinkingTokensFrames(t *testing.T) {
	var got int
	first := jsonlOutput{sessionID: "native-id", onProgress: func() { got++ }}
	const firstFrame = `{"type":"system","subtype":"thinking_tokens","session_id":"native-id","parent_tool_use_id":null,"uuid":"frame-1","estimated_tokens":8,"estimated_tokens_delta":8}`
	const secondFrame = `{"type":"system","subtype":"thinking_tokens","session_id":"native-id","parent_tool_use_id":null,"uuid":"frame-2","estimated_tokens":12,"estimated_tokens_delta":4}`
	for i, step := range []struct {
		line string
		want int
	}{
		{firstFrame, 1},
		{firstFrame, 1},
		{secondFrame, 2},
		{`{"type":"system","subtype":"thinking_tokens","session_id":"native-id","parent_tool_use_id":null,"uuid":"zero","estimated_tokens":12,"estimated_tokens_delta":0}`, 2},
		{`{"type":"system","subtype":"thinking_tokens","session_id":"native-id","uuid":"negative","estimated_tokens_delta":-1}`, 2},
		{`{"type":"system","subtype":"thinking_tokens","session_id":"native-id","estimated_tokens_delta":3}`, 2},
		{`{"type":"system","subtype":"thinking_tokens","session_id":"native-id","uuid":"no-delta","estimated_tokens":15,"duration_ms":500}`, 2},
		{`{"type":"system","subtype":"thinking_tokens","session_id":"foreign","uuid":"foreign","estimated_tokens_delta":3}`, 2},
		{`{"type":"system","subtype":"thinking_tokens","uuid":"unbound","estimated_tokens_delta":3}`, 2},
	} {
		writeChunks(t, &first, step.line+"\n")
		if first.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, first.err)
		}
	}

	resumed := jsonlOutput{sessionID: "native-id", onProgress: first.onProgress, progressSeen: first.progressSeen}
	writeChunks(t, &resumed, firstFrame+"\n"+secondFrame+"\n")
	writeChunks(t, &resumed, `{"type":"system","subtype":"thinking_tokens","session_id":"native-id","parent_tool_use_id":null,"uuid":"frame-3","estimated_tokens_delta":1}`+"\n")
	if resumed.err != nil || got != 3 {
		t.Fatalf("resumed thinking progress=%d, want 3; parser error=%v", got, resumed.err)
	}
}

func TestProgressThinkingTokensIgnoresTaggedHelpers(t *testing.T) {
	var got int
	o := jsonlOutput{sessionID: "native-id", onProgress: func() { got++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"system","subtype":"thinking_tokens","session_id":"native-id","uuid":"helper-agent","agent_id":"helper","estimated_tokens_delta":1}`, 0},
		{`{"type":"system","subtype":"thinking_tokens","session_id":"native-id","uuid":"helper-parent","parent_tool_use_id":"tool-1","estimated_tokens_delta":1}`, 0},
		{`{"type":"system","subtype":"thinking_tokens","session_id":"native-id","uuid":"unknown-parent","estimated_tokens_delta":1}`, 0},
		{`{"type":"system","subtype":"thinking_tokens","session_id":"native-id","uuid":"root","parent_tool_use_id":null,"estimated_tokens_delta":1}`, 1},
	} {
		writeChunks(t, &o, step.line+"\n")
		if o.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, o.err)
		}
	}
}

func TestSharedStopUUIDKeepsDistinctRunlessTaskPending(t *testing.T) {
	const runlessStart = `{"type":"system","subtype":"task_started","task_id":"helper"}`
	const runlessList = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const taggedStart = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const synthetic = `{"type":"user","isSynthetic":true,"uuid":"stop-1","message":{"content":"<task-notification><task-id>helper</task-id><status>stopped</status><summary>helper stopped</summary></task-notification>"}}`
	const native = `{"type":"system","subtype":"task_notification","uuid":"stop-1","task_id":"helper","run_id":"run-1","status":"stopped","summary":"helper stopped"}`
	const ownFinish = `{"type":"system","subtype":"task_notification","uuid":"finish-runless","task_id":"helper","status":"completed"}`
	const fresh = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"fresh"}}`
	for _, tc := range []struct{ name, first, second string }{
		{"start before tagged", runlessStart, taggedStart},
		{"tagged before start", taggedStart, runlessStart},
		{"list before tagged", runlessList, taggedStart},
		{"tagged before list", taggedStart, runlessList},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.first+"\n"+tc.second+"\n"+synthetic+"\n"+native+"\n")
			if o.err != nil || o.pending != 1 || o.runlessPending["helper"] != 1 || len(o.taskRuns) != 0 {
				t.Fatalf("shared stop cleared distinct task: pending=%d runless=%v runs=%v error=%v", o.pending, o.runlessPending, o.taskRuns, o.err)
			}
			writeChunks(t, &o, ownFinish+"\n"+fresh+"\n"+successfulResult+"\n")
			o.finish()
			if o.err != nil || !o.completed || o.pending != 0 || o.result.Answer != "final answer" {
				t.Fatalf("independent finish not accepted: pending=%d completed=%t answer=%q error=%v", o.pending, o.completed, o.result.Answer, o.err)
			}
		})
	}
}

func TestSharedStopUUIDKeepsRunlessTaskPendingAfterResume(t *testing.T) {
	const summary = `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","summary":"working"}`
	const taggedStart = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const runlessStart = `{"type":"system","subtype":"task_started","task_id":"helper"}`
	const synthetic = `{"type":"user","isSynthetic":true,"uuid":"stop-1","message":{"content":"<task-notification><task-id>helper</task-id><status>stopped</status><summary>helper stopped</summary></task-notification>"}}`
	const native = `{"type":"system","subtype":"task_notification","uuid":"stop-1","task_id":"helper","run_id":"run-1","status":"stopped","summary":"helper stopped"}`
	first := jsonlOutput{}
	writeChunks(t, &first, summary+"\n"+taggedStart+"\n"+taggedStart+"\n")
	if first.err != nil || first.taskRunCounts["helper"] != 1 {
		t.Fatalf("repeated start counted twice: counts=%v error=%v", first.taskRunCounts, first.err)
	}
	resumed := jsonlOutput{progressSeen: first.progressSeen, progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	writeChunks(t, &resumed, runlessStart+"\n"+synthetic+"\n"+native+"\n")
	if resumed.err != nil || resumed.pending != 1 || resumed.runlessPending["helper"] != 1 || resumed.taskRunCounts["helper"] != 0 {
		t.Fatalf("resumed shared stop cleared distinct task: pending=%d runless=%v counts=%v error=%v", resumed.pending, resumed.runlessPending, resumed.taskRunCounts, resumed.err)
	}
}

func TestSharedCompletionUUIDKeepsDistinctRunlessTaskPending(t *testing.T) {
	const runlessStart = `{"type":"system","subtype":"task_started","task_id":"helper"}`
	const runlessList = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const taggedStart = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	for _, status := range []string{"completed", "failed"} {
		for _, tc := range []struct{ name, first, second string }{
			{"start before tagged", runlessStart, taggedStart},
			{"tagged before start", taggedStart, runlessStart},
			{"list before tagged", runlessList, taggedStart},
			{"tagged before list", taggedStart, runlessList},
		} {
			t.Run(status+"/"+tc.name, func(t *testing.T) {
				synthetic := fmt.Sprintf(`{"type":"user","isSynthetic":true,"uuid":"finish-1","message":{"content":"<task-notification><task-id>helper</task-id><status>%s</status></task-notification>"}}`, status)
				native := fmt.Sprintf(`{"type":"system","subtype":"task_notification","uuid":"finish-1","task_id":"helper","run_id":"run-1","status":%q}`, status)
				o := jsonlOutput{}
				writeChunks(t, &o, tc.first+"\n"+tc.second+"\n"+synthetic+"\n"+native+"\n"+successfulResult+"\n")
				o.finish()
				if o.pending != 1 || o.runlessPending["helper"] != 1 || o.completed || o.result.Answer != "" || o.err == nil {
					t.Fatalf("shared %s cleared distinct task: pending=%d runless=%v completed=%t answer=%q error=%v", status, o.pending, o.runlessPending, o.completed, o.result.Answer, o.err)
				}
			})
		}
	}
}

func TestSharedTaskNoticeUUIDStillCreditsTaggedFinish(t *testing.T) {
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	for _, status := range []string{"completed", "failed", "stopped", "killed"} {
		for _, array := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/array=%t", status, array), func(t *testing.T) {
				progress := 0
				o := jsonlOutput{onProgress: func() { progress++ }}
				notice := fmt.Sprintf("<task-notification><task-id>helper</task-id><status>%s</status></task-notification>", status)
				content := fmt.Sprintf("%q", notice)
				if array {
					content = fmt.Sprintf(`[{"type":"text","text":%q}]`, notice)
				}
				synthetic := `{"type":"user","isSynthetic":true,"uuid":"notice-1","message":{"content":` + content + `}}`
				tagged := fmt.Sprintf(`{"type":"system","subtype":"task_notification","uuid":"notice-1","task_id":"helper","run_id":"run-1","status":%q}`, status)
				writeChunks(t, &o, started+"\n"+synthetic+"\n")
				if o.err != nil || progress != 1 || o.pending != 1 {
					t.Fatalf("synthetic notice: progress=%d pending=%d error=%v", progress, o.pending, o.err)
				}
				writeChunks(t, &o, tagged+"\n"+tagged+"\n")
				if o.err != nil || progress != 2 || o.pending != 0 {
					t.Fatalf("identified tagged finish: progress=%d pending=%d error=%v", progress, o.pending, o.err)
				}
			})
		}
	}
}

func TestAmbiguousSyntheticCompletionReplayKeepsRunlessTaskPending(t *testing.T) {
	const runlessStart = `{"type":"system","subtype":"task_started","task_id":"helper"}`
	const taggedStart = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	for _, status := range []string{"completed", "failed"} {
		for _, noticeUUID := range []string{`"uuid":"finish-1",`, ""} {
			t.Run(fmt.Sprintf("%s/identified=%t", status, noticeUUID != ""), func(t *testing.T) {
				synthetic := fmt.Sprintf(`{"type":"user","isSynthetic":true,%s"message":{"content":"<task-notification><task-id>helper</task-id><status>%s</status></task-notification>"}}`, noticeUUID, status)
				native := fmt.Sprintf(`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":%q}`, status)
				o := jsonlOutput{}
				writeChunks(t, &o, runlessStart+"\n"+taggedStart+"\n"+synthetic+"\n"+native+"\n"+synthetic+"\n"+successfulResult+"\n")
				o.finish()
				if o.pending != 1 || o.runlessPending["helper"] != 1 || o.completed || o.err == nil {
					t.Fatalf("replayed %s cleared distinct task: pending=%d runless=%v completed=%t error=%v", status, o.pending, o.runlessPending, o.completed, o.err)
				}
			})
		}
	}
}

func TestBareCompletionKeepsDistinctRunlessTaskPending(t *testing.T) {
	const runlessStart = `{"type":"system","subtype":"task_started","task_id":"helper"}`
	const runlessList = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const taggedStart = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	for _, status := range []string{"completed", "failed"} {
		for _, tc := range []struct{ name, first, second string }{
			{"start before tagged", runlessStart, taggedStart},
			{"tagged before start", taggedStart, runlessStart},
			{"list before tagged", runlessList, taggedStart},
			{"tagged before list", taggedStart, runlessList},
		} {
			t.Run(status+"/"+tc.name, func(t *testing.T) {
				bare := fmt.Sprintf(`{"type":"system","subtype":"task_notification","task_id":"helper","status":%q}`, status)
				tagged := fmt.Sprintf(`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":%q}`, status)
				o := jsonlOutput{}
				writeChunks(t, &o, tc.first+"\n"+tc.second+"\n"+bare+"\n"+tagged+"\n"+bare+"\n"+successfulResult+"\n")
				o.finish()
				if o.pending != 1 || o.runlessPending["helper"] != 1 || o.completed || o.result.Answer != "" || o.err == nil {
					t.Fatalf("bare %s cleared distinct task: pending=%d runless=%v completed=%t answer=%q error=%v", status, o.pending, o.runlessPending, o.completed, o.result.Answer, o.err)
				}
			})
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
	if resumed.err != nil || got != 1 {
		t.Fatalf("replay or repeated task notice renewed progress: got=%d error=%v", got, resumed.err)
	}
	const worker = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"}]}`
	const clear = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	writeChunks(t, &resumed, worker+"\n"+clear+"\n")
	replayed := jsonlOutput{sessionID: "native-id", onProgress: first.onProgress, progressSeen: resumed.progressSeen}
	writeChunks(t, &replayed, worker+"\n"+clear+"\n")
	if got != 1 || replayed.err != nil {
		t.Fatalf("replayed task list renewed progress: count=%d error=%v", got, replayed.err)
	}
	var session Session
	session.progressSeen = replayed.progressSeen
	session.NewConversation()
	if session.progressSeen != nil {
		t.Fatal("new conversation retained replay IDs")
	}
}

func TestFullAssistantContentVariantsRenewProgressWithoutStreamDelta(t *testing.T) {
	var got int
	o := jsonlOutput{onProgress: func() { got++ }}
	const first = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"A"}]}}`
	const next = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"AB"}]}}`
	for i, step := range []struct {
		line string
		want int
	}{{first, 1}, {next, 2}, {first, 2}, {next, 2}} {
		writeChunks(t, &o, step.line+"\n")
		if o.err != nil || got != step.want {
			t.Fatalf("variant %d: progress=%d want=%d error=%v", i, got, step.want, o.err)
		}
	}
}

func TestFullAssistantContentAfterStreamDeltaDoesNotDoubleCount(t *testing.T) {
	var got int
	o := jsonlOutput{onProgress: func() { got++ }}
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"A"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}}`
	const first = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"A"}]}}`
	const next = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"AB"}]}}`
	writeChunks(t, &o, start+"\n"+delta+"\n"+first+"\n")
	if o.err != nil || got != 1 {
		t.Fatalf("full message duplicated stream progress: got=%d error=%v", got, o.err)
	}
	writeChunks(t, &o, next+"\n"+first+"\n")
	if o.err != nil || got != 2 {
		t.Fatalf("fresh variant or replay credit wrong: got=%d error=%v", got, o.err)
	}
}

func TestEmptyOrReplayedFullFrameDoesNotRepeatStreamCredit(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"A"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}}`
	const empty = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[]}}`
	const first = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"A"}]}}`
	const next = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"AB"}]}}`
	for _, tc := range []struct {
		name, events, after string
		before, want        int
	}{
		{"empty frame", start + "\n" + delta + "\n" + empty + "\n" + first, next, 1, 2},
		{"replayed frame", start + "\n" + delta + "\n" + first + "\n" + delta + "\n" + delta + "\n" + first, next, 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := 0
			o := jsonlOutput{onProgress: func() { got++ }}
			writeChunks(t, &o, tc.events+"\n")
			if o.err != nil || got != tc.before {
				t.Fatalf("first full frame: progress=%d want=%d error=%v", got, tc.before, o.err)
			}
			writeChunks(t, &o, tc.after+"\n")
			if o.err != nil || got != tc.want {
				t.Fatalf("later unstreamed content lost: progress=%d want=%d error=%v", got, tc.want, o.err)
			}
		})
	}
}

func TestFullClaudeFrameSealsOnlyItsStreamBlock(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"A"}}}`
	const textStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}`
	const textDelta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}}`
	const ignoredIndex = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","index":99}}`
	const textFull = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"A"}]}}`
	const textStop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_stop","index":0}}`
	const toolStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use"}}}`
	const toolDelta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}}`
	const toolFull = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"tool_use","id":"tool","name":"Read","input":{"x":1}}]}}`
	const toolStop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_stop","index":1}}`
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{start, 0}, {textStart, 0}, {textDelta, 1}, {ignoredIndex, 1}, {textFull, 1},
		{textDelta, 1}, {textStop, 1}, {toolStart, 2}, {textFull, 2}, {toolDelta, 3},
		{textDelta, 3}, {toolFull, 3}, {toolDelta, 3}, {toolStop, 4}, {toolStop, 4},
	} {
		writeChunks(t, &o, step.line+"\n")
		if o.err != nil || progress != step.want {
			t.Fatalf("step %d: progress=%d want=%d error=%v", i, progress, step.want, o.err)
		}
	}
}

func TestFullClaudeFrameBeforeDeltaAndEmptyFrameStreamCredit(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"A"}}}`
	const textStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}`
	const textDelta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}}`
	const empty = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[]}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"A"}]}}`
	for _, tc := range []struct {
		name, events string
		want         int
	}{
		{"full before delta", start + "\n" + textStart + "\n" + full + "\n" + textDelta, 1},
		{"empty before delta", start + "\n" + textStart + "\n" + textDelta + "\n" + empty + "\n" + textDelta, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			progress := 0
			o := jsonlOutput{onProgress: func() { progress++ }}
			writeChunks(t, &o, tc.events+"\n")
			if o.err != nil || progress != tc.want {
				t.Fatalf("progress=%d want=%d error=%v", progress, tc.want, o.err)
			}
		})
	}
}

func TestFirstFullFrameAfterEmptyStreamReplayPreservesResult(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"A"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}}`
	const empty = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[]}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"A"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}}`
	const changed = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"AB"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}}`
	const resumedResult = `{"type":"result","subtype":"success","result":"final answer","modelUsage":{"claude-main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":11}}}`
	for _, callback := range []bool{false, true} {
		t.Run(fmt.Sprintf("callback=%t", callback), func(t *testing.T) {
			progress := 0
			first := jsonlOutput{}
			if callback {
				first.onProgress = func() { progress++ }
			}
			writeChunks(t, &first, start+"\n"+delta+"\n"+empty+"\n"+successfulResult+"\n")
			first.finish()
			if first.err != nil || !first.completed || callback && progress != 1 {
				t.Fatalf("first call: completed=%t progress=%d error=%v", first.completed, progress, first.err)
			}
			resumed := jsonlOutput{onProgress: first.onProgress, baseline: first.result.Tokens, progressSeen: first.progressSeen,
				assistantHistory: first.assistantHistory, progressBytes: first.progressBytes, progressEntries: first.progressEntries}
			writeChunks(t, &resumed, resumedResult+"\n"+full+"\n")
			if resumed.err != nil || !resumed.completed || resumed.result.Answer == "" ||
				!resumed.result.UsageComplete || resumed.result.Tokens != (usage.Tokens{}) || callback && progress != 1 {
				t.Fatalf("stream-covered replay: result=%+v completed=%t progress=%d error=%v", resumed.result, resumed.completed, progress, resumed.err)
			}
			writeChunks(t, &resumed, changed+"\n")
			if resumed.err != nil || resumed.completed || callback && progress != 2 {
				t.Fatalf("changed full frame did not count: completed=%t progress=%d error=%v", resumed.completed, progress, resumed.err)
			}
		})
	}
}

func TestFirstFullFrameAfterEmptyStreamKeepsUsageEvidence(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"A"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}}`
	const empty = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[]}}`
	for _, tc := range []struct {
		name, full   string
		wantComplete bool
	}{
		{"omitted usage", `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"A"}]}}`, true},
		{"malformed usage", `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"A"}],"usage":{}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			progress := 0
			o := jsonlOutput{onProgress: func() { progress++ }}
			writeChunks(t, &o, start+"\n"+delta+"\n"+empty+"\n"+successfulResult+"\n")
			if o.err != nil || !o.completed || !o.result.UsageComplete || progress != 1 {
				t.Fatalf("stream and result: result=%+v completed=%t progress=%d error=%v", o.result, o.completed, progress, o.err)
			}
			writeChunks(t, &o, tc.full+"\n")
			if o.err != nil || !o.completed || o.result.UsageComplete != tc.wantComplete || o.result.Answer != "final answer" || progress != 1 {
				t.Fatalf("full frame changed result: result=%+v completed=%t progress=%d error=%v", o.result, o.completed, progress, o.err)
			}
		})
	}
}

func TestReplayedStreamStartsDoNotRetainAgentState(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"A"}}}`
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	writeChunks(t, &o, start+"\n")
	for i := range 100 {
		writeChunks(t, &o, fmt.Sprintf(`{"type":"stream_event","agent_id":"agent-%d","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"A"}}}`+"\n", i))
	}
	if o.err != nil || len(o.progressStreams) != 1 || progress != 0 {
		t.Fatalf("replayed starts retained agent state: streams=%d progress=%d error=%v", len(o.progressStreams), progress, o.err)
	}
}

func TestProgressTracksForegroundTaskRuns(t *testing.T) {
	var got int
	o := jsonlOutput{sessionID: "native-id", onProgress: func() { got++ }}
	const started = `{"type":"system","subtype":"task_started","session_id":"native-id","task_id":"helper","run_id":"run-1","is_backgrounded":false}`
	const progress = `{"type":"system","subtype":"task_progress","session_id":"native-id","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5,"tool_uses":1},"summary":"read files","duration_ms":300}`
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"system","subtype":"task_started","task_id":"ambient","run_id":"run-1","ambient":true}`, 0},
		{`{"type":"system","subtype":"task_started","run_id":"run-1"}`, 0},
		{`{"type":"system","subtype":"task_started","session_id":"foreign","task_id":"helper","run_id":"run-1"}`, 0},
		{started, 1},
		{started, 1},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":0,"tool_uses":0},"duration_ms":100}`, 1},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":0,"tool_uses":0},"duration_ms":200}`, 1},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5,"tool_uses":0},"duration_ms":250}`, 2},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5,"tool_uses":0},"duration_ms":300}`, 2},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5,"tool_uses":1}}`, 3},
		{progress, 3},
		{progress, 3},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","summary":"write patch","duration_ms":400}`, 3},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":3,"tool_uses":0},"summary":"read files","duration_ms":500}`, 3},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":100},"ambient":true}`, 3},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`, 4},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`, 4},
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2","is_backgrounded":false}`, 5},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-2","usage":{"total_tokens":1}}`, 6},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`, 7},
	} {
		writeChunks(t, &o, step.line+"\n")
		if o.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, o.err)
		}
	}
	replayed := jsonlOutput{sessionID: "native-id", onProgress: o.onProgress, progressSeen: o.progressSeen, taskProgress: o.taskProgress}
	for _, line := range []string{started, progress, `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}`, `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-2","usage":{"total_tokens":1}}`} {
		writeChunks(t, &replayed, line+"\n")
	}
	if replayed.err != nil || got != 7 {
		t.Fatalf("replayed task run renewed progress: count=%d error=%v", got, replayed.err)
	}
	var session Session
	session.progressSeen, session.taskProgress = replayed.progressSeen, replayed.taskProgress
	session.NewConversation()
	if session.progressSeen != nil || session.taskProgress != nil {
		t.Fatal("new conversation retained task progress history")
	}
}

func TestProgressTaskSummariesDoNotRenewInactivity(t *testing.T) {
	var got int
	first := jsonlOutput{onProgress: func() { got++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`, 1},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","summary":"read files"}`, 1},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","summary":"write patch"}`, 1},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5,"tool_uses":1},"summary":"review patch"}`, 2},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5,"tool_uses":1},"summary":"test patch"}`, 2},
	} {
		writeChunks(t, &first, step.line+"\n")
		if first.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, first.err)
		}
	}
	resumed := jsonlOutput{onProgress: first.onProgress, progressSeen: first.progressSeen, taskProgress: first.taskProgress}
	writeChunks(t, &resumed, `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5,"tool_uses":1},"summary":"another patch"}`+"\n")
	if resumed.err != nil || got != 2 {
		t.Fatalf("resumed summary renewed progress: count=%d error=%v", got, resumed.err)
	}
}

func TestProgressRunningTaskNotificationDoesNotRenewInactivity(t *testing.T) {
	var got int
	o := jsonlOutput{onProgress: func() { got++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"running"}`, 0},
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`, 1},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"running"}`, 1},
		{`{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":1}}`, 2},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"running"}`, 2},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`, 3},
	} {
		writeChunks(t, &o, step.line+"\n")
		if o.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, o.err)
		}
	}
}

func TestRunLessTaskMetadataTracksPendingWithoutTimerCredit(t *testing.T) {
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const notice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	writeChunks(t, &o, listed+"\n")
	if o.err != nil || progress != 0 || o.pending != 1 {
		t.Fatalf("runless list: progress=%d pending=%d error=%v", progress, o.pending, o.err)
	}
	writeChunks(t, &o, notice+"\n"+successfulResult+"\n")
	if o.err != nil || progress != 0 || o.pending != 0 || !o.completed {
		t.Fatalf("runless completion: progress=%d pending=%d completed=%t error=%v", progress, o.pending, o.completed, o.err)
	}
}

func TestRunLessDistinctStartsRemainAmbiguousAfterIDOnlyCompletion(t *testing.T) {
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	const first = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`
	const second = `{"type":"system","subtype":"task_started","uuid":"start-2","task_id":"helper"}`
	const finish = `{"type":"system","subtype":"task_notification","uuid":"finish-1","task_id":"helper","status":"completed"}`
	writeChunks(t, &o, first+"\n"+second+"\n"+finish+"\n"+successfulResult+"\n")
	if o.err != nil || progress != 0 || o.pending == 0 || o.completed {
		t.Fatalf("distinct starts cleared by one ID-only finish: pending=%d completed=%t progress=%d error=%v", o.pending, o.completed, progress, o.err)
	}
}

func TestRunLessListBeforeStartCannotProveSameTask(t *testing.T) {
	o := jsonlOutput{}
	const listed = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-1","tasks":[{"task_id":"helper"}]}`
	const started = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`
	const finished = `{"type":"system","subtype":"task_notification","uuid":"finish-1","task_id":"helper","status":"completed"}`
	// A list and later start may describe one task or two tasks reusing the ID.
	// With no run identity, one finish cannot prove that both possibilities ended.
	writeChunks(t, &o, listed+"\n"+started+"\n"+finished+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending == 0 || o.completed {
		t.Fatalf("list-first work cleared by one ID-only finish: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
}

func TestRunLessStartUUIDReplayCanBeCleared(t *testing.T) {
	o := jsonlOutput{}
	const first = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`
	const finish = `{"type":"system","subtype":"task_notification","uuid":"finish-1","task_id":"helper","status":"completed"}`
	writeChunks(t, &o, first+"\n"+first+"\n"+finish+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending != 0 || !o.completed {
		t.Fatalf("replayed start became a second task: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
}

func TestRunLessStartWithoutUUIDRepeatedIsAmbiguous(t *testing.T) {
	o := jsonlOutput{}
	const start = `{"type":"system","subtype":"task_started","task_id":"helper"}`
	const finish = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	writeChunks(t, &o, start+"\n"+start+"\n"+finish+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending != 1 || o.completed {
		t.Fatalf("unidentified duplicate start cleared: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
}

func TestRunLessUncorrelatedSignalsStayAmbiguous(t *testing.T) {
	const listed = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-1","tasks":[{"task_id":"helper"}]}`
	const usage = `{"type":"system","subtype":"task_progress","task_id":"helper","usage":{"total_tokens":1}}`
	const started = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`
	const finished = `{"type":"system","subtype":"task_notification","uuid":"finish-1","task_id":"helper","status":"completed"}`
	for _, tc := range []struct{ name, first, second string }{
		{"list then usage", listed, usage},
		{"usage then start", usage, started},
		{"start then usage", started, usage},
		{"start then list", started, listed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.first+"\n"+tc.second+"\n"+finished+"\n"+successfulResult+"\n")
			if o.err != nil || o.pending == 0 || o.completed {
				t.Fatalf("overlapping ID-only evidence cleared by one notice: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
			}
		})
	}
}

func TestRunLessNoticeUUIDReplayCannotFinishLaterStart(t *testing.T) {
	o := jsonlOutput{}
	const first = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`
	const second = `{"type":"system","subtype":"task_started","uuid":"start-2","task_id":"helper"}`
	const finish = `{"type":"system","subtype":"task_notification","uuid":"finish-1","task_id":"helper","status":"completed"}`
	writeChunks(t, &o, first+"\n"+finish+"\n"+second+"\n"+finish+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending != 1 || o.completed {
		t.Fatalf("replayed finish cleared later start: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
}

func TestRunLessNewNoticeUUIDCannotFinishReusedTaskID(t *testing.T) {
	const first = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`
	const finished = `{"type":"system","subtype":"task_notification","uuid":"finish-1","task_id":"helper","status":"completed"}`
	const second = `{"type":"system","subtype":"task_started","uuid":"start-2","task_id":"helper"}`
	const delayed = `{"type":"system","subtype":"task_notification","uuid":"finish-2","task_id":"helper","status":"completed"}`
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%t", resume), func(t *testing.T) {
			o := &jsonlOutput{}
			initial := first + "\n" + finished + "\n"
			if resume {
				initial += successfulResult + "\n"
			}
			writeChunks(t, o, initial)
			if resume {
				o.finish()
				if o.err != nil || !o.completed {
					t.Fatalf("first call did not complete: completed=%t error=%v", o.completed, o.err)
				}
				o = &jsonlOutput{progressSeen: o.progressSeen, progressBytes: o.progressBytes, progressEntries: o.progressEntries}
			}
			writeChunks(t, o, second+"\n"+delayed+"\n"+successfulResult+"\n")
			o.finish()
			if o.pending != 1 || o.completed || o.err == nil {
				t.Fatalf("uncorrelated finish cleared reused task ID: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
			}
		})
	}
}

func TestRunLessNoticeWithoutUUIDReplayCannotFinishLaterStart(t *testing.T) {
	const first = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`
	const second = `{"type":"system","subtype":"task_started","uuid":"start-2","task_id":"helper"}`
	const finish = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%t", resume), func(t *testing.T) {
			o := &jsonlOutput{}
			writeChunks(t, o, first+"\n"+finish+"\n")
			if resume {
				o = &jsonlOutput{progressSeen: o.progressSeen, progressBytes: o.progressBytes, progressEntries: o.progressEntries}
			}
			writeChunks(t, o, second+"\n"+finish+"\n"+successfulResult+"\n")
			o.finish()
			if o.pending != 1 || o.completed || o.err == nil {
				t.Fatalf("replayed unidentified finish cleared later start: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
			}
		})
	}
}

func TestRunLessNoticeWithoutUUIDLeavesLaterFinishAmbiguous(t *testing.T) {
	o := jsonlOutput{}
	const first = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`
	const second = `{"type":"system","subtype":"task_started","uuid":"start-2","task_id":"helper"}`
	const bare = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	const identified = `{"type":"system","subtype":"task_notification","uuid":"finish-2","task_id":"helper","status":"completed"}`
	writeChunks(t, &o, first+"\n"+bare+"\n"+second+"\n"+bare+"\n"+identified+"\n"+successfulResult+"\n")
	o.finish()
	if o.pending != 1 || o.completed || o.err == nil {
		t.Fatalf("uncorrelated later finish cleared reused ID: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
}

func TestSyntheticNoticeArrayCompletesDistinctTasksWithSharedUUID(t *testing.T) {
	const first = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"one"}`
	const second = `{"type":"system","subtype":"task_started","uuid":"start-2","task_id":"two"}`
	const notices = `{"type":"user","isSynthetic":true,"uuid":"notices","message":{"content":[{"type":"text","text":"<task-notification><task-id>one</task-id><status>completed</status></task-notification>"},{"type":"text","text":"<task-notification><task-id>two</task-id><status>completed</status></task-notification>"}]}}`
	o := jsonlOutput{}
	writeChunks(t, &o, first+"\n"+second+"\n"+notices+"\n"+successfulResult+"\n"+notices+"\n")
	o.finish()
	if o.err != nil || o.pending != 0 || !o.completed || o.result.Answer != "final answer" {
		t.Fatalf("shared notice UUID lost task completion or replayed it: pending=%d completed=%t answer=%q error=%v", o.pending, o.completed, o.result.Answer, o.err)
	}
}

func TestRunLessStartSurvivesReplayedEmptyList(t *testing.T) {
	o := jsonlOutput{}
	const empty = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-empty-1","tasks":[]}`
	const start = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`
	const finish = `{"type":"system","subtype":"task_notification","uuid":"finish-1","task_id":"helper","status":"completed"}`
	writeChunks(t, &o, empty+"\n"+start+"\n"+empty+"\n")
	if o.err != nil || o.pending != 1 || o.completed {
		t.Fatalf("replayed empty list cleared start: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
	writeChunks(t, &o, finish+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending != 0 || !o.completed {
		t.Fatalf("attributable finish did not clear start: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
}

func TestRunLessFreshListCanClearListOnlyTask(t *testing.T) {
	o := jsonlOutput{}
	const listed = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-open-1","tasks":[{"task_id":"helper"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-empty-1","tasks":[]}`
	writeChunks(t, &o, listed+"\n")
	if o.err != nil || o.pending != 1 || o.completed {
		t.Fatalf("list did not open task: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
	writeChunks(t, &o, empty+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending != 0 || !o.completed {
		t.Fatalf("fresh empty list did not clear list-only task: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
}

func TestRunLessUnidentifiedListCannotClearPending(t *testing.T) {
	o := jsonlOutput{}
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	writeChunks(t, &o, listed+"\n"+empty+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending != 1 || o.completed {
		t.Fatalf("unidentified list cleared task: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
}

func TestRunLessFreshListCannotClearExplicitStart(t *testing.T) {
	o := jsonlOutput{}
	const started = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-empty-1","tasks":[]}`
	writeChunks(t, &o, started+"\n"+empty+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending != 1 || o.completed {
		t.Fatalf("list without start identity cleared task: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
}

func TestRunLessUUIDHistoryIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
	}{
		{"starts", `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`, `{"type":"system","subtype":"task_started","uuid":"start-2","task_id":"helper"}`},
		{"lists", `{"type":"system","subtype":"background_tasks_changed","uuid":"list-1","tasks":[]}`, `{"type":"system","subtype":"background_tasks_changed","uuid":"list-2","tasks":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{progressEntryLimit: 1}
			writeChunks(t, &o, tc.first+"\n"+tc.second+"\n")
			if !errors.Is(o.err, aicli.ErrOutputLimit) || o.progressEntries != 1 {
				t.Fatalf("UUID history escaped bound: entries=%d error=%v", o.progressEntries, o.err)
			}
		})
	}
}

func TestRunLessListCountsConcurrentSameIDTasks(t *testing.T) {
	o := jsonlOutput{}
	const listed = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-two-1","tasks":[{"task_id":"helper"},{"task_id":"helper"}]}`
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	writeChunks(t, &o, listed+"\n"+finished+"\n"+finished+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending != 2 || o.completed {
		t.Fatalf("ambiguous notices cleared runless tasks: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
	writeChunks(t, &o, `{"type":"system","subtype":"background_tasks_changed","uuid":"list-empty-1","tasks":[]}`+"\n")
	if o.err != nil || o.pending != 0 {
		t.Fatalf("empty list did not clear tasks: pending=%d error=%v", o.pending, o.err)
	}
}

func TestRunLessListShrinkKeepsReusedIDPending(t *testing.T) {
	o := jsonlOutput{}
	const two = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-two-1","tasks":[{"task_id":"helper"},{"task_id":"helper"}]}`
	const one = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-one-1","tasks":[{"task_id":"helper"}]}`
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-empty-1","tasks":[]}`
	writeChunks(t, &o, two+"\n"+one+"\n"+finished+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending != 1 || o.completed {
		t.Fatalf("delayed finish cleared remaining same-ID task: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
	writeChunks(t, &o, one+"\n"+finished+"\n")
	if o.err != nil || o.pending != 1 {
		t.Fatalf("repeated list/finish cleared ambiguous task: pending=%d error=%v", o.pending, o.err)
	}
	writeChunks(t, &o, empty+"\n")
	if o.err != nil || o.pending != 0 {
		t.Fatalf("authoritative empty list did not clear task: pending=%d error=%v", o.pending, o.err)
	}
}

func TestTaggedListAndSyntheticNoticeCannotClearOpenRun(t *testing.T) {
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	const notice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	const runlessFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	writeChunks(t, &o, listed+"\n"+notice+"\n"+runlessFinish+"\n"+empty+"\n"+successfulResult+"\n")
	if o.err != nil || progress != 0 || o.pending != 1 || o.completed {
		t.Fatalf("tagged run cleared early: progress=%d pending=%d completed=%t error=%v", progress, o.pending, o.completed, o.err)
	}
	writeChunks(t, &o, finished+"\n")
	if o.err != nil || progress != 1 || o.pending != 0 {
		t.Fatalf("native finish did not clear run: progress=%d pending=%d error=%v", progress, o.pending, o.err)
	}
}

func TestRunLessFinishCannotCompleteLaterTaggedRun(t *testing.T) {
	const listed = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-one-1","tasks":[{"task_id":"helper"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","uuid":"list-empty-1","tasks":[]}`
	const notice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	const later = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-2"}]}`
	for _, tc := range []struct{ name, first string }{{"list", listed + "\n" + empty}, {"notice", notice}} {
		t.Run(tc.name, func(t *testing.T) {
			progress := 0
			o := jsonlOutput{onProgress: func() { progress++ }}
			writeChunks(t, &o, tc.first+"\n"+later+"\n"+successfulResult+"\n")
			if o.err != nil || progress != 0 || o.pending != 1 || o.completed ||
				o.progressSeen["task_finish:"+taskRunKey("helper", "run-2")] {
				t.Fatalf("later run was finished by runless history: progress=%d pending=%d completed=%t error=%v", progress, o.pending, o.completed, o.err)
			}
		})
	}
}

func TestRunLessStartNeedsOwnFinishBesideTaggedRun(t *testing.T) {
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	const taggedStart = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const runlessStart = `{"type":"system","subtype":"task_started","task_id":"helper"}`
	const runlessFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	const taggedFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	writeChunks(t, &o, taggedStart+"\n"+runlessStart+"\n"+runlessFinish+"\n")
	if o.err != nil || progress != 1 || o.pending != 2 {
		t.Fatalf("ambiguous runless finish cleared overlapping work: progress=%d pending=%d error=%v", progress, o.pending, o.err)
	}
	writeChunks(t, &o, taggedFinish+"\n")
	if o.err != nil || progress != 2 || o.pending != 1 {
		t.Fatalf("tagged finish: progress=%d pending=%d error=%v", progress, o.pending, o.err)
	}
	const ownFinish = `{"type":"system","subtype":"task_notification","uuid":"own-finish","task_id":"helper","status":"completed"}`
	writeChunks(t, &o, ownFinish+"\n")
	if o.err != nil || o.pending != 0 {
		t.Fatalf("independent runless finish: pending=%d error=%v", o.pending, o.err)
	}
}

func TestRunLessStartBeforeTaggedRunNeedsOwnNotice(t *testing.T) {
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	const runlessStart = `{"type":"system","subtype":"task_started","task_id":"helper"}`
	const taggedStart = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const taggedFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	const runlessFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	writeChunks(t, &o, runlessStart+"\n"+taggedStart+"\n"+taggedFinish+"\n"+successfulResult+"\n")
	if o.err != nil || progress != 2 || o.pending != 1 || o.completed {
		t.Fatalf("unidentified run cleared without proof: progress=%d pending=%d completed=%t error=%v", progress, o.pending, o.completed, o.err)
	}
	writeChunks(t, &o, empty+"\n")
	if o.err != nil || o.pending != 1 {
		t.Fatalf("empty list cleared distinct runless pending: pending=%d error=%v", o.pending, o.err)
	}
	writeChunks(t, &o, runlessFinish+"\n")
	if o.err != nil || o.pending != 0 {
		t.Fatalf("own notice did not clear runless pending: pending=%d error=%v", o.pending, o.err)
	}
}

func TestFinishedTaggedRunIgnoresDelayedUsageAndReplay(t *testing.T) {
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const usage = `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":1}}`
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	const delayed = `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5}}`
	const stale = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	writeChunks(t, &o, started+"\n"+usage+"\n"+finished+"\n"+delayed+"\n"+stale+"\n"+started+"\n")
	_, retained := o.taskProgress[taskRunKey("helper", "run-1")]
	if o.err != nil || progress != 3 || o.pending != 0 || retained {
		t.Fatalf("finished run replayed or retained counters: progress=%d pending=%d retained=%t error=%v", progress, o.pending, retained, o.err)
	}
}

func TestSummaryOnlyUsageDoesNotHideLaterTaggedStart(t *testing.T) {
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	const summary = `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","summary":"working"}`
	const runlessFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const nativeFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	writeChunks(t, &o, summary+"\n"+runlessFinish+"\n"+started+"\n")
	if o.err != nil || progress != 1 || o.pending != 1 || !o.progressSeen["task_seen:"+taskRunKey("helper", "run-1")] ||
		!o.progressSeen["task_start:"+taskRunKey("helper", "run-1")] {
		t.Fatalf("summary-only identity hid native start: progress=%d pending=%d error=%v", progress, o.pending, o.err)
	}
	writeChunks(t, &o, started+"\n")
	if o.err != nil || progress != 1 {
		t.Fatalf("replayed native start renewed timer: progress=%d error=%v", progress, o.err)
	}
	writeChunks(t, &o, nativeFinish+"\n")
	if o.err != nil || progress != 2 || o.pending != 0 {
		t.Fatalf("native finish missed: progress=%d pending=%d error=%v", progress, o.pending, o.err)
	}
}

func TestPositiveUsageProvesStartWithoutSecondCallback(t *testing.T) {
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	const usage = `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5}}`
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	writeChunks(t, &o, usage+"\n"+started+"\n"+listed+"\n")
	if o.err != nil || progress != 1 || o.pending != 1 {
		t.Fatalf("usage-first start: progress=%d pending=%d error=%v", progress, o.pending, o.err)
	}
	writeChunks(t, &o, finished+"\n")
	if o.err != nil || progress != 2 || o.pending != 0 {
		t.Fatalf("native completion: progress=%d pending=%d error=%v", progress, o.pending, o.err)
	}
}

func TestRunLessUsageDoesNotRenewTimer(t *testing.T) {
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	const runless = `{"type":"system","subtype":"task_progress","task_id":"helper","usage":{"total_tokens":5}}`
	const tagged = `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5}}`
	writeChunks(t, &o, runless+"\n"+tagged+"\n"+runless+"\n")
	if o.err != nil || progress != 1 {
		t.Fatalf("runless usage credited: progress=%d error=%v", progress, o.err)
	}
}

func TestRunLessUsageKeepsTaskPendingWithoutTimerCredit(t *testing.T) {
	const usage = `{"type":"system","subtype":"task_progress","task_id":"helper","usage":{"total_tokens":5}}`
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	for _, tc := range []struct {
		name, events string
		pending      int
	}{
		{"unfinished", usage + "\n" + successfulResult, 1},
		{"completion", usage + "\n" + finished + "\n" + successfulResult, 0},
		{"empty list", usage + "\n" + empty + "\n" + successfulResult, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			progress := 0
			o := jsonlOutput{onProgress: func() { progress++ }}
			writeChunks(t, &o, tc.events+"\n")
			o.finish()
			if o.pending != tc.pending || o.completed != (tc.pending == 0) || (o.err != nil) != (tc.pending != 0) || progress != 0 {
				t.Fatalf("runless usage: pending=%d completed=%t progress=%d error=%v", o.pending, o.completed, progress, o.err)
			}
		})
	}
}

func TestTaggedFinishClearsPendingWithoutProgressCallback(t *testing.T) {
	o := jsonlOutput{}
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	writeChunks(t, &o, started+"\n")
	if o.err != nil || o.pending != 1 {
		t.Fatalf("tagged start without callback: pending=%d error=%v", o.pending, o.err)
	}
	writeChunks(t, &o, finished+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending != 0 || !o.completed {
		t.Fatalf("tagged finish without callback: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
}

func TestTaggedListWithoutProgressCallbackHasBoundedHistory(t *testing.T) {
	o := jsonlOutput{progressEntryLimit: 1}
	const first = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	const second = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"},{"task_id":"helper","run_id":"run-2"}]}`
	writeChunks(t, &o, first+"\n")
	if o.err != nil || o.pending != 1 || o.progressEntries != 1 {
		t.Fatalf("first tagged list: pending=%d entries=%d error=%v", o.pending, o.progressEntries, o.err)
	}
	writeChunks(t, &o, second+"\n")
	if !errors.Is(o.err, aicli.ErrOutputLimit) || o.progressEntries != 1 {
		t.Fatalf("unbounded tagged list history: entries=%d error=%v", o.progressEntries, o.err)
	}
}

func TestSyntheticSummaryMarkupCannotSupplyTaskFields(t *testing.T) {
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	const notice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><status>completed</status><summary>See <task-id>helper</task-id> in output</summary></task-notification>"}}`
	o := jsonlOutput{}
	writeChunks(t, &o, started+"\n"+listed+"\n"+notice+"\n"+successfulResult+"\n")
	if o.err != nil || o.pending != 1 || o.completed {
		t.Fatalf("summary markup cleared tagged run: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
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
		`{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>worker</task-id><status>stopped"}}`,
	} {
		writeChunks(t, &o, line+"\n")
	}
	if got != 0 || o.err != nil || o.stopped {
		t.Fatalf("unbound or malformed content changed task state: count=%d stopped=%t error=%v", got, o.stopped, o.err)
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
	var progress atomic.Int32
	session.OnProgress = func() { progress.Add(1) }
	result, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if err != nil || result.Answer != "final answer" || progress.Load() != 3 {
		t.Fatalf("Run=%+v, %v; progress=%d, want start, native output, and exit", result, err, progress.Load())
	}
}

func TestSessionRunResumeKeepsProgressIdentity(t *testing.T) {
	const oldMessage = `{"type":"assistant","message":{"id":"old","content":[{"type":"text","text":"old work"}]}}`
	const newMessage = `{"type":"assistant","message":{"id":"new","content":[{"type":"text","text":"new work"}]}}`
	fakeExecutable(t, "claude", `n=0
[ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
printf '%s\n' '`+oldMessage+`'
if [ "$n" -eq 2 ]; then printf '%s\n' '`+newMessage+`'; fi
printf '%s\n' '`+successfulResult+`'
`)
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	var progress atomic.Int32
	session.OnProgress = func() { progress.Add(1) }
	workspace := t.TempDir()
	for call := 1; call <= 2; call++ {
		result, err := session.Run(t.Context(), workspace, "task", false, "", config.ClaudeConfig{})
		if err != nil || result.Answer != "final answer" || progress.Load() != int32(3*call) {
			t.Fatalf("call %d: result=%+v error=%v progress=%d", call, result, err, progress.Load())
		}
	}
}
