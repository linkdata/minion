package claudecli

import (
	"encoding/json"
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

func TestOpenTaskRunIndexTracksUniqueRunAcrossResume(t *testing.T) {
	const notice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const usage = `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-2","usage":{"total_tokens":1}}`
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-3"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	const finishSecond = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`
	var progress int
	first := jsonlOutput{onProgress: func() { progress++ }}
	writeChunks(t, &first, started+"\n")
	if got := first.openTaskRun("helper"); got != "run-1" {
		t.Fatalf("started run: got %q", got)
	}
	for i := range 1024 {
		if !first.rememberProgress(fmt.Sprintf("assistant:unrelated-%d", i)) {
			t.Fatalf("unrelated history entry %d was not recorded: %v", i, first.err)
		}
	}
	if len(first.openTaskRuns) != 1 {
		t.Fatalf("unrelated history entered run index: %d tasks", len(first.openTaskRuns))
	}
	writeChunks(t, &first, usage+"\n"+listed+"\n")
	if got := len(first.openTaskRuns["helper"].many); got != 3 {
		t.Fatalf("task list did not add third run: %d", got)
	}
	writeChunks(t, &first, empty+"\n")
	if got := len(first.openTaskRuns["helper"].many); got != 2 {
		t.Fatalf("task list did not finish third run: %d", got)
	}
	if got := first.openTaskRun("helper"); got != "" {
		t.Fatalf("two unfinished runs matched as %q", got)
	}
	writeChunks(t, &first, strings.Repeat(notice+"\n", 128))
	if progress != 4 || first.err != nil {
		t.Fatalf("ambiguous notices renewed progress: %d, error=%v", progress, first.err)
	}
	writeChunks(t, &first, finishSecond+"\n")
	if got := first.openTaskRun("helper"); got != "run-1" {
		t.Fatalf("run after finish: got %q, index=%+v, progress=%d, err=%v", got, first.openTaskRuns, progress, first.err)
	}

	resumed := jsonlOutput{onProgress: first.onProgress, progressSeen: first.progressSeen,
		taskProgress: first.taskProgress, openTaskRuns: first.openTaskRuns,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	writeChunks(t, &resumed, strings.Repeat(notice+"\n", 128))
	if got := resumed.openTaskRun("helper"); resumed.err != nil || got != "run-1" || progress != 5 {
		t.Fatalf("ambiguous resumed notices: open=%q progress=%d error=%v", got, progress, resumed.err)
	}
	writeChunks(t, &resumed, started+"\n"+usage+"\n"+finishSecond+"\n"+notice+"\n")
	if got := resumed.openTaskRun("helper"); resumed.err != nil || got != "run-1" || progress != 5 {
		t.Fatalf("ambiguous replay finished open run: open=%q progress=%d error=%v", got, progress, resumed.err)
	}
	writeChunks(t, &resumed, `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`+"\n")
	if got := resumed.openTaskRun("helper"); resumed.err != nil || got != "" || progress != 6 {
		t.Fatalf("identified finish lost: open=%q progress=%d error=%v", got, progress, resumed.err)
	}
	session := Session{progressSeen: resumed.progressSeen, taskProgress: resumed.taskProgress,
		openTaskRuns: resumed.openTaskRuns}
	session.NewConversation()
	if session.openTaskRuns != nil || session.progressSeen != nil || session.taskProgress != nil {
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
		{`{"type":"system","subtype":"background_tasks_changed","session_id":"native-id","tasks":[{"task_id":"ambient-list"}]}`, 4},
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
		{notice, 3},
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

func TestProgressSyntheticCompletionBindsFirstNativeRunOnResume(t *testing.T) {
	const notice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"start then finish", []string{started, finished}},
		{"finish then start", []string{finished, started}},
		{"task list then empty", []string{listed, empty}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := 0
			first := jsonlOutput{onProgress: func() { got++ }}
			writeChunks(t, &first, notice+"\n")
			if first.err != nil || got != 1 {
				t.Fatalf("synthetic completion: progress=%d error=%v", got, first.err)
			}
			resumed := jsonlOutput{onProgress: first.onProgress, progressSeen: first.progressSeen,
				progressBytes: first.progressBytes, progressEntries: first.progressEntries}
			for _, line := range tc.lines {
				writeChunks(t, &resumed, line+"\n")
				if resumed.err != nil || got != 1 {
					t.Fatalf("first native run renewed progress after %s: progress=%d error=%v", line, got, resumed.err)
				}
			}
			writeChunks(t, &resumed, started+"\n"+finished+"\n")
			if resumed.err != nil || got != 1 {
				t.Fatalf("native replay renewed progress: progress=%d error=%v", got, resumed.err)
			}
			const fresh = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}` + "\n" +
				`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}` + "\n"
			writeChunks(t, &resumed, fresh)
			if resumed.err != nil || got != 3 {
				t.Fatalf("distinct native run lost progress: progress=%d error=%v", got, resumed.err)
			}
		})
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
	if o.err != nil || got != 3 {
		t.Fatalf("run-less completion finished reused task ID: count=%d error=%v", got, o.err)
	}
	writeChunks(t, &o, `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`+"\n")
	if o.err != nil || got != 4 {
		t.Fatalf("identified reused task completion: count=%d error=%v", got, o.err)
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
	if replayed.err != nil || got != 6 {
		t.Fatalf("run-less replay renewed progress: count=%d error=%v", got, replayed.err)
	}
}

func TestProgressRunLessTaskListUpgradesToNativeRun(t *testing.T) {
	got := 0
	o := jsonlOutput{onProgress: func() { got++ }}
	const runless = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const runful = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	for i, step := range []struct {
		line string
		want int
	}{
		{runless, 1},
		{runful, 1},
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`, 1},
		{runful, 1},
		{runless, 1},
		{empty, 2},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`, 2},
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}`, 3},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`, 4},
	} {
		writeChunks(t, &o, step.line+"\n")
		if o.err != nil || got != step.want {
			t.Fatalf("step %d: progress=%d, want %d; parser error=%v", i, got, step.want, o.err)
		}
	}
	resumed := jsonlOutput{onProgress: o.onProgress, progressSeen: o.progressSeen,
		progressBytes: o.progressBytes, progressEntries: o.progressEntries}
	writeChunks(t, &resumed, runless+"\n"+runful+"\n"+empty+"\n")
	if resumed.err != nil || got != 4 {
		t.Fatalf("resumed task-list replay renewed progress: progress=%d error=%v", got, resumed.err)
	}
}

func TestProgressRunLessTaskListUpgradeAcrossResume(t *testing.T) {
	got := 0
	first := jsonlOutput{onProgress: func() { got++ }}
	const runless = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const runful = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	writeChunks(t, &first, runless+"\n")
	if first.err != nil || got != 1 {
		t.Fatalf("run-less start: progress=%d error=%v", got, first.err)
	}
	resumed := jsonlOutput{onProgress: first.onProgress, progressSeen: first.progressSeen,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	writeChunks(t, &resumed, runful+"\n"+empty+"\n")
	if resumed.err != nil || got != 2 {
		t.Fatalf("run identity upgrade across resume: progress=%d error=%v", got, resumed.err)
	}
	writeChunks(t, &resumed, `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}`+"\n"+
		`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`+"\n")
	if resumed.err != nil || got != 4 {
		t.Fatalf("distinct run after completed upgrade: progress=%d error=%v", got, resumed.err)
	}
}

func TestProgressRunLessListNativeFinishCountsOnce(t *testing.T) {
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	for _, resume := range []bool{false, true} {
		for _, nativeFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("resume=%t/native_first=%t", resume, nativeFirst), func(t *testing.T) {
				got := 0
				first := jsonlOutput{onProgress: func() { got++ }}
				writeChunks(t, &first, listed+"\n")
				if first.err != nil || got != 1 {
					t.Fatalf("list start: progress=%d error=%v", got, first.err)
				}
				current := &first
				if resume {
					current = &jsonlOutput{onProgress: first.onProgress, progressSeen: first.progressSeen,
						progressBytes: first.progressBytes, progressEntries: first.progressEntries}
				}
				lines := []string{empty, finished}
				if nativeFirst {
					lines[0], lines[1] = lines[1], lines[0]
				}
				for _, line := range lines {
					writeChunks(t, current, line+"\n")
				}
				if current.err != nil || got != 2 {
					t.Fatalf("one task finish counted more than once: progress=%d error=%v", got, current.err)
				}
			})
		}
	}
}

func TestProgressRunLessListDoesNotBindFinishedNativeRun(t *testing.T) {
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	for _, stale := range []string{started, `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`} {
		t.Run(stale, func(t *testing.T) {
			got := 0
			o := jsonlOutput{onProgress: func() { got++ }}
			for _, step := range []struct {
				line string
				want int
			}{{started, 1}, {finished, 2}, {listed, 2}, {stale, 2}, {empty, 2}} {
				writeChunks(t, &o, step.line+"\n")
				if o.err != nil || got != step.want {
					t.Fatalf("after %s: progress=%d, want %d; error=%v", step.line, got, step.want, o.err)
				}
			}
		})
	}
}

func TestProgressFinishedNativeRunDoesNotReplayStart(t *testing.T) {
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	for _, stale := range []struct {
		name, line string
	}{
		{"task started", `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`},
		{"task list", `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`},
	} {
		for _, resume := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/resume=%t", stale.name, resume), func(t *testing.T) {
				got := 0
				first := jsonlOutput{onProgress: func() { got++ }}
				writeChunks(t, &first, finished+"\n"+successfulResult+"\n")
				if first.err != nil || got != 1 || !first.completed {
					t.Fatalf("native finish and result: progress=%d completed=%t error=%v", got, first.completed, first.err)
				}
				current := &first
				if resume {
					current = &jsonlOutput{onProgress: first.onProgress, progressSeen: first.progressSeen,
						progressBytes: first.progressBytes, progressEntries: first.progressEntries}
				}
				writeChunks(t, current, stale.line+"\n")
				if current.err != nil || got != 1 || current.pending != 0 || !resume && (!current.completed || current.result.Answer != "final answer") {
					t.Fatalf("finished run replayed state: progress=%d pending=%d completed=%t answer=%q error=%v", got, current.pending, current.completed, current.result.Answer, current.err)
				}
				writeChunks(t, current, empty+"\n")
				if current.err != nil || got != 1 {
					t.Fatalf("finished run replayed start: progress=%d error=%v", got, current.err)
				}
				writeChunks(t, current, `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}`+"\n"+
					`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`+"\n")
				if current.err != nil || got != 3 {
					t.Fatalf("distinct run lost progress: progress=%d error=%v", got, current.err)
				}
			})
		}
	}
}

func TestProgressStaleFinishedRunListPreservesOtherActiveRun(t *testing.T) {
	got := 0
	o := jsonlOutput{onProgress: func() { got++ }}
	const stale = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	const active = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-2"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	writeChunks(t, &o, `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`+"\n"+
		`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`+"\n"+
		`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}`+"\n"+active+"\n")
	if o.err != nil || got != 3 || o.pending != 1 || !o.progressTasks[taskRunKey("helper", "run-2")] {
		t.Fatalf("second run not active: progress=%d pending=%d tasks=%v error=%v", got, o.pending, o.progressTasks, o.err)
	}
	writeChunks(t, &o, stale+"\n"+stale+"\n")
	if o.err != nil || got != 3 || o.pending != 1 || !o.progressTasks[taskRunKey("helper", "run-2")] || o.progressSeen["task_finish:"+taskRunKey("helper", "run-2")] {
		t.Fatalf("stale list finished active run: progress=%d pending=%d tasks=%v error=%v", got, o.pending, o.progressTasks, o.err)
	}
	writeChunks(t, &o, `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`+"\n"+empty+"\n")
	if o.err != nil || got != 4 || o.pending != 0 {
		t.Fatalf("second run finish: progress=%d pending=%d error=%v", got, o.pending, o.err)
	}
}

func TestFinishedRunListWithoutProgressCallbackKeepsResult(t *testing.T) {
	const finished = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	const stale = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	first := jsonlOutput{onProgress: func() {}}
	writeChunks(t, &first, finished+"\n")
	if first.err != nil || !first.progressSeen["task_finish:"+taskRunKey("helper", "run-1")] {
		t.Fatalf("missing finished-run history: error=%v", first.err)
	}
	resumed := jsonlOutput{progressSeen: first.progressSeen, progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	writeChunks(t, &resumed, successfulResult+"\n"+stale+"\n")
	if resumed.err != nil || resumed.pending != 0 || !resumed.completed || resumed.result.Answer != "final answer" {
		t.Fatalf("stale list invalidated result without callback: pending=%d completed=%t answer=%q error=%v", resumed.pending, resumed.completed, resumed.result.Answer, resumed.err)
	}
}

func TestProgressMixedRunLessTaskSignalsShareNativeRun(t *testing.T) {
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	const runlessFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	const nativeFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	for _, nativeStartFirst := range []bool{true, false} {
		for _, listFinishFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("native_start_first=%t/list_finish_first=%t", nativeStartFirst, listFinishFirst), func(t *testing.T) {
				progress := 0
				output := jsonlOutput{onProgress: func() { progress++ }}
				check := func(line string, want int) {
					t.Helper()
					writeChunks(t, &output, line+"\n")
					if output.err != nil || progress != want {
						t.Fatalf("after %s: progress=%d, want %d; error=%v", line, progress, want, output.err)
					}
				}
				if nativeStartFirst {
					check(started, 1)
					check(listed, 1)
				} else {
					check(listed, 1)
					check(started, 1)
				}
				check(listed, 1)
				if listFinishFirst {
					check(empty, 2)
					check(runlessFinish, 2)
				} else {
					check(runlessFinish, 2)
					check(listed, 2)
					check(empty, 2)
				}
				check(nativeFinish, 2)
			})
		}
	}
}

func TestProgressRunLessListOverlapDoesNotRetargetNotice(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	for _, step := range []struct {
		line string
		want int
	}{
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`, 1},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`, 1},
		{`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}`, 2},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`, 3},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`, 3},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`, 3},
		{`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`, 3},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`, 3},
		{`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`, 4},
	} {
		writeChunks(t, &output, step.line+"\n")
		if output.err != nil || progress != step.want {
			t.Fatalf("after %s: progress=%d, want %d; error=%v", step.line, progress, step.want, output.err)
		}
	}
}

func TestProgressDelayedRunLessSignalsDoNotFinishReusedTaskID(t *testing.T) {
	const startedFirst = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const finishedFirst = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	const startedSecond = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}`
	const listedSecond = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-2"}]}`
	const delayedNotice = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	const delayedSyntheticNotice = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	const delayedList = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	for _, tc := range []struct {
		name, delayed string
	}{
		{"notice", delayedNotice},
		{"synthetic notice", delayedSyntheticNotice},
		{"list", delayedList + "\n" + empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			progress := 0
			o := jsonlOutput{onProgress: func() { progress++ }}
			writeChunks(t, &o, startedFirst+"\n"+finishedFirst+"\n"+startedSecond+"\n"+listedSecond+"\n")
			if o.err != nil || progress != 3 || o.pending != 1 {
				t.Fatalf("setup: progress=%d pending=%d error=%v", progress, o.pending, o.err)
			}
			writeChunks(t, &o, tc.delayed+"\n"+successfulResult+"\n")
			if o.err != nil || progress != 3 || o.pending != 1 || o.completed ||
				o.progressSeen["task_finish:"+taskRunKey("helper", "run-2")] {
				t.Fatalf("delayed %s finished live run: progress=%d pending=%d completed=%t error=%v", tc.name, progress, o.pending, o.completed, o.err)
			}
		})
	}
}

func TestProgressRunLessReplayBeforeReusedTaskStartKeepsPending(t *testing.T) {
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	const startedFirst = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const finishedFirst = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	const staleList = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const startedSecond = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}`
	const finishedSecond = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	writeChunks(t, &o, startedFirst+"\n"+finishedFirst+"\n"+staleList+"\n")
	if o.err != nil || progress != 2 || o.pending != 1 {
		t.Fatalf("stale list: progress=%d pending=%d error=%v", progress, o.pending, o.err)
	}
	writeChunks(t, &o, startedSecond+"\n"+empty+"\n"+successfulResult+"\n")
	if o.err != nil || progress != 3 || o.pending != 1 || o.completed {
		t.Fatalf("stale clear finished new run: progress=%d pending=%d completed=%t error=%v", progress, o.pending, o.completed, o.err)
	}
	writeChunks(t, &o, finishedSecond+"\n"+empty+"\n")
	if o.err != nil || progress != 4 || o.pending != 0 {
		t.Fatalf("identified finish did not clear pending: progress=%d pending=%d error=%v", progress, o.pending, o.err)
	}
}

func TestProgressRunLessReplayAfterNativeSignalsDoesNotRenew(t *testing.T) {
	const nativeUsage = `{"type":"system","subtype":"task_progress","task_id":"helper","run_id":"run-1","usage":{"total_tokens":5}}`
	const runlessUsage = `{"type":"system","subtype":"task_progress","task_id":"helper","usage":{"total_tokens":5}}`
	const nativeFinish = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	const syntheticStart = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>started</status></task-notification>"}}`
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	writeChunks(t, &o, nativeUsage+"\n"+runlessUsage+"\n")
	if o.err != nil || progress != 1 || o.taskProgress["helper"].totalTokens != 0 {
		t.Fatalf("run-less usage replay: progress=%d error=%v", progress, o.err)
	}
	progress = 0
	start := jsonlOutput{onProgress: func() { progress++ }}
	writeChunks(t, &start, nativeUsage+"\n"+nativeFinish+"\n"+syntheticStart+"\n")
	if start.err != nil || progress != 2 || start.progressSeen["task_start:helper"] {
		t.Fatalf("synthetic start replay: progress=%d error=%v", progress, start.err)
	}
}

func TestProgressAmbiguousOpenRunSurvivesResumeWithNewTask(t *testing.T) {
	progress := 0
	first := jsonlOutput{onProgress: func() { progress++ }}
	for _, id := range []string{"helper", "other"} {
		writeChunks(t, &first, fmt.Sprintf(`{"type":"system","subtype":"task_started","task_id":%q,"run_id":"run-1"}`, id)+"\n"+
			fmt.Sprintf(`{"type":"system","subtype":"task_notification","task_id":%q,"run_id":"run-1","status":"completed"}`, id)+"\n")
		writeChunks(t, &first, fmt.Sprintf(`{"type":"system","subtype":"task_started","task_id":%q,"run_id":"run-2"}`, id)+"\n"+
			fmt.Sprintf(`{"type":"system","subtype":"task_notification","task_id":%q,"status":"completed"}`, id)+"\n")
	}
	writeChunks(t, &first, `{"type":"system","subtype":"task_notification","task_id":"other","run_id":"run-2","status":"completed"}`+"\n")
	if first.err != nil || !first.ambiguousOpen["helper"] || first.ambiguousOpen["other"] {
		t.Fatalf("initial ambiguous runs: index=%v error=%v", first.ambiguousOpen, first.err)
	}
	session := Session{progressSeen: first.progressSeen, openTaskRuns: first.openTaskRuns,
		ambiguousOpen: first.ambiguousOpen, progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	resumed := jsonlOutput{onProgress: first.onProgress, progressSeen: session.progressSeen,
		openTaskRuns: session.openTaskRuns, ambiguousOpen: session.ambiguousOpen,
		progressBytes: session.progressBytes, progressEntries: session.progressEntries}
	writeChunks(t, &resumed, `{"type":"system","subtype":"task_started","task_id":"other","run_id":"run-3"}`+"\n"+
		`{"type":"system","subtype":"task_notification","task_id":"other","run_id":"run-3","status":"completed"}`+"\n"+
		`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`+"\n"+successfulResult+"\n")
	if resumed.err != nil || resumed.pending != 1 || resumed.completed || !resumed.ambiguousOpen["helper"] {
		t.Fatalf("resume forgot older open run: pending=%d completed=%t index=%v error=%v", resumed.pending, resumed.completed, resumed.ambiguousOpen, resumed.err)
	}
	session.NewConversation()
	if session.ambiguousOpen != nil {
		t.Fatal("new conversation retained ambiguous task index")
	}
}

func TestProgressSyntheticSummaryMarkupCannotSupplyTaskFields(t *testing.T) {
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-1"}]}`
	for _, tc := range []struct {
		name, notice, setup       string
		wantProgress, wantPending int
		wantCompleted             bool
	}{
		{"task ID", `<task-notification><status>completed</status><summary>See <task-id>helper</task-id> in output</summary></task-notification>`, started + "\n" + listed + "\n", 1, 1, false},
		{"status", `<task-notification><task-id>helper</task-id><summary>See <status>stopped</status> in output</summary></task-notification>`, "", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			progress := 0
			o := jsonlOutput{onProgress: func() { progress++ }}
			writeChunks(t, &o, tc.setup)
			notice, _ := json.Marshal(tc.notice)
			writeChunks(t, &o, `{"type":"user","isSynthetic":true,"message":{"content":`+string(notice)+`}}`+"\n"+successfulResult+"\n")
			if o.err != nil || progress != tc.wantProgress || o.pending != tc.wantPending || o.completed != tc.wantCompleted || o.stopped ||
				o.progressSeen["task_finish:"+taskRunKey("helper", "run-1")] {
				t.Fatalf("summary supplied %s: progress=%d pending=%d completed=%t stopped=%t error=%v", tc.name, progress, o.pending, o.completed, o.stopped, o.err)
			}
		})
	}
}

func TestProgressAmbiguousRunLessListReuseNeedsNativeIdentity(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	const listed = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
	writeChunks(t, &output, `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`+"\n"+
		`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-2"}`+"\n"+
		listed+"\n"+empty+"\n"+
		`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`+"\n"+
		`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-2","status":"completed"}`+"\n")
	if output.err != nil || progress != 4 {
		t.Fatalf("initial native runs: count=%d error=%v", progress, output.err)
	}
	replayed := jsonlOutput{onProgress: output.onProgress, progressSeen: output.progressSeen,
		progressBytes: output.progressBytes, progressEntries: output.progressEntries}
	writeChunks(t, &replayed, listed+"\n"+
		`{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`+"\n"+empty+"\n"+
		`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"helper","run_id":"run-3"}]}`+"\n"+empty+"\n"+
		`{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-3"}`+"\n"+
		`{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-3","status":"completed"}`+"\n")
	if replayed.err != nil || progress != 6 {
		t.Fatalf("ambiguous reuse or new native run: count=%d error=%v", progress, replayed.err)
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
