package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestNewAssistantParentageCannotPreserveStaleResult(t *testing.T) {
	for _, parent := range []struct {
		name, value                 string
		oldResult, stoppedNewResult bool
	}{
		{"omitted", "", false, false},
		{"root", `null`, false, true},
		{"delegate", `"delegate"`, true, false},
		{"empty", `""`, false, false},
		{"number", `0`, false, false},
		{"boolean", `false`, false, false},
		{"object", `{}`, false, false},
		{"array", `[]`, false, false},
	} {
		for _, test := range []struct {
			name                 string
			stopped, fresh, want bool
		}{
			{"old result", false, false, parent.oldResult},
			{"fresh result", false, true, true},
			{"stopped old result", true, false, false},
			{"stopped fresh result", true, true, parent.stoppedNewResult},
		} {
			t.Run(parent.name+"/"+test.name, func(t *testing.T) {
				data := successfulResult + "\n"
				if test.stopped {
					data += `{"type":"system","subtype":"task_notification","status":"stopped"}` + "\n"
				}
				field := ""
				if parent.value != "" {
					field = `,"parent_tool_use_id":` + parent.value
				}
				data += `{"type":"assistant"` + field + `,"message":{"id":"new"}}` + "\n"
				if test.fresh {
					data += successfulResult + "\n"
				}
				o := jsonlOutput{}
				writeChunks(t, &o, numberResultEvents(data))
				o.finish()
				if o.completed != test.want || (o.err == nil) != test.want || !test.want && o.result.Answer != "" || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
					t.Fatalf("stale completion or lost usage: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
				}
			})
		}
	}
}

func TestReplayedAssistantFromEarlierCallCannotAssessStoppedTask(t *testing.T) {
	const old = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old"}}`
	const fresh = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"fresh"}}`
	const stop = `{"type":"system","subtype":"task_notification","uuid":"stop-1","status":"stopped"}`
	for _, tc := range []struct {
		name, after string
		wantSuccess bool
	}{
		{"replay", old, false},
		{"updated historical root", `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"late text"}]}}`, false},
		{"fresh root", old + "\n" + fresh, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := jsonlOutput{}
			writeChunks(t, &first, old+"\n"+successfulResult+"\n")
			first.finish()
			if first.err != nil {
				t.Fatal(first.err)
			}
			o := jsonlOutput{progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
				progressBytes: first.progressBytes, progressEntries: first.progressEntries}
			writeChunks(t, &o, stop+"\n"+tc.after+"\n"+successfulResult+"\n")
			o.finish()
			if (o.err == nil) != tc.wantSuccess || o.completed != tc.wantSuccess {
				t.Fatalf("stopped task assessment: completed=%t error=%v", o.completed, o.err)
			}
		})
	}
}

func TestRootStreamMustStartAfterTaskStopToAssessIt(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root-A"}}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root-A","content":[{"type":"text","text":"assessment"}]}}`
	const stop = `{"type":"system","subtype":"task_notification","uuid":"stop-1","status":"stopped"}`
	for _, tc := range []struct {
		name, events string
		wantSuccess  bool
	}{
		{"start before stop", start + "\n" + stop + "\n" + full, false},
		{"replayed start after stop", start + "\n" + stop + "\n" + start + "\n" + full, false},
		{"start after stop", stop + "\n" + start + "\n" + full, true},
		{"full response without stream", stop + "\n" + full, true},
	} {
		for _, progress := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/progress=%t", tc.name, progress), func(t *testing.T) {
				o := jsonlOutput{}
				if progress {
					o.onProgress = func() {}
				}
				writeChunks(t, &o, tc.events+"\n"+successfulResult+"\n")
				o.finish()
				if (o.err == nil) != tc.wantSuccess || o.completed != tc.wantSuccess {
					t.Fatalf("stream/stop ordering: completed=%t stopped=%t error=%v", o.completed, o.stopped, o.err)
				}
			})
		}
	}
}

func TestNoCallbackResumedStreamStartCannotAssessLaterStop(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root-A"}}}`
	const stop = `{"type":"system","subtype":"task_notification","uuid":"stop-1","status":"stopped"}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root-A"}}`
	first := jsonlOutput{}
	writeChunks(t, &first, start+"\n"+successfulResult+"\n")
	first.finish()
	if first.err != nil || !first.completed {
		t.Fatalf("first call: completed=%t error=%v", first.completed, first.err)
	}
	second := jsonlOutput{progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	writeChunks(t, &second, stop+"\n"+full+"\n"+successfulResult+"\n")
	second.finish()
	if second.err == nil || second.completed || !second.stopped {
		t.Fatalf("resumed old stream assessed stop: completed=%t stopped=%t error=%v", second.completed, second.stopped, second.err)
	}
}

func TestUntrackedRootStreamStartCannotAssessStoppedTask(t *testing.T) {
	const stop = `{"type":"system","subtype":"task_notification","uuid":"stop-1","status":"stopped"}`
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root-A"}}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root-A"}}`
	o := jsonlOutput{progressEntryLimit: 1}
	writeChunks(t, &o, stop+"\n"+start+"\n"+full+"\n"+successfulResult+"\n")
	o.finish()
	if !errors.Is(o.err, aicli.ErrOutputLimit) || o.completed || !o.stopped {
		t.Fatalf("untracked start assessed stopped task: completed=%t stopped=%t error=%v", o.completed, o.stopped, o.err)
	}
}

func TestNewAssistantContentAfterResultRequiresAnotherResult(t *testing.T) {
	for _, tc := range []struct{ name, parent string }{
		{"root", `null`},
		{"helper", `"delegate"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := `{"type":"assistant","parent_tool_use_id":` + tc.parent + `,"message":{"id":"A","content":[{"type":"text","text":"A"}]}}`
			changed := `{"type":"assistant","parent_tool_use_id":` + tc.parent + `,"message":{"id":"A","content":[{"type":"text","text":"AB"}]}}`
			o := jsonlOutput{}
			writeChunks(t, &o, numberResultEvents(first+"\n"+successfulResult+"\n"+changed)+"\n")
			if o.err != nil || o.completed || o.result.Answer != "" {
				t.Fatalf("new content kept old result: completed=%t answer=%q error=%v", o.completed, o.result.Answer, o.err)
			}
			writeChunks(t, &o, strings.TrimSuffix(successfulResult, "}")+`,"result_index":1}`+"\n")
			o.finish()
			if o.err != nil || !o.completed || o.result.Answer != "final answer" {
				t.Fatalf("later result did not assess new content: completed=%t answer=%q error=%v", o.completed, o.result.Answer, o.err)
			}
		})
	}
}

func TestEarlierAssistantAddedContentInvalidatesCurrentResult(t *testing.T) {
	const old = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"before"}]}}`
	const changed = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"after"}]}}`
	first := jsonlOutput{}
	writeChunks(t, &first, old+"\n"+successfulResult+"\n")
	first.finish()
	if first.err != nil {
		t.Fatal(first.err)
	}
	second := jsonlOutput{progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	writeChunks(t, &second, successfulResult+"\n"+changed+"\n")
	second.finish()
	if second.err == nil || second.completed || second.result.Answer != "" {
		t.Fatalf("added historical content left stale answer: completed=%t answer=%q error=%v", second.completed, second.result.Answer, second.err)
	}
}

func TestEarlierAssistantEquivalentContentAreReplays(t *testing.T) {
	for _, tc := range []struct {
		name, first, later string
	}{
		{"empty list", `{"id":"old"}`, `{"id":"old","content":[]}`},
		{"empty object", `{"id":"old"}`, `{"id":"old","content":{}}`},
		{"empty string", `{"id":"old"}`, `{"id":"old","content":""}`},
		{"string to text block", `{"id":"old","content":"hello"}`, `{"id":"old","content":[{"type":"text","text":"hello"}]}`},
		{"text block to string", `{"id":"old","content":[{"type":"text","text":"hello"}]}`, `{"id":"old","content":"hello"}`},
		{"reordered keys", `{"id":"old","content":[{"type":"tool_use","id":"tool","input":{"a":1,"b":2}}]}`,
			`{"id":"old","content":[{"id":"tool","input":{"b":2,"a":1},"type":"tool_use"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := jsonlOutput{}
			writeChunks(t, &first, `{"type":"assistant","parent_tool_use_id":null,"message":`+tc.first+`}`+"\n"+successfulResult+"\n")
			first.finish()
			if first.err != nil {
				t.Fatal(first.err)
			}
			second := jsonlOutput{progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
				progressBytes: first.progressBytes, progressEntries: first.progressEntries}
			writeChunks(t, &second, successfulResult+"\n"+`{"type":"assistant","parent_tool_use_id":null,"message":`+tc.later+`}`+"\n")
			second.finish()
			if second.err != nil || !second.completed || second.result.Answer != "final answer" {
				t.Fatalf("semantic replay revoked result: completed=%t answer=%q error=%v", second.completed, second.result.Answer, second.err)
			}
		})
	}
}

func TestNonadjacentEarlierAssistantContentReplayIsIgnored(t *testing.T) {
	const firstFrame = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"A"}]}}`
	const laterFrame = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"AB"}]}}`
	first := jsonlOutput{}
	writeChunks(t, &first, firstFrame+"\n"+laterFrame+"\n"+successfulResult+"\n")
	first.finish()
	if first.err != nil {
		t.Fatal(first.err)
	}
	second := jsonlOutput{progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	writeChunks(t, &second, successfulResult+"\n"+firstFrame+"\n")
	second.finish()
	if second.err != nil || !second.completed || second.result.Answer != "final answer" {
		t.Fatalf("nonadjacent content replay revoked result: completed=%t answer=%q error=%v", second.completed, second.result.Answer, second.err)
	}
}

func TestEarlierAssistantIDMakesResumedStreamStartAReplay(t *testing.T) {
	const old = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old"}}`
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"old"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"old output"}}}`
	const stop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_stop"}}`
	first := jsonlOutput{}
	writeChunks(t, &first, old+"\n"+successfulResult+"\n")
	first.finish()
	if first.err != nil {
		t.Fatal(first.err)
	}
	second := jsonlOutput{progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	writeChunks(t, &second, successfulResult+"\n"+start+"\n"+delta+"\n"+stop+"\n")
	second.finish()
	if second.err != nil || !second.completed || second.result.Answer != "final answer" {
		t.Fatalf("resumed stream replay revoked result: completed=%t answer=%q error=%v", second.completed, second.result.Answer, second.err)
	}
}

func TestFirstFullAssistantAfterResumedStreamOnlyResultIsReplay(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"old"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"old output"}}}`
	const stop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_stop"}}`
	const replay = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"old output"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}}`
	const changed = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"new output"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}}`
	for _, callback := range []bool{false, true} {
		t.Run(fmt.Sprintf("progress callback=%t", callback), func(t *testing.T) {
			progress, want := 0, 0
			first := jsonlOutput{}
			if callback {
				first.onProgress = func() { progress++ }
				want = 1
			}
			writeChunks(t, &first, start+"\n"+delta+"\n"+stop+"\n"+successfulResult+"\n")
			first.finish()
			if first.err != nil || !first.completed || progress != want {
				t.Fatalf("stream-only first call: completed=%t progress=%d error=%v", first.completed, progress, first.err)
			}
			second := jsonlOutput{onProgress: first.onProgress, baseline: first.cumulative, progressSeen: first.progressSeen,
				assistantHistory: first.assistantHistory, progressBytes: first.progressBytes, progressEntries: first.progressEntries}
			writeChunks(t, &second, successfulResult+"\n"+replay+"\n")
			if second.err != nil || !second.completed || second.result.Answer != "final answer" || progress != want {
				t.Fatalf("first full replay renewed progress or revoked result: completed=%t progress=%d answer=%q error=%v",
					second.completed, progress, second.result.Answer, second.err)
			}
			writeChunks(t, &second, changed+"\n")
			if second.err != nil || second.completed || second.result.Answer != "" || progress != 2*want {
				t.Fatalf("changed full content was lost: completed=%t progress=%d answer=%q error=%v",
					second.completed, progress, second.result.Answer, second.err)
			}
		})
	}
}

func TestFirstFullAssistantAfterSameCallResultCompletesStream(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"old"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"old output"}}}`
	const stop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_stop"}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"old output"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}}`
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	writeChunks(t, &o, start+"\n"+delta+"\n"+stop+"\n"+successfulResult+"\n"+full+"\n")
	o.finish()
	if o.err != nil || !o.completed || o.result.Answer != "final answer" || progress != 1 {
		t.Fatalf("full stream completion after result: completed=%t progress=%d answer=%q error=%v",
			o.completed, progress, o.result.Answer, o.err)
	}
}

func TestFirstFullAssistantWithUsageBeyondResultNeedsCoveringUsage(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"current"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}}`
	const stop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_stop"}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"current","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":12}}}`
	data := start + "\n" + delta + "\n" + stop + "\n" + successfulResult + "\n" + full + "\n"
	for _, recovered := range []bool{false, true} {
		o := jsonlOutput{}
		writeChunks(t, &o, numberResultEvents(data))
		if !o.completed || o.result.Answer != "final answer" || o.result.UsageComplete || o.result.Tokens.Output != 12 {
			t.Fatalf("result hid excess assistant usage: result=%+v completed=%t", o.result, o.completed)
		}
		if recovered {
			next := strings.ReplaceAll(successfulResult, `"outputTokens":11`, `"outputTokens":12`)
			writeChunks(t, &o, strings.TrimSuffix(next, "}")+`,"result_index":1}`+"\n")
		}
		o.finish()
		if o.err != nil || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 12}) || o.result.UsageComplete != recovered {
			t.Fatalf("recovered=%t result=%+v error=%v", recovered, o.result, o.err)
		}
	}
}

func TestCumulativeResultCannotHideInvalidAssistantCounterDelta(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"current"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}}`
	const result = `{"type":"result","subtype":"success","result":"answer","modelUsage":{"main":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":11}}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"current","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":15,"output_tokens":12}}}`
	o := jsonlOutput{baseline: usage.Tokens{Input: 15, Output: 11}}
	writeChunks(t, &o, numberResultEvents(start+"\n"+delta+"\n"+result+"\n"+full+"\n"+result+"\n"))
	o.finish()
	if o.err != nil || o.result.UsageComplete || o.assistantTotal != (usage.Tokens{Input: 15, CachedInput: 15, Output: 12}) {
		t.Fatalf("cumulative result ignored assistant totals: result=%+v total=%+v error=%v", o.result, o.assistantTotal, o.err)
	}
}

func TestImpossibleAssistantCounterFloorPersistsUntilNewConversation(t *testing.T) {
	first := jsonlOutput{}
	writeChunks(t, &first, assistantUsageEvent("old", math.MaxInt64-20, 0, 0, 1)+"\n"+
		assistantUsageEvent("old", 1, 0, 0, math.MaxInt64-2)+"\n")
	if snapshot := first.assistantHistory["old"]; !snapshot.hasTokens || snapshot.tokens.Valid() {
		t.Fatalf("impossible counter floor was not retained: %+v", snapshot)
	}
	var session Session
	session.assistantHistory = first.assistantHistory
	second := jsonlOutput{assistantHistory: session.assistantHistory}
	writeChunks(t, &second, successfulResult+"\n")
	second.finish()
	if second.err != nil || second.result.Answer != "final answer" || second.result.UsageComplete {
		t.Fatalf("resumed call certified impossible usage: %+v, %v", second.result, second.err)
	}
	session.NewConversation()
	third := jsonlOutput{assistantHistory: session.assistantHistory}
	writeChunks(t, &third, successfulResult+"\n")
	third.finish()
	if third.err != nil || !third.result.UsageComplete {
		t.Fatalf("new conversation retained impossible usage: %+v, %v", third.result, third.err)
	}
}

func TestFirstHistoricalCountersBeyondCumulativeResultNeedCoveringUsage(t *testing.T) {
	const old = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old"}}`
	const late = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":12}}}`
	const resumedResult = `{"type":"result","subtype":"success","result":"final answer","modelUsage":{"claude-main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":11}}}`
	first := jsonlOutput{}
	writeChunks(t, &first, old+"\n"+successfulResult+"\n")
	first.finish()
	if first.err != nil || !first.result.UsageComplete {
		t.Fatalf("first cumulative result: %+v, %v", first.result, first.err)
	}
	second := jsonlOutput{baseline: first.cumulative, progressSeen: first.progressSeen,
		assistantHistory: first.assistantHistory}
	writeChunks(t, &second, resumedResult+"\n"+late+"\n")
	second.finish()
	if second.err != nil || !second.completed || second.result.Answer != "final answer" || second.result.UsageComplete || second.result.Tokens.Output < 1 {
		t.Fatalf("late historical counters retained exact usage: %+v completed=%t error=%v", second.result, second.completed, second.err)
	}
}

func TestTwoLateStreamedMessagesCannotExceedCumulativeResult(t *testing.T) {
	o := jsonlOutput{}
	var events string
	for _, id := range []string{"one", "two"} {
		events += fmt.Sprintf(`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":%q}}}`, id) + "\n"
		events += `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"part"}}}` + "\n"
		events += `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_stop"}}` + "\n"
	}
	events += successfulResult + "\n"
	for _, id := range []string{"one", "two"} {
		events += assistantUsageEvent(id, 1, 0, 0, 8) + "\n"
	}
	writeChunks(t, &o, events)
	o.finish()
	if o.err != nil || !o.completed || o.result.Answer != "final answer" || o.result.UsageComplete || o.result.Tokens.Output < 16 {
		t.Fatalf("separate message usage escaped cumulative check: %+v completed=%t error=%v", o.result, o.completed, o.err)
	}
}

func TestEarlierAssistantCounterIncreaseInvalidatesCurrentResult(t *testing.T) {
	first := jsonlOutput{}
	writeChunks(t, &first, assistantUsageEvent("old", 3, 5, 7, 2)+"\n"+successfulResult+"\n")
	first.finish()
	if first.err != nil || !first.hasCumulative {
		t.Fatalf("first usage baseline: error=%v cumulative=%t", first.err, first.hasCumulative)
	}
	second := jsonlOutput{baseline: first.cumulative, progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	const cumulative = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":14}}}`
	writeChunks(t, &second, cumulative+"\n"+assistantUsageEvent("old", 3, 5, 7, 5)+"\n")
	second.finish()
	if second.err == nil || second.completed || second.result.Tokens.Output < 3 || second.result.UsageComplete {
		t.Fatalf("late counter increase left stale result or lost known usage: result=%+v error=%v", second.result, second.err)
	}
}

func TestExactEarlierAssistantReplayAfterCurrentResultIsIgnored(t *testing.T) {
	old := assistantUsageEvent("old", 3, 5, 7, 2)
	first := jsonlOutput{}
	writeChunks(t, &first, old+"\n"+successfulResult+"\n")
	first.finish()
	if first.err != nil || !first.hasCumulative {
		t.Fatalf("first usage baseline: error=%v cumulative=%t", first.err, first.hasCumulative)
	}
	second := jsonlOutput{baseline: first.cumulative, progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	const cumulative = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":14}}}`
	writeChunks(t, &second, cumulative+"\n"+old+"\n")
	second.finish()
	if second.err != nil || !second.completed || second.result.Answer != "done" ||
		second.result.Tokens != (usage.Tokens{Output: 3}) || !second.result.UsageComplete {
		t.Fatalf("exact replay revoked or inflated current result: result=%+v completed=%t error=%v", second.result, second.completed, second.err)
	}
}

func TestEarlierAssistantHigherCountersUseOnlyIncrease(t *testing.T) {
	first := jsonlOutput{}
	writeChunks(t, &first, assistantUsageEvent("old", 3, 5, 7, 2)+"\n"+successfulResult+"\n")
	first.finish()
	if first.err != nil || !first.hasCumulative {
		t.Fatalf("first usage baseline: error=%v cumulative=%t", first.err, first.hasCumulative)
	}
	second := jsonlOutput{baseline: first.cumulative, progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	const cumulative = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":14}}}`
	writeChunks(t, &second, assistantUsageEvent("old", 3, 5, 7, 5)+"\n"+cumulative+"\n")
	second.finish()
	if second.err != nil || !second.completed || second.result.Tokens != (usage.Tokens{Output: 3}) || !second.result.UsageComplete {
		t.Fatalf("historical counters inflated current usage: result=%+v completed=%t error=%v", second.result, second.completed, second.err)
	}
}

func TestHistoricalAssistantWithoutCountersNeedsAggregateForExactUsage(t *testing.T) {
	first := jsonlOutput{}
	const old = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old"}}`
	const initial = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":100,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`
	writeChunks(t, &first, old+"\n"+initial+"\n")
	first.finish()
	if first.err != nil || !first.hasCumulative {
		t.Fatalf("first call lacked a known baseline: cumulative=%t error=%v", first.hasCumulative, first.err)
	}
	second := jsonlOutput{baseline: first.cumulative, progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	const fallback = `{"type":"result","subtype":"success","result":"done","usage":{"input_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}`
	writeChunks(t, &second, assistantUsageEvent("old", 100, 0, 0, 0)+"\n"+
		assistantUsageEvent("old", 105, 0, 0, 0)+"\n"+fallback+"\n")
	second.finish()
	if second.err != nil || second.result.Tokens != (usage.Tokens{Input: 20}) ||
		second.assistantTokens != (usage.Tokens{Input: 5}) || second.result.UsageComplete || second.hasCumulative {
		t.Fatalf("historical counters misattributed: assistant=%+v result=%+v cumulative=%t error=%v",
			second.assistantTokens, second.result, second.hasCumulative, second.err)
	}
}

func TestLateFirstHistoricalAssistantCountersDoNotRevokeCoveredResult(t *testing.T) {
	first := jsonlOutput{}
	const old = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old"}}`
	const initial = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":100,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`
	writeChunks(t, &first, old+"\n"+initial+"\n")
	first.finish()
	if first.err != nil || !first.hasCumulative {
		t.Fatalf("first call lacked a known baseline: cumulative=%t error=%v", first.hasCumulative, first.err)
	}
	second := jsonlOutput{baseline: first.cumulative, progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	const cumulative = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":120,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`
	writeChunks(t, &second, cumulative+"\n"+assistantUsageEvent("old", 100, 0, 0, 0)+"\n")
	second.finish()
	if second.err != nil || !second.completed || second.result.Answer != "done" ||
		second.result.Tokens != (usage.Tokens{Input: 20}) || !second.result.UsageComplete || !second.hasCumulative {
		t.Fatalf("late historical floor revoked covered result: result=%+v completed=%t cumulative=%t error=%v",
			second.result, second.completed, second.hasCumulative, second.err)
	}
}

func TestAssistantReplayHistoryLimitFailsClosed(t *testing.T) {
	o := jsonlOutput{progressEntryLimit: 1}
	writeChunks(t, &o, `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"one"}}`+"\n"+
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"two"}}`+"\n"+successfulResult+"\n")
	o.finish()
	if !errors.Is(o.err, aicli.ErrOutputLimit) || o.completed || o.result.Answer != "" {
		t.Fatalf("assistant history limit did not stop result: completed=%t answer=%q error=%v", o.completed, o.result.Answer, o.err)
	}
}

func TestForeignAndAmbientAssistantsCannotChangeResultOrStopState(t *testing.T) {
	const result = `{"type":"result","session_id":"native-id","subtype":"success","result":"done"}`
	const stop = `{"type":"system","subtype":"task_notification","session_id":"native-id","uuid":"stop-1","status":"stopped"}`
	for _, tc := range []struct{ name, field string }{
		{"foreign", `"session_id":"foreign"`},
		{"ambient", `"session_id":"native-id","ambient":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assistant := `{"type":"assistant",` + tc.field + `,"parent_tool_use_id":null,"message":{"id":"unrelated"}}`
			before := jsonlOutput{sessionID: "native-id"}
			writeChunks(t, &before, `{"type":"system","subtype":"init","session_id":"native-id"}`+"\n"+result+"\n"+assistant+"\n")
			before.finish()
			if before.err != nil || !before.completed || before.result.Answer != "done" {
				t.Fatalf("unrelated assistant revoked result: completed=%t answer=%q error=%v", before.completed, before.result.Answer, before.err)
			}
			after := jsonlOutput{sessionID: "native-id"}
			writeChunks(t, &after, `{"type":"system","subtype":"init","session_id":"native-id"}`+"\n"+stop+"\n"+assistant+"\n"+result+"\n")
			after.finish()
			if after.err == nil || after.completed || !after.stopped {
				t.Fatalf("unrelated assistant assessed stop: completed=%t stopped=%t error=%v", after.completed, after.stopped, after.err)
			}
		})
	}
}

func TestCurrentSessionHelperAssistantStillContributesUsage(t *testing.T) {
	o := jsonlOutput{sessionID: "native-id"}
	helper := strings.Replace(assistantUsageEvent("helper", 1, 0, 0, 2),
		`"parent_tool_use_id":null`, `"parent_tool_use_id":"delegate","session_id":"native-id","agent_id":"agent-1"`, 1)
	const result = `{"type":"result","session_id":"native-id","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	writeChunks(t, &o, `{"type":"system","subtype":"init","session_id":"native-id"}`+"\n"+helper+"\n"+result+"\n")
	o.finish()
	if o.err != nil || o.assistantTokens != (usage.Tokens{Input: 1, Output: 2}) ||
		o.result.Tokens != (usage.Tokens{Input: 1, Output: 2}) || !o.result.UsageComplete {
		t.Fatalf("current-session helper usage lost: assistant=%+v result=%+v error=%v", o.assistantTokens, o.result, o.err)
	}
}

func TestNewRootStreamCannotPreserveStaleResult(t *testing.T) {
	start := func(id, fields string) string {
		return fmt.Sprintf(`{"type":"stream_event"%s,"event":{"type":"message_start","message":{"id":%q}}}`, fields, id)
	}
	missingID := func(fields string) string {
		return fmt.Sprintf(`{"type":"stream_event"%s,"event":{"type":"message_start","message":{}}}`, fields)
	}
	const delta = `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"new answer"}}}`
	const stop = `{"type":"stream_event","event":{"type":"message_stop"}}`
	const helperDelta = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"tool","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"helper answer"}}}`
	const helperStop = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"tool","event":{"type":"message_stop"}}`
	for _, tc := range []struct {
		name, before, after, resumeKey string
		want                           bool
	}{
		{"fresh root", "", start("new", `,"parent_tool_use_id":null`) + "\n" + delta + "\n" + stop, "", false},
		{"empty fresh root", "", start("new", `,"parent_tool_use_id":null`) + "\n" + stop, "", false},
		{"fresh untagged", "", start("new", "") + "\n" + delta + "\n" + stop, "", false},
		{"unknown parent", "", start("new", `,"parent_tool_use_id":0`) + "\n" + delta + "\n" + stop, "", false},
		{"missing ID root", "", missingID(`,"parent_tool_use_id":null`), "", false},
		{"missing ID untagged", "", missingID(""), "", false},
		{"missing ID unknown parent", "", missingID(`,"parent_tool_use_id":0`), "", false},
		{"missing ID then fresh result", "", missingID(`,"parent_tool_use_id":null`) + "\n" + successfulResult, "", true},
		{"missing ID agent helper", "", missingID(`,"agent_id":"worker"`), "", true},
		{"missing ID parent-tagged helper", "", missingID(`,"parent_tool_use_id":"tool"`), "", true},
		{"helper", "", start("helper", `,"agent_id":"worker","parent_tool_use_id":"tool"`) + "\n" + helperDelta + "\n" + helperStop, "", true},
		{"parent-tagged helper", "", start("helper", `,"parent_tool_use_id":"tool"`) + "\n" + strings.ReplaceAll(helperDelta, `"agent_id":"worker",`, "") + "\n" + strings.ReplaceAll(helperStop, `"agent_id":"worker",`, ""), "", true},
		{"stream replay", start("old", `,"parent_tool_use_id":null`) + "\n" + delta + "\n" + stop, start("old", `,"parent_tool_use_id":null`) + "\n" + delta + "\n" + stop, "", true},
		{"assistant replay", `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old"}}`, start("old", `,"parent_tool_use_id":null`) + "\n" + delta + "\n" + stop, "", true},
		{"resumed replay", "", start("old", `,"parent_tool_use_id":null`) + "\n" + delta + "\n" + stop, "assistant:old", true},
		{"resumed empty replay", "", start("old", `,"parent_tool_use_id":null`) + "\n" + stop, "stream_start:old", true},
		{"fresh result", "", start("new", `,"parent_tool_use_id":null`) + "\n" + delta + "\n" + stop + "\n" + successfulResult, "", true},
	} {
		for _, progress := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/progress=%t", tc.name, progress), func(t *testing.T) {
				data := tc.before
				if data != "" {
					data += "\n"
				}
				data += successfulResult + "\n" + tc.after + "\n"
				o := jsonlOutput{}
				if tc.resumeKey != "" {
					o.progressSeen = map[string]bool{tc.resumeKey: true}
				}
				if progress {
					o.onProgress = func() {}
				} else {
					// Assistant IDs now occupy bounded replay history without a callback.
					o.progressByteLimit, o.progressEntryLimit = 128, 4
				}
				writeChunks(t, &o, numberResultEvents(data))
				o.finish()
				if errors.Is(o.err, aicli.ErrOutputLimit) || (o.err == nil) != tc.want || o.completed != tc.want || (o.result.Answer == "final answer") != tc.want || o.result.UsageComplete != tc.want || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
					t.Fatalf("stale stream completion: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
				}
			})
		}
	}
}

func TestOutputStoppedTaskNeedsNewRootResponseAndResult(t *testing.T) {
	const old = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old"}}`
	const fresh = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"new"}}`
	const pending = `{"type":"system","subtype":"background_tasks_changed","uuid":"pending-list","tasks":[{"task_id":"tests"}]}`
	const clear = `{"type":"system","subtype":"background_tasks_changed","uuid":"cleared-list","tasks":[]}`
	const stop = `{"type":"system","subtype":"task_notification","status":"stopped","summary":"Background command tests was stopped"}`
	notice, _ := json.Marshal(`<task-notification><task-id>tests</task-id><status>killed</status><summary>Tests stopped</summary></task-notification>`)
	synthetic := `{"type":"user","isSynthetic":true,"message":{"content":` + string(notice) + `}}`
	noticeEvent := func(summary string) string {
		value, _ := json.Marshal("<task-notification><status>killed</status><summary>" + summary + "</summary></task-notification>")
		return `{"type":"user","isSynthetic":true,"message":{"content":` + string(value) + `}}`
	}
	const pressureSummary = `Background command "tests" was stopped because the system is running low on memory`
	pressureNotice, _ := json.Marshal(`<task-notification><task-id>worker</task-id><status>killed</status><summary>Background command &quot;tests&quot; was stopped because the system is running low on memory</summary></task-notification>`)
	pressureSystem := fmt.Sprintf(`{"type":"system","subtype":"task_notification","status":"stopped","summary":%q}`, pressureSummary)
	pressureSynthetic := `{"type":"user","isSynthetic":true,"message":{"content":` + string(pressureNotice) + `}}`
	later := strings.Replace(successfulResult, `"outputTokens":11`, `"outputTokens":22`, 1)
	for _, tc := range []struct {
		name, after string
		fails       bool
		failure     string
		output      int64
	}{
		{"wind down then held waiting result", pending + "\n" + stop + "\n" + clear + "\n" + successfulResult, true, "", 11},
		{"stop after final", stop, true, "", 11},
		{"new result without root response", stop + "\n" + successfulResult, true, "", 11},
		{"repeated old root ID", stop + "\n" + old + "\n" + successfulResult, true, "", 11},
		{"memory pressure remains fatal after new root", strings.Replace(stop, `"summary":"Background command tests was stopped"`, `"summary":"Background command \"tests\" was stopped because the system is running low on memory"`, 1) + "\n" + fresh + "\n" + successfulResult, true, "provider memory pressure", 11},
		{"ambient stop after final", strings.Replace(stop, `"status":"stopped"`, `"status":"stopped","ambient":true`, 1), false, "", 11},
		{"synthetic killed string", synthetic + "\n" + successfulResult, true, "", 11},
		{"synthetic stopped block", `{"type":"user","isSynthetic":true,"message":{"content":[{"type":"text","text":` + strings.Replace(string(notice), "killed", "stopped", 1) + `}]}}` + "\n" + successfulResult, true, "", 11},
		{"synthetic stop assessed", synthetic + "\n" + fresh + "\n" + successfulResult, false, "", 11},
		{"raw ampersand notice", noticeEvent(`Background command "go test && go vet" was stopped`), true, "", 11},
		{"raw less-than notice", noticeEvent(`Background command "grep x <file" was stopped`), true, "", 11},
		{"entity-escaped notice", noticeEvent(`Background command &quot;go test &amp;&amp; go vet&quot; was stopped`), true, "", 11},
		{"killed system event", strings.Replace(stop, `"status":"stopped"`, `"status":"killed"`, 1), true, "", 11},
		{"raw memory-pressure summary", noticeEvent(`Background command "go test && go vet" was stopped because the system is running low on memory`) + "\n" + fresh + "\n" + successfulResult, true, "provider memory pressure", 11},
		{"literal closing summary tag in command", noticeEvent(`Background command "echo </summary> && go vet" was stopped because the system is running low on memory`) + "\n" + fresh + "\n" + successfulResult, true, "provider memory pressure", 11},
		{"ordinary user quote", strings.Replace(synthetic, `"isSynthetic":true`, `"isSynthetic":false`, 1), false, "", 11},
		{"ordinary failed command assessed", strings.Replace(stop, `"status":"stopped"`, `"status":"failed"`, 1), false, "", 11},
		{"memory pressure/native system", pressureSystem + "\n" + later, true, "provider memory pressure", 22},
		{"memory pressure/native synthetic string", pressureSynthetic + "\n" + later, true, "provider memory pressure", 22},
		{"memory pressure/native synthetic text block", `{"type":"user","isSynthetic":true,"message":{"content":[{"type":"text","text":` + string(pressureNotice) + `}]}}` + "\n" + later, true, "provider memory pressure", 22},
		{"memory pressure/ordinary user quote", strings.Replace(pressureSynthetic, `"isSynthetic":true`, `"isSynthetic":false`, 1) + "\n" + later, false, "", 22},
		{"memory pressure/ambient server stopped", `{"type":"system","subtype":"task_notification","status":"stopped","ambient":true,"summary":"Background server deliberately stopped"}` + "\n" + later, false, "", 22},
		{"memory pressure/ordinary completed task", strings.Replace(pressureSystem, `"stopped"`, `"completed"`, 1) + "\n" + later, false, "", 22},
		{"memory pressure/unrelated assistant summary", strings.Replace(pressureSystem, `"type":"system"`, `"type":"assistant"`, 1) + "\n" + later, false, "", 22},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			data := numberResultEvents(old+"\n"+successfulResult+"\n"+tc.after) + "\n"
			writeChunks(t, &o, data)
			o.finish()
			if tc.failure != "" && (o.err == nil || !strings.Contains(o.err.Error(), tc.failure)) {
				t.Fatalf("lost native failure %q: %v", tc.failure, o.err)
			}
			if (o.err != nil) != tc.fails || o.completed == tc.fails || o.result.UsageComplete == tc.fails || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: tc.output}) {
				t.Fatalf("result=%+v completed=%v error=%v", o.result, o.completed, o.err)
			}
		})
	}
}

func TestIdentifiedTaskStopReplayKeepsAssessedResult(t *testing.T) {
	const fresh = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"fresh"}}`
	for _, tc := range []struct {
		name, first, replay string
	}{
		{
			"same native UUID",
			`{"type":"system","subtype":"task_notification","uuid":"stop-1","task_id":"helper","run_id":"run-1","status":"stopped","summary":"helper stopped"}`,
			`{"type":"system","subtype":"task_notification","uuid":"stop-1","task_id":"helper","run_id":"run-1","status":"stopped","summary":"helper stopped"}`,
		},
		{
			"same run with new UUID",
			`{"type":"system","subtype":"task_notification","uuid":"stop-1","task_id":"helper","run_id":"run-1","status":"killed","summary":"helper killed"}`,
			`{"type":"system","subtype":"task_notification","uuid":"stop-2","task_id":"helper","run_id":"run-1","status":"killed","summary":"helper killed"}` + "\n" +
				`{"type":"user","isSynthetic":true,"uuid":"stop-2","message":{"content":"<task-notification><task-id>helper</task-id><status>killed</status><summary>helper killed</summary></task-notification>"}}`,
		},
		{
			"UUID without task identity",
			`{"type":"system","subtype":"task_notification","uuid":"stop-1","status":"stopped","summary":"helper stopped"}`,
			`{"type":"system","subtype":"task_notification","uuid":"stop-1","status":"stopped","summary":"helper stopped"}`,
		},
		{
			"synthetic string UUID",
			`{"type":"user","isSynthetic":true,"uuid":"stop-1","message":{"content":"<task-notification><task-id>helper</task-id><status>stopped</status><summary>helper stopped</summary></task-notification>"}}`,
			`{"type":"user","isSynthetic":true,"uuid":"stop-1","message":{"content":"<task-notification><task-id>helper</task-id><status>stopped</status><summary>helper stopped</summary></task-notification>"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.first+"\n"+fresh+"\n"+successfulResult+"\n"+tc.replay+"\n")
			o.finish()
			if o.err != nil || !o.completed || o.result.Answer != "final answer" {
				t.Fatalf("replayed stop revoked assessed result: completed=%t answer=%q error=%v", o.completed, o.result.Answer, o.err)
			}
			stops := 0
			for _, diagnostic := range o.result.Diagnostics {
				if diagnostic.Code == "task_stopped" {
					stops++
				}
			}
			if stops != 1 {
				t.Fatalf("recorded %d stop diagnostics, want one", stops)
			}
		})
	}
}

func TestSyntheticStopAndTaggedNativeStopShareUUID(t *testing.T) {
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const synthetic = `{"type":"user","isSynthetic":true,"uuid":"stop-1","message":{"content":"<task-notification><task-id>helper</task-id><status>stopped</status><summary>helper stopped</summary></task-notification>"}}`
	const native = `{"type":"system","subtype":"task_notification","uuid":"stop-1","task_id":"helper","run_id":"run-1","status":"stopped","summary":"helper stopped"}`
	const replay = `{"type":"system","subtype":"task_notification","uuid":"stop-2","task_id":"helper","run_id":"run-1","status":"stopped","summary":"helper stopped"}`
	const fresh = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"fresh"}}`
	o := jsonlOutput{}
	writeChunks(t, &o, started+"\n"+synthetic+"\n"+native+"\n")
	if o.err != nil || o.pending != 0 || !o.progressSeen["task_finish:"+taskRunKey("helper", "run-1")] {
		t.Fatalf("native stop did not finish tagged run: pending=%d error=%v", o.pending, o.err)
	}
	writeChunks(t, &o, fresh+"\n"+successfulResult+"\n"+replay+"\n")
	o.finish()
	if o.err != nil || !o.completed || o.result.Answer != "final answer" {
		t.Fatalf("replayed stop revoked assessed result: completed=%t answer=%q error=%v", o.completed, o.result.Answer, o.err)
	}
	stops := 0
	for _, diagnostic := range o.result.Diagnostics {
		if diagnostic.Code == "task_stopped" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("recorded %d stop diagnostics, want one", stops)
	}
}

func TestSyntheticStopAndTaggedNativePressureUpgradeShareUUID(t *testing.T) {
	const started = `{"type":"system","subtype":"task_started","task_id":"helper","run_id":"run-1"}`
	const synthetic = `{"type":"user","isSynthetic":true,"uuid":"stop-1","message":{"content":"<task-notification><task-id>helper</task-id><status>stopped</status><summary>helper stopped</summary></task-notification>"}}`
	const pressure = `{"type":"system","subtype":"task_notification","uuid":"stop-1","task_id":"helper","run_id":"run-1","status":"stopped","summary":"Background command \"helper\" was stopped because the system is running low on memory"}`
	o := jsonlOutput{}
	writeChunks(t, &o, started+"\n"+synthetic+"\n"+pressure+"\n")
	o.finish()
	if o.err == nil || !strings.Contains(o.err.Error(), "provider memory pressure") || o.pending != 0 || o.completed {
		t.Fatalf("pressure upgrade was lost: pending=%d completed=%t error=%v", o.pending, o.completed, o.err)
	}
}

func TestSyntheticArrayDuplicateStopRecordsOneDiagnostic(t *testing.T) {
	const notice = `<task-notification><task-id>helper</task-id><status>stopped</status><summary>helper stopped</summary></task-notification>`
	const event = `{"type":"user","isSynthetic":true,"uuid":"stop-array","message":{"content":[{"type":"text","text":"` + notice + `"},{"type":"text","text":"` + notice + `"}]}}`
	const fresh = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"fresh"}}`
	o := jsonlOutput{}
	writeChunks(t, &o, event+"\n"+fresh+"\n"+successfulResult+"\n"+event+"\n")
	o.finish()
	if o.err != nil || !o.completed {
		t.Fatalf("duplicate stop revoked assessed result: completed=%t error=%v", o.completed, o.err)
	}
	stops := 0
	for _, diagnostic := range o.result.Diagnostics {
		if diagnostic.Code == "task_stopped" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("recorded %d stop diagnostics, want one", stops)
	}
}

func TestTaskStopPressureUpgradeRemainsFatal(t *testing.T) {
	const ordinary = `{"type":"system","subtype":"task_notification","uuid":"stop-1","task_id":"helper","run_id":"run-1","status":"stopped","summary":"helper stopped"}`
	const pressure = `{"type":"system","subtype":"task_notification","uuid":"stop-1","task_id":"helper","run_id":"run-1","status":"stopped","summary":"Background command \"helper\" was stopped because the system is running low on memory"}`
	const fresh = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"fresh"}}`
	o := jsonlOutput{}
	writeChunks(t, &o, ordinary+"\n"+fresh+"\n"+successfulResult+"\n"+pressure+"\n")
	o.finish()
	if o.err == nil || !strings.Contains(o.err.Error(), "provider memory pressure") || o.completed {
		t.Fatalf("pressure escalation after ordinary stop was lost: completed=%t error=%v", o.completed, o.err)
	}
}

func TestOutputTerminalReasonRequiresCompletion(t *testing.T) {
	for _, reason := range []string{`"completed"`, `"tool_deferred"`, `"aborted_tools"`, `"aborted_streaming"`, `"background_requested"`, `"hook_stopped"`, `"stop_hook_prevented"`, `""`, `null`, `42`} {
		t.Run(reason, func(t *testing.T) {
			terminal := strings.TrimSuffix(successfulResult, "}") + `,"terminal_reason":` + reason + `}`
			for _, recover := range []bool{false, true} {
				o := jsonlOutput{}
				data := successfulResult + "\n" + terminal
				if recover {
					data += "\n" + strings.TrimSuffix(successfulResult, "}") + `,"terminal_reason":"completed"}`
				}
				_, _ = o.Write([]byte(numberResultEvents(data)))
				o.finish()
				fails := !recover && reason != `"completed"`
				if (o.err != nil) != fails || o.result.UsageComplete == fails || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
					t.Fatalf("recover=%v result=%+v error=%v", recover, o.result, o.err)
				}
			}
		})
	}
}

func TestOutputUsageGraceStartsOnlyOnce(t *testing.T) {
	const rejected = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`
	const paid = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","isUsingOverage":true}}`
	const pressure = `{"type":"system","subtype":"task_notification","status":"stopped","summary":"Background command \"tests\" was stopped because the system is running low on memory"}`
	for _, events := range []string{rejected + "\n" + rejected, pressure + "\n" + pressure, rejected + "\n" + pressure, pressure + "\n" + rejected} {
		calls := 0
		o := jsonlOutput{fatal: func() { calls++ }}
		_, _ = o.Write([]byte(paid + "\n"))
		if calls != 0 {
			t.Fatalf("usable overage started cancellation grace before %q", events)
		}
		_, _ = o.Write([]byte(events + "\n"))
		if calls != 1 || o.err != nil {
			t.Fatalf("events=%q grace calls=%d error=%v", events, calls, o.err)
		}
		_, _ = o.Write([]byte(successfulResult + "\n"))
		o.finish()
		if o.err == nil || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
			t.Fatalf("events=%q late result usage lost: %+v error=%v", events, o.result, o.err)
		}
	}
}

func TestRunFatalGraceStopsWatchdogAndKeepsObservedUsage(t *testing.T) {
	for _, scenario := range []struct{ name, event, failure string }{
		{"quota", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`, "provider usage limit reached"},
		{"pressure", `{"type":"system","subtype":"task_notification","status":"stopped","summary":"Background command \"tests\" was stopped because the system is running low on memory"}`, "provider memory pressure"},
	} {
		for _, resultFollows := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/result=%t", scenario.name, resultFollows), func(t *testing.T) {
				script := "cat >/dev/null\ncat <<'EVENTS'\n" + assistantUsageEvent("root", 3, 5, 7, 2) + "\n" + `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"worker"}]}` + "\n" + scenario.event + "\nEVENTS\n"
				if resultFollows {
					script += "cat <<'RESULT'\n" + successfulResult + "\nRESULT\n"
				}
				script += "exec sleep 30\n"
				fakeExecutable(t, "claude", script)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				start := time.Now()
				result, err := Run(ctx, t.TempDir(), "task", false, "", config.ClaudeConfig{})
				want := usage.Tokens{Input: 15, CachedInput: 7, Output: 2}
				if resultFollows {
					want.Output = 11
				}
				if err == nil || !strings.Contains(err.Error(), scenario.failure) || ctx.Err() != nil || result.Tokens != want || result.Answer != "" || result.UsageComplete || time.Since(start) > 5*time.Second {
					t.Fatalf("result=%+v elapsed=%v error=%v parent=%v", result, time.Since(start), err, ctx.Err())
				}
			})
		}
	}
}
