package claudecli

import (
	"errors"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
)

func TestProgressHistoryIsBoundedAcrossResumedCalls(t *testing.T) {
	progress := 0
	const started = `{"type":"system","subtype":"task_started","session_id":"native-id","task_id":"helper","run_id":"run"}`
	const usage = `{"type":"system","subtype":"task_progress","session_id":"native-id","task_id":"helper","run_id":"run","usage":{"total_tokens":1}}`
	first := jsonlOutput{sessionID: "native-id", onProgress: func() { progress++ }, progressEntryLimit: 5}
	writeChunks(t, &first, started+"\n"+usage+"\n")
	if first.err != nil || progress != 2 || first.progressEntries != 3 {
		t.Fatalf("initial progress: count=%d entries=%d err=%v", progress, first.progressEntries, first.err)
	}
	resumed := jsonlOutput{sessionID: "native-id", onProgress: first.onProgress,
		progressSeen: first.progressSeen, taskProgress: first.taskProgress,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries, progressEntryLimit: 5}
	writeChunks(t, &resumed, started+"\n"+usage+"\n"+
		strings.ReplaceAll(started, `"run_id":"run"`, `"run_id":"run-2"`)+"\n"+
		strings.ReplaceAll(usage, `"run_id":"run"`, `"run_id":"run-2"`)+"\n")
	if resumed.err != nil || progress != 4 || resumed.progressEntries != 5 {
		t.Fatalf("resumed progress: count=%d entries=%d err=%v", progress, resumed.progressEntries, resumed.err)
	}
	writeChunks(t, &resumed, strings.ReplaceAll(started, `"run_id":"run"`, `"run_id":"run-3"`)+"\n")
	if !errors.Is(resumed.err, aicli.ErrOutputLimit) || progress != 4 || resumed.progressEntries != 5 {
		t.Fatalf("entry limit: count=%d entries=%d err=%v", progress, resumed.progressEntries, resumed.err)
	}

	progress = 0
	limited := jsonlOutput{onProgress: func() { progress++ }, progressByteLimit: 20}
	writeChunks(t, &limited, `{"type":"system","subtype":"task_started","task_id":"h"}`+"\n")
	writeChunks(t, &limited, `{"type":"system","subtype":"task_progress","task_id":"h","usage":{"total_tokens":1}}`+"\n")
	if limited.err != nil || limited.progressBytes != 13 || progress != 2 {
		t.Fatalf("initial byte budget: bytes=%d count=%d err=%v", limited.progressBytes, progress, limited.err)
	}
	resumedBytes := jsonlOutput{onProgress: limited.onProgress,
		progressSeen: limited.progressSeen, taskProgress: limited.taskProgress,
		progressBytes: limited.progressBytes, progressEntries: limited.progressEntries, progressByteLimit: 20}
	writeChunks(t, &resumedBytes, `{"type":"system","subtype":"task_progress","task_id":"h","usage":{"total_tokens":1}}`+"\n")
	writeChunks(t, &resumedBytes, `{"type":"system","subtype":"task_progress","task_id":"longerid","usage":{"total_tokens":1}}`+"\n")
	if !errors.Is(resumedBytes.err, aicli.ErrOutputLimit) || progress != 2 || resumedBytes.progressBytes != 13 {
		t.Fatalf("resumed byte limit: count=%d bytes=%d err=%v", progress, resumedBytes.progressBytes, resumedBytes.err)
	}
	session := Session{progressSeen: resumed.progressSeen, taskProgress: resumed.taskProgress,
		progressBytes: resumed.progressBytes, progressEntries: resumed.progressEntries}
	session.NewConversation()
	if session.progressSeen != nil || session.taskProgress != nil || session.progressBytes != 0 || session.progressEntries != 0 {
		t.Fatal("new conversation retained progress history")
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
					if o.err == nil || progress != 1 || o.completed || o.result.Answer != "" {
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
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"},{"task_id":"ambient","ambient":true}]}`, 9},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"},{"task_id":"ambient","ambient":true}]}`, 9},
		{`{"type":"system","subtype":"task_notification","task_id":"worker","status":"completed"}`, 10},
		{`{"type":"system","subtype":"task_notification","task_id":"worker","status":"completed"}`, 10},
		{`{"type":"system","subtype":"task_notification","task_id":"ambient","status":"completed","ambient":true}`, 10},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`, 10},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`, 10},
		{`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`, 10},
		{successfulResult, 10},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"message_start","message":{"id":"message-1"}}}`, 10},
		{`{"type":"stream_event","session_id":"native-id","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"working"}}}`, 10},
		{`{"type":"assistant","session_id":"foreign","parent_tool_use_id":null,"message":{"id":"foreign","content":[{"type":"text","text":"foreign work"}]}}`, 10},
	} {
		writeChunks(t, &o, step.line+"\n")
		if o.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, o.err)
		}
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
	if resumed.err != nil || got != 2 {
		t.Fatalf("replay or repeated task notice renewed progress: got=%d error=%v", got, resumed.err)
	}
	const worker = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"}]}`
	const clear = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	writeChunks(t, &resumed, worker+"\n"+clear+"\n")
	replayed := jsonlOutput{sessionID: "native-id", onProgress: first.onProgress, progressSeen: resumed.progressSeen}
	writeChunks(t, &replayed, worker+"\n"+clear+"\n")
	if got != 3 || replayed.err != nil {
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

func TestProgressSyntheticCompletionAfterNativeStart(t *testing.T) {
	const notice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	var got int
	first := jsonlOutput{onProgress: func() { got++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`, 1},
		{notice, 2},
		{notice, 2},
	} {
		writeChunks(t, &first, step.line+"\n")
		if first.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, first.err)
		}
	}
	resumed := jsonlOutput{onProgress: first.onProgress, progressSeen: first.progressSeen}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`, 2},
		{notice, 2},
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}`, 3},
		{notice, 4},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`, 4},
	} {
		writeChunks(t, &resumed, step.line+"\n")
		if resumed.err != nil || got != step.want {
			t.Fatalf("resumed step %d: progress=%d, want %d; parser error=%v", i, got, step.want, resumed.err)
		}
	}
}

func TestProgressSyntheticCompletionBeforeNativeRun(t *testing.T) {
	const notice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	var got int
	first := jsonlOutput{onProgress: func() { got++ }}
	writeChunks(t, &first, notice+"\n")
	if first.err != nil || got != 1 {
		t.Fatalf("synthetic completion: count=%d error=%v", got, first.err)
	}
	resumed := jsonlOutput{onProgress: first.onProgress, progressSeen: first.progressSeen}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`, 1},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`, 1},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`, 2},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`, 2},
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-3"}`, 3},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-3","status":"completed"}`, 4},
	} {
		writeChunks(t, &resumed, step.line+"\n")
		if resumed.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, resumed.err)
		}
	}
}

func TestProgressNativeUsageBeforeSyntheticCompletion(t *testing.T) {
	const notice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	var got int
	o := jsonlOutput{onProgress: func() { got++ }}
	writeChunks(t, &o, `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5}}`+"\n"+notice+"\n")
	if o.err != nil || got != 2 {
		t.Fatalf("synthetic completion after native usage: count=%d error=%v", got, o.err)
	}
	writeChunks(t, &o, `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`+"\n")
	if o.err != nil || got != 2 {
		t.Fatalf("native completion repeated synthetic completion: count=%d error=%v", got, o.err)
	}
	writeChunks(t, &o, `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-2","usage":{"total_tokens":1}}`+"\n"+notice+"\n")
	if o.err != nil || got != 4 {
		t.Fatalf("reused task ID lost synthetic completion: count=%d error=%v", got, o.err)
	}
	writeChunks(t, &o, `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`+"\n")
	if o.err != nil || got != 4 {
		t.Fatalf("reused task ID repeated synthetic completion: count=%d error=%v", got, o.err)
	}
}

func TestProgressSyntheticTaskNoticeDoesNotReplayNativeRun(t *testing.T) {
	const notice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	for _, tc := range []struct {
		name, native string
	}{
		{"task events", `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}` + "\n" +
			`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`},
		{"task list", `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}` + "\n" +
			`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got int
			first := jsonlOutput{onProgress: func() { got++ }}
			writeChunks(t, &first, tc.native+"\n")
			if first.err != nil || got != 2 {
				t.Fatalf("native task progress=%d, error=%v", got, first.err)
			}
			resumed := jsonlOutput{onProgress: first.onProgress, progressSeen: first.progressSeen,
				progressBytes: first.progressBytes, progressEntries: first.progressEntries}
			writeChunks(t, &resumed, notice+"\n")
			if resumed.err != nil || got != 2 {
				t.Fatalf("synthetic replay renewed task progress: count=%d, error=%v", got, resumed.err)
			}
		})
	}
	var got int
	legacy := jsonlOutput{onProgress: func() { got++ }}
	writeChunks(t, &legacy, notice+"\n")
	if legacy.err != nil || got != 1 {
		t.Fatalf("legacy synthetic-only task progress=%d, error=%v", got, legacy.err)
	}
}

func TestProgressTaskListUsesRunIdentity(t *testing.T) {
	var got int
	o := jsonlOutput{onProgress: func() { got++ }}
	const first = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	const second = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-2"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	for i, step := range []struct {
		line string
		want int
	}{
		{first, 1},
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`, 1},
		{first, 1},
		{second, 3},
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}`, 3},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`, 3},
		{second, 3},
		{empty, 4},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`, 4},
		{empty, 4},
	} {
		writeChunks(t, &o, step.line+"\n")
		if o.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, o.err)
		}
	}
	replayed := jsonlOutput{onProgress: o.onProgress, progressSeen: o.progressSeen}
	writeChunks(t, &replayed, first+"\n"+empty+"\n")
	if replayed.err != nil || got != 4 {
		t.Fatalf("replayed task-list run renewed progress: count=%d error=%v", got, replayed.err)
	}
	const third = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-3"}]}`
	writeChunks(t, &replayed, `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-3"}`+"\n"+third+"\n")
	if replayed.err != nil || got != 5 {
		t.Fatalf("task list repeated task start: count=%d error=%v", got, replayed.err)
	}
	writeChunks(t, &replayed, `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-3","status":"completed"}`+"\n"+empty+"\n")
	if replayed.err != nil || got != 6 {
		t.Fatalf("task list repeated task finish: count=%d error=%v", got, replayed.err)
	}
	const legacy = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	writeChunks(t, &replayed, legacy+"\n"+empty+"\n")
	if replayed.err != nil || got != 8 {
		t.Fatalf("legacy task-list fallback: count=%d error=%v", got, replayed.err)
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
