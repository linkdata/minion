package claudecli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
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
	const firstFrame = `{"type":"system","subtype":"thinking_tokens","session_id":"native-id","uuid":"frame-1","estimated_tokens":8,"estimated_tokens_delta":8}`
	const secondFrame = `{"type":"system","subtype":"thinking_tokens","session_id":"native-id","uuid":"frame-2","estimated_tokens":12,"estimated_tokens_delta":4}`
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"assistant","session_id":"native-id","message":{"id":"redacted","content":[{"type":"redacted_thinking","data":"opaque"}]}}`, 0},
		{firstFrame, 1},
		{firstFrame, 1},
		{secondFrame, 2},
		{`{"type":"system","subtype":"thinking_tokens","session_id":"native-id","uuid":"zero","estimated_tokens":12,"estimated_tokens_delta":0}`, 2},
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
	writeChunks(t, &resumed, `{"type":"system","subtype":"thinking_tokens","session_id":"native-id","uuid":"frame-3","estimated_tokens_delta":1}`+"\n")
	if resumed.err != nil || got != 3 {
		t.Fatalf("resumed thinking progress=%d, want 3; parser error=%v", got, resumed.err)
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

func TestRunLessNoticeWithoutUUIDAllowsIdentifiableLaterFinish(t *testing.T) {
	o := jsonlOutput{}
	const first = `{"type":"system","subtype":"task_started","uuid":"start-1","task_id":"helper"}`
	const second = `{"type":"system","subtype":"task_started","uuid":"start-2","task_id":"helper"}`
	const bare = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	const identified = `{"type":"system","subtype":"task_notification","uuid":"finish-2","task_id":"helper","status":"completed"}`
	writeChunks(t, &o, first+"\n"+bare+"\n"+second+"\n"+bare+"\n"+identified+"\n"+successfulResult+"\n")
	o.finish()
	if o.pending != 0 || !o.completed || o.err != nil {
		t.Fatalf("identified later finish was not accepted: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
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

func TestRunLessStartDoesNotReplayTaggedStart(t *testing.T) {
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	const taggedStart = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const runlessStart = `{"type":"system","subtype":"task_started","task_id":"helper"}`
	const runlessFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	const taggedFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	writeChunks(t, &o, taggedStart+"\n"+runlessStart+"\n"+runlessFinish+"\n")
	if o.err != nil || progress != 1 || o.pending != 1 {
		t.Fatalf("runless replay renewed or cleared tagged work: progress=%d pending=%d error=%v", progress, o.pending, o.err)
	}
	writeChunks(t, &o, taggedFinish+"\n")
	if o.err != nil || progress != 2 || o.pending != 0 {
		t.Fatalf("tagged finish: progress=%d pending=%d error=%v", progress, o.pending, o.err)
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
	if o.err != nil || progress != 3 || o.pending != 0 || o.taskProgress[taskRunKey("helper", "run-1")].totalTokens != 1 {
		t.Fatalf("finished run replayed: progress=%d pending=%d error=%v", progress, o.pending, o.err)
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
	var progress int
	session.OnProgress = func() { progress++ }
	result, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if err != nil || result.Answer != "final answer" || progress != 1 {
		t.Fatalf("Run=%+v, %v; progress=%d, want 1", result, err, progress)
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
	progress := 0
	session.OnProgress = func() { progress++ }
	workspace := t.TempDir()
	for call := 1; call <= 2; call++ {
		result, err := session.Run(t.Context(), workspace, "task", false, "", config.ClaudeConfig{})
		if err != nil || result.Answer != "final answer" || progress != call {
			t.Fatalf("call %d: result=%+v error=%v progress=%d", call, result, err, progress)
		}
	}
}
