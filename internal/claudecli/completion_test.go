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
				if o.completed != test.want || (o.err == nil) != test.want || !test.want && o.result.Answer != "" || o.result.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}) {
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
	const helperStart = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"root-A"}}}`
	const contradictory = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":null,"message":{"id":"root-A"}}`
	const missingParent = `{"type":"assistant","agent_id":"worker","message":{"id":"root-A"}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root-A","content":[{"type":"text","text":"assessment"}]}}`
	const stop = `{"type":"system","subtype":"task_notification","uuid":"stop-1","status":"stopped"}`
	for _, tc := range []struct {
		name, events string
		wantSuccess  bool
	}{
		{"start before stop", start + "\n" + stop + "\n" + full, false},
		{"replayed start after stop", start + "\n" + stop + "\n" + start + "\n" + full, false},
		{"start after stop", stop + "\n" + start + "\n" + full, true},
		{"root ID shared with helper", helperStart + "\n" + stop + "\n" + start + "\n" + full, true},
		{"contradictory helper cannot assess stop", stop + "\n" + contradictory, false},
		{"helper without parent cannot assess stop", stop + "\n" + missingParent, false},
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

func TestUnsupportedLateRootContentRequiresNewResult(t *testing.T) {
	o := jsonlOutput{}
	writeChunks(t, &o, successfulResult+"\n"+`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","content":{}}}`+"\n")
	o.finish()
	if o.err == nil || o.completed || o.result.Answer != "" || o.result.UsageComplete {
		t.Fatalf("unsupported late content retained result: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
	}
}

func TestUnprovedLateRootContentRequiresNewResult(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root"}}}`
	const delta = `{"type":"stream_event","uuid":"chunk","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","content":[{"type":"text","text":"A"}]}}`
	const changed = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","content":[{"type":"text","text":"B"}]}}`
	const mixed = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","content":[{"type":"text","text":"A"},{"type":"tool_use","name":"Bash","input":{}}]}}`
	const contentStop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_stop","delta":{"type":"text_delta","text":"B"}}}`
	const contentStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root","content":[{"type":"text","text":"B"}]}}}`
	untaggedStart := strings.Replace(start, `"parent_tool_use_id":null,`, "", 1)
	untaggedDelta := strings.Replace(delta, `"parent_tool_use_id":null,`, "", 1)
	untaggedChanged := strings.Replace(changed, `"parent_tool_use_id":null,`, "", 1)
	for _, tc := range []struct{ name, before, after string }{
		{"changed first full frame", start + "\n" + delta, changed},
		{"untagged changed root frame", untaggedStart + "\n" + untaggedDelta, untaggedChanged},
		{"malformed historical full frame", full, mixed},
		{"content on message stop", start + "\n" + delta, contentStop},
		{"content on replayed message start", start + "\n" + delta, contentStart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.before+"\n"+successfulResult+"\n"+tc.after+"\n")
			o.finish()
			if o.err == nil || o.completed || o.result.Answer != "" || o.result.UsageComplete {
				t.Fatalf("unproved root content retained result: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
			}
		})
	}
}

func TestCrossKeyStreamIDKeepsLateFullCountersSeparate(t *testing.T) {
	const agentStart = `{"type":"stream_event","agent_id":"A","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"shared"}}}`
	const agentDelta = `{"type":"stream_event","agent_id":"A","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"work"}}}`
	const rootStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"shared"}}}`
	const rootDelta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"work"}}}`
	const agentBFull = `{"type":"assistant","agent_id":"B","parent_tool_use_id":"delegate","message":{"id":"shared","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":100}}}`
	const helperFull = `{"type":"assistant","parent_tool_use_id":"delegate","message":{"id":"shared","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":100}}}`
	const rootFull = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"shared","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":100}}}`
	for _, tc := range []struct {
		name, before, after string
		completed           bool
		output              int64
	}{
		{"agent A versus B", agentStart + "\n" + agentDelta, agentBFull, true, 100},
		{"root versus helper", rootStart + "\n" + rootDelta, helperFull, true, 100},
		{"helper versus root", agentStart + "\n" + agentDelta, rootFull, false, 111},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.before+"\n"+successfulResult+"\n"+tc.after+"\n")
			o.finish()
			if o.result.Tokens.Output != tc.output || o.result.UsageComplete || o.completed != tc.completed || (o.err == nil) != tc.completed {
				t.Fatalf("cross-key counters merged or lost: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
			}
		})
	}
}

func TestCrossKeyFullFramesCountEachMessageBeforeResult(t *testing.T) {
	const fullA = `{"type":"assistant","agent_id":"A","parent_tool_use_id":"delegate","message":{"id":"shared","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const fullB = `{"type":"assistant","agent_id":"B","parent_tool_use_id":"delegate","message":{"id":"shared","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const fullOtherParent = `{"type":"assistant","agent_id":"A","parent_tool_use_id":"other","message":{"id":"shared","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const startA = `{"type":"stream_event","agent_id":"A","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"shared"}}}`
	const deltaA = `{"type":"stream_event","agent_id":"A","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"work"}}}`
	const startOtherParent = `{"type":"stream_event","agent_id":"A","parent_tool_use_id":"other","event":{"type":"message_start","message":{"id":"shared"}}}`
	const deltaOtherParent = `{"type":"stream_event","agent_id":"A","parent_tool_use_id":"other","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"work"}}}`
	const result = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}}`
	for _, tc := range []struct{ name, before string }{
		{"two full-only agents", fullA + "\n" + fullB},
		{"streamed A and full-only B", startA + "\n" + deltaA + "\n" + fullA + "\n" + fullB},
		{"one agent with two full-only parents", fullA + "\n" + fullOtherParent},
		{"one agent with two streamed parents", startA + "\n" + deltaA + "\n" + startOtherParent + "\n" + deltaOtherParent + "\n" + fullA + "\n" + fullOtherParent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.before+"\n"+result+"\n")
			o.finish()
			if o.err != nil || !o.completed || o.result.Tokens.Output != 10 || o.result.UsageComplete ||
				o.nativeCostUSD != nil || o.result.EstimatedCostUSD == nil || *o.result.EstimatedCostUSD != 0.5 {
				t.Fatalf("cross-key full frames merged or certified: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
			}
		})
	}
}

func TestAmbiguousAssistantProvenanceCannotCertifyUsageOrCost(t *testing.T) {
	const usage = `"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}`
	const invalidNumber = `{"type":"assistant","parent_tool_use_id":0,"message":{"id":"shared",` + usage + `}}`
	const invalidEmpty = `{"type":"assistant","parent_tool_use_id":"","message":{"id":"shared",` + usage + `}}`
	const missing = `{"type":"assistant","message":{"id":"shared",` + usage + `}}`
	const root = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"shared",` + usage + `}}`
	const tagged = `{"type":"assistant","agent_id":"A","parent_tool_use_id":"delegate","message":{"id":"shared",` + usage + `}}`
	const untagged = `{"type":"assistant","parent_tool_use_id":"delegate","message":{"id":"shared",` + usage + `}}`
	const taggedStart = `{"type":"stream_event","agent_id":"A","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"shared"}}}`
	const untaggedStart = `{"type":"stream_event","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"shared"}}}`
	const result = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}}`
	for _, tc := range []struct {
		name, first, second string
		known               int64
	}{
		{"invalid parent values", invalidNumber, invalidEmpty, 0},
		{"missing then null root parent", missing, root, 5},
		{"null then missing root parent", root, missing, 5},
		{"tagged then untagged helper", tagged, untagged, 5},
		{"untagged then tagged helper", untagged, tagged, 5},
		{"tagged stream then untagged full", taggedStart, untagged, 0},
		{"untagged stream then tagged full", untaggedStart, tagged, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.first+"\n"+tc.second+"\n"+result+"\n")
			o.finish()
			if o.err != nil || !o.completed || o.assistantTokens.Output != tc.known ||
				o.result.Tokens.Output != 5 || o.result.UsageComplete || o.nativeCostUSD != nil ||
				o.result.EstimatedCostUSD == nil || *o.result.EstimatedCostUSD != 0.5 {
				t.Fatalf("ambiguous frames were allocated or certified: assistant=%+v result=%+v completed=%t error=%v",
					o.assistantTokens, o.result, o.completed, o.err)
			}
		})
	}
}

func TestMissingAssistantIDCannotCertifySmallerFinalReport(t *testing.T) {
	const unknown = `{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":10}}}`
	const result = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}}`
	for _, tc := range []struct {
		name    string
		resumed bool
		output  int64
	}{
		{"fresh", false, 10},
		{"resumed", true, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{resumed: tc.resumed}
			writeChunks(t, &o, unknown+"\n"+result+"\n")
			o.finish()
			if o.err != nil || !o.completed || o.assistantTokens != (usage.Tokens{}) ||
				o.result.Tokens.Output != tc.output || o.result.UsageComplete || o.nativeCostUSD != nil {
				t.Fatalf("unidentified counters certified final report: assistant=%+v result=%+v completed=%t error=%v",
					o.assistantTokens, o.result, o.completed, o.err)
			}
		})
	}
}

func TestRejectedOriginDoesNotAllocateUnprovedFailedCallCounters(t *testing.T) {
	const rootStart = `{"type":"stream_event","session_id":"native-id","event":{"type":"message_start","message":{"id":"shared"}}}`
	const rootFull = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"shared","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const tagged = `{"type":"assistant","session_id":"native-id","agent_id":"A","parent_tool_use_id":"delegate","message":{"id":"shared","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const other = `{"type":"assistant","session_id":"native-id","agent_id":"B","parent_tool_use_id":"delegate","message":{"id":"other","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const untaggedRevision = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":"delegate","message":{"id":"shared","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":8}}}`
	const taggedStart = `{"type":"stream_event","session_id":"native-id","agent_id":"A","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"shared"}}}`
	const untaggedStart = `{"type":"stream_event","session_id":"native-id","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"shared"}}}`
	for _, tc := range []struct {
		name, before, rejected string
		known                  int64
	}{
		{"root stream then explicit full", rootStart, rootFull, 0},
		{"tagged helper then untagged revision", tagged, untaggedRevision, 5},
		{"two helpers and rejected revision", tagged + "\n" + other, untaggedRevision, 10},
		{"tagged stream then untagged full", taggedStart, untaggedRevision, 0},
		{"untagged stream then tagged full", untaggedStart, tagged, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{sessionID: "native-id", identitySeen: true}
			writeChunks(t, &o, tc.before+"\n"+tc.rejected+"\n")
			o.finish()
			if o.err == nil || !o.provenanceGap || o.result.Tokens.Output != tc.known || o.result.UsageComplete || o.nativeCostUSD != nil {
				t.Fatalf("rejected counters were allocated: result=%+v error=%v", o.result, o.err)
			}
		})
	}
}

func TestRejectedHistoricalReplayCannotInflateResumedCall(t *testing.T) {
	const firstResult = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":100}}}`
	const taggedStart = `{"type":"stream_event","agent_id":"A","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"shared"}}}`
	const tagged = `{"type":"assistant","agent_id":"A","parent_tool_use_id":"delegate","message":{"id":"shared","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const oldUntagged = `{"type":"assistant","parent_tool_use_id":"delegate","message":{"id":"shared","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":100}}}`
	const secondResult = `{"type":"result","subtype":"success","result":"second","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":105}}}`
	first := jsonlOutput{}
	writeChunks(t, &first, firstResult+"\n")
	first.finish()
	if first.err != nil || first.cumulative.Output != 100 {
		t.Fatalf("first call: result=%+v error=%v", first.result, first.err)
	}
	second := jsonlOutput{resumed: true, baseline: first.cumulative, progressSeen: first.progressSeen,
		assistantHistory: first.assistantHistory, progressBytes: first.progressBytes, progressEntries: first.progressEntries}
	writeChunks(t, &second, taggedStart+"\n"+tagged+"\n"+oldUntagged+"\n"+secondResult+"\n")
	second.finish()
	if second.err != nil || !second.completed || second.assistantTokens.Output != 0 ||
		second.result.Tokens.Output != 5 || second.result.UsageComplete || second.nativeCostUSD != nil {
		t.Fatalf("rejected historical replay inflated resumed subtotal: result=%+v assistant=%+v error=%v",
			second.result, second.assistantTokens, second.err)
	}
}

func TestReplacedFreshStreamCannotCertifySmallerResult(t *testing.T) {
	const startA = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"A"}}}`
	const startB = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"B"}}}`
	const fullA = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"A","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":100}}}`
	const fullB = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"B","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const result = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}}`
	o := jsonlOutput{}
	writeChunks(t, &o, startA+"\n"+startB+"\n"+fullA+"\n"+fullB+"\n"+result+"\n")
	o.finish()
	if o.err != nil || !o.completed || o.result.Tokens.Output != 100 || o.result.UsageComplete ||
		o.result.EstimatedCostUSD == nil || *o.result.EstimatedCostUSD != 0.5 || o.nativeCostUSD != nil || !o.provenanceGap {
		t.Fatalf("replaced stream certified smaller result: result=%+v gap=%t error=%v", o.result, o.provenanceGap, o.err)
	}
}

func TestResumedUnprovedLateUnknownParentClearsAnswer(t *testing.T) {
	const result = `{"type":"result","session_id":"native-id","subtype":"success","result":"old answer","modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}}`
	for _, tc := range []struct{ name, parent string }{
		{"missing", ""},
		{"invalid", `,"parent_tool_use_id":0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{sessionID: "native-id", resumed: true}
			late := `{"type":"assistant","session_id":"native-id"` + tc.parent + `,"message":{"id":"late","content":[{"type":"text","text":"new work"}]}}`
			writeChunks(t, &o, result+"\n"+late+"\n")
			o.finish()
			if o.err == nil || o.completed || o.result.Answer != "" || o.result.UsageComplete || !o.provenanceGap {
				t.Fatalf("unproved late frame retained stale answer: result=%+v completed=%t gap=%t error=%v",
					o.result, o.completed, o.provenanceGap, o.err)
			}
		})
	}
}

func TestResumedIDOnlyAfterResultCannotCertifyAnswerOrCost(t *testing.T) {
	const result = `{"type":"result","session_id":"native-id","subtype":"success","result":"old answer","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}}`
	const late = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"new"}}`
	o := jsonlOutput{sessionID: "native-id", resumed: true}
	writeChunks(t, &o, result+"\n"+late+"\n")
	o.finish()
	if o.err == nil || o.completed || o.result.Answer != "" || o.result.UsageComplete ||
		o.nativeCostUSD != nil || !o.provenanceGap {
		t.Fatalf("ID-only late frame certified stale answer or cost: result=%+v completed=%t gap=%t error=%v",
			o.result, o.completed, o.provenanceGap, o.err)
	}
}

func TestResumedUnprovedAssistantContentMarksOnlyReceiptOmissions(t *testing.T) {
	const result = `{"type":"result","session_id":"native-id","subtype":"success","result":"old answer","modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}}`
	const emptyText = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"empty","content":[{"type":"text","text":""}]}}`
	const mixedEmptyText = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"mixed","content":[{"type":"text","text":"","thinking":"private"}]}}`
	for _, test := range []struct {
		name, frame string
		missing     bool
	}{
		{"no text ID", `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"content":[{"type":"text","text":"omitted"}]}}`, true},
		{"no tool ID", `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"tool","name":"Bash","input":{}}]}}`, true},
		{"malformed message", `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":"malformed"}`, true},
		{"malformed private block", `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"thought","content":[{"type":"thinking","thinking":"private","text":42}]}}`, true},
		{"empty text block", emptyText, false},
		{"mixed empty text block", mixedEmptyText, true},
		{"thinking only", `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"thought","content":[{"type":"thinking","thinking":"private"}]}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []aicli.StreamEvent
			o := jsonlOutput{sessionID: "native-id", resumed: true, onEvent: func(e aicli.StreamEvent) { events = append(events, e) }}
			writeChunks(t, &o, result+"\n"+test.frame+"\n")
			if !o.provenanceGap {
				t.Fatalf("unproved resumed frame preserved provenance: %+v", o.result)
			}
			if test.missing {
				if len(events) != 1 || events[0] != (aicli.StreamEvent{Kind: "content_missing"}) {
					t.Fatalf("missing content lacked one metadata-only marker: %+v", events)
				}
			} else if len(events) != 0 {
				t.Fatalf("private thinking falsely changed receipt coverage: %+v", events)
			}
		})
	}
	for _, freshTest := range []struct {
		frame   string
		missing bool
	}{{emptyText, false}, {mixedEmptyText, true}} {
		var freshEvents []aicli.StreamEvent
		fresh := jsonlOutput{sessionID: "native-id", identitySeen: true,
			onEvent: func(e aicli.StreamEvent) { freshEvents = append(freshEvents, e) }}
		writeChunks(t, &fresh, freshTest.frame+"\n")
		if freshTest.missing {
			if len(freshEvents) != 1 || freshEvents[0].Kind != "assistant" || freshEvents[0].Text != "" {
				t.Fatalf("fresh malformed empty text lacked a receipt gap: %+v", freshEvents)
			}
		} else if len(freshEvents) != 0 {
			t.Fatalf("fresh empty text falsely changed receipt coverage: %+v", freshEvents)
		}
	}
}

func TestContradictoryHelperResultAndInitCannotClaimRoot(t *testing.T) {
	for _, parent := range []string{"null", ""} {
		t.Run("parent="+parent, func(t *testing.T) {
			parentField := `,"parent_tool_use_id":` + parent
			if parent == "" {
				parentField = ""
			}
			result := `{"type":"result","agent_id":"worker"` + parentField + `,"session_id":"native-id","subtype":"success","result":"wrong","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}}`
			init := `{"type":"system","subtype":"init","agent_id":"worker"` + parentField + `,"session_id":"native-id"}`
			o := jsonlOutput{sessionID: "native-id", identitySeen: true}
			writeChunks(t, &o, result+"\n")
			if o.results != 0 || o.completed || o.result.Answer != "" || o.nativeCostUSD != nil || !o.provenanceGap {
				t.Fatalf("contradictory helper result claimed root: result=%+v completed=%t", o.result, o.completed)
			}
			o = jsonlOutput{sessionID: "native-id"}
			writeChunks(t, &o, init+"\n")
			if o.identitySeen || !o.provenanceGap {
				t.Fatalf("contradictory helper init claimed root: identity=%t gap=%t", o.identitySeen, o.provenanceGap)
			}
		})
	}
}

func TestLateRootFullFrameRecoversAfterNewResult(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root"}}}`
	const delta = `{"type":"stream_event","uuid":"chunk","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","content":[{"type":"text","text":"A"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}}`
	o := jsonlOutput{}
	writeChunks(t, &o, numberResultEvents(start+"\n"+delta+"\n"+successfulResult+"\n"+full+"\n"+successfulResult+"\n"))
	o.finish()
	if o.err != nil || !o.completed || o.result.Answer != "final answer" || !o.result.UsageComplete || o.result.Tokens.Output != 11 {
		t.Fatalf("covering result did not recover root usage and answer: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
	}
}

func TestLateRootMalformedCountersKeepAnswerButLoseCoverage(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}}`
	const late = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","content":[],"usage":{}}}`
	o := jsonlOutput{}
	writeChunks(t, &o, start+"\n"+delta+"\n"+successfulResult+"\n"+late+"\n")
	o.finish()
	if o.err != nil || !o.completed || o.result.Answer != "final answer" || o.result.UsageComplete || o.result.Tokens.Output != 11 {
		t.Fatalf("malformed root counters changed answer or certified usage: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
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

func TestEarlierAssistantIDCannotProveResumedStreamContent(t *testing.T) {
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
	if second.err == nil || second.completed || second.result.Answer != "" || second.result.UsageComplete {
		t.Fatalf("unproven resumed stream content retained result: completed=%t answer=%q result=%+v error=%v", second.completed, second.result.Answer, second.result, second.err)
	}
}

func TestFirstFullAssistantAfterResumedStreamOnlyResultNeedsNewResult(t *testing.T) {
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
			if second.err != nil || second.completed || second.result.Answer != "" || second.result.UsageComplete || progress != want {
				t.Fatalf("first unproved full frame retained result or renewed progress: completed=%t progress=%d answer=%q error=%v",
					second.completed, progress, second.result.Answer, second.err)
			}
			writeChunks(t, &second, changed+"\n")
			if second.err != nil || second.completed || second.result.Answer != "" || progress != want {
				t.Fatalf("unmatched historical content renewed progress: completed=%t progress=%d answer=%q error=%v",
					second.completed, progress, second.result.Answer, second.err)
			}
		})
	}
}

func TestFirstFullRootAssistantAfterResultNeedsNewResult(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"old"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"old output"}}}`
	const stop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_stop"}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"old output"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}}`
	progress := 0
	o := jsonlOutput{onProgress: func() { progress++ }}
	writeChunks(t, &o, start+"\n"+delta+"\n"+stop+"\n"+successfulResult+"\n"+full+"\n")
	o.finish()
	if o.err == nil || o.completed || o.result.Answer != "" || o.result.UsageComplete || progress != 1 {
		t.Fatalf("full stream completion after result: completed=%t progress=%d answer=%q error=%v",
			o.completed, progress, o.result.Answer, o.err)
	}
}

func TestFirstHelperFullWithUsageBeyondResultNeedsCoveringUsage(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"current"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}}`
	const stop = `{"type":"stream_event","parent_tool_use_id":"delegate","event":{"type":"message_stop"}}`
	const full = `{"type":"assistant","parent_tool_use_id":"delegate","message":{"id":"current","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":12}}}`
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
		if o.err != nil || o.result.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 12}) || o.result.UsageComplete != recovered {
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
	if snapshot := first.assistantHistory[streamIdentity{"root", "old"}]; !snapshot.hasTokens || snapshot.tokens.Valid() {
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

func TestTwoLateStreamedMessagesKeepKnownFloorButRequireNewResult(t *testing.T) {
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
	if o.err == nil || o.completed || o.result.Answer != "" || o.result.UsageComplete || o.result.Tokens.Output < 16 {
		t.Fatalf("late streamed root usage was lost or left a stale answer: %+v completed=%t error=%v", o.result, o.completed, o.err)
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
	const old = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"historical text"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":2}}}`
	first := jsonlOutput{sessionID: "native-id", identitySeen: true}
	writeChunks(t, &first, old+"\n"+successfulResult+"\n")
	first.finish()
	if first.err != nil || !first.hasCumulative {
		t.Fatalf("first usage baseline: error=%v cumulative=%t", first.err, first.hasCumulative)
	}
	var events []aicli.StreamEvent
	second := jsonlOutput{sessionID: "native-id", identitySeen: true, resumed: true,
		baseline: first.cumulative, progressSeen: first.progressSeen, assistantHistory: first.assistantHistory,
		progressBytes: first.progressBytes, progressEntries: first.progressEntries,
		onEvent: func(e aicli.StreamEvent) { events = append(events, e) }}
	const cumulative = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":14}}}`
	writeChunks(t, &second, cumulative+"\n"+old+"\n")
	second.finish()
	if second.err != nil || !second.completed || second.result.Answer != "done" ||
		second.result.Tokens != (usage.Tokens{Output: 3}) || !second.result.UsageComplete {
		t.Fatalf("exact replay revoked or inflated current result: result=%+v completed=%t error=%v", second.result, second.completed, second.err)
	}
	if len(events) != 0 {
		t.Fatalf("exact historical replay entered current receipt: %+v", events)
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

func TestAmbientInitAndResultCannotClaimRoot(t *testing.T) {
	const ambientInit = `{"type":"system","subtype":"init","session_id":"native-id","ambient":true}`
	const ambientResult = `{"type":"result","session_id":"native-id","ambient":true,"parent_tool_use_id":null,"subtype":"success","result":"background answer","modelUsage":{"main":{"inputTokens":99,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":99}}}`
	const currentInit = `{"type":"system","subtype":"init","session_id":"native-id"}`
	const currentResult = `{"type":"result","session_id":"native-id","subtype":"success","result":"current answer","modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	o := jsonlOutput{sessionID: "native-id"}
	writeChunks(t, &o, ambientInit+"\n"+ambientResult+"\n")
	if o.identitySeen || o.completed || o.result.Answer != "" || o.result.Tokens != (usage.Tokens{}) {
		t.Fatalf("ambient event claimed root identity or result: identity=%t result=%+v", o.identitySeen, o.result)
	}
	writeChunks(t, &o, currentInit+"\n"+currentResult+"\n"+ambientResult+"\n")
	o.finish()
	if o.err != nil || !o.completed || !o.identitySeen || o.result.Answer != "current answer" ||
		o.result.Tokens != (usage.Tokens{Input: 1, Output: 2}) || !o.result.UsageComplete {
		t.Fatalf("ambient result changed current root: result=%+v identity=%t error=%v", o.result, o.identitySeen, o.err)
	}
}

func TestAmbientAndForeignQuotaCannotFailCurrentResult(t *testing.T) {
	const currentInit = `{"type":"system","subtype":"init","session_id":"native-id"}`
	const currentResult = `{"type":"result","session_id":"native-id","subtype":"success","result":"current answer","modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	for _, tc := range []struct{ name, fields string }{
		{"ambient", `"session_id":"native-id","ambient":true`},
		{"foreign", `"session_id":"foreign"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			o := jsonlOutput{sessionID: "native-id", fatal: func() { calls++ }}
			quota := `{"type":"rate_limit_event",` + tc.fields + `,"rate_limit_info":{"status":"rejected"}}`
			writeChunks(t, &o, currentInit+"\n"+currentResult+"\n"+quota+"\n")
			o.finish()
			if o.err != nil || o.usageLimit != nil || calls != 0 || !o.completed ||
				o.result.Answer != "current answer" || !o.result.UsageComplete {
				t.Fatalf("unrelated quota failed current result: result=%+v calls=%d error=%v", o.result, calls, o.err)
			}
		})
	}
}

func TestMalformedForeignUserCannotFailCurrentResult(t *testing.T) {
	const init = `{"type":"system","subtype":"init","session_id":"native-id"}`
	const result = `{"type":"result","session_id":"native-id","subtype":"success","result":"current answer","modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	for _, tc := range []struct {
		name, session string
		fails         bool
	}{
		{"foreign", "foreign", false},
		{"current", "native-id", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []aicli.StreamEvent
			o := jsonlOutput{sessionID: "native-id", onEvent: func(e aicli.StreamEvent) { got = append(got, e) }}
			malformed := `{"type":"user","session_id":"` + tc.session + `","message":{"content":[{"type":"text","text":42}]}}`
			writeChunks(t, &o, init+"\n"+result+"\n"+malformed+"\n")
			o.finish()
			if (o.err != nil) != tc.fails || o.completed == tc.fails || (o.result.Answer == "current answer") == tc.fails {
				t.Fatalf("foreign user changed current result: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
			}
			if !tc.fails && (len(got) != 1 || got[0] != (aicli.StreamEvent{Kind: "user"})) {
				t.Fatalf("foreign user content leaked instead of a marker: %+v", got)
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
	rootAssistant := (streamIdentity{"root", "old"}).progressKey("assistant")
	unknownAssistant := (streamIdentity{"unknown-parent", "old"}).progressKey("assistant")
	helperAssistant := (streamIdentity{"parent:tool", "helper"}).progressKey("assistant")
	rootStart := (streamIdentity{"root", "old"}).progressKey("stream_start")
	start := func(id, fields string) string {
		return fmt.Sprintf(`{"type":"stream_event"%s,"event":{"type":"message_start","message":{"id":%q}}}`, fields, id)
	}
	missingID := func(fields string) string {
		return fmt.Sprintf(`{"type":"stream_event"%s,"event":{"type":"message_start","message":{}}}`, fields)
	}
	const delta = `{"type":"stream_event","uuid":"chunk","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"new answer"}}}`
	rootDelta := strings.Replace(delta, `"event":`, `"parent_tool_use_id":null,"event":`, 1)
	unknownParentDelta := strings.Replace(delta, `"event":`, `"parent_tool_use_id":0,"event":`, 1)
	emptyParentDelta := strings.Replace(unknownParentDelta, `"parent_tool_use_id":0`, `"parent_tool_use_id":""`, 1)
	const stop = `{"type":"stream_event","event":{"type":"message_stop"}}`
	rootStop := strings.Replace(stop, `"event":`, `"parent_tool_use_id":null,"event":`, 1)
	const helperDelta = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"tool","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"helper answer"}}}`
	const helperStop = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"tool","event":{"type":"message_stop"}}`
	for _, tc := range []struct {
		name, before, after, resumeKey string
		want, incomplete               bool
	}{
		{"fresh root", "", start("new", `,"parent_tool_use_id":null`) + "\n" + delta + "\n" + stop, "", false, false},
		{"empty fresh root", "", start("new", `,"parent_tool_use_id":null`) + "\n" + stop, "", false, false},
		{"fresh untagged", "", start("new", "") + "\n" + delta + "\n" + stop, "", false, false},
		{"unknown parent", "", start("new", `,"parent_tool_use_id":0`) + "\n" + delta + "\n" + stop, "", false, false},
		{"unknown parent old ID with content", "", start("old", `,"parent_tool_use_id":0`) + "\n" + unknownParentDelta, unknownAssistant, false, true},
		{"distinct unknown parents cannot share stream proof", start("old", `,"parent_tool_use_id":0`) + "\n" + unknownParentDelta,
			start("old", `,"parent_tool_use_id":""`) + "\n" + emptyParentDelta, "", false, true},
		{"missing ID root", "", missingID(`,"parent_tool_use_id":null`), "", false, false},
		{"missing ID untagged", "", missingID(""), "", false, false},
		{"missing ID unknown parent", "", missingID(`,"parent_tool_use_id":0`), "", false, false},
		{"missing ID then fresh result", "", missingID(`,"parent_tool_use_id":null`) + "\n" + successfulResult, "", true, false},
		{"missing ID agent helper", "", missingID(`,"agent_id":"worker"`), "", false, true},
		{"missing ID parent-tagged helper", "", missingID(`,"parent_tool_use_id":"tool"`), "", true, true},
		{"helper", "", start("helper", `,"agent_id":"worker","parent_tool_use_id":"tool"`) + "\n" + helperDelta + "\n" + helperStop, "", true, true},
		{"parent-tagged helper", "", start("helper", `,"parent_tool_use_id":"tool"`) + "\n" + strings.ReplaceAll(helperDelta, `"agent_id":"worker",`, "") + "\n" + strings.ReplaceAll(helperStop, `"agent_id":"worker",`, ""), "", true, true},
		{"helper delta after result", start("helper", `,"agent_id":"worker","parent_tool_use_id":"tool"`), helperDelta, "", true, true},
		{"repeated helper start then fresh delta", start("helper", `,"agent_id":"worker","parent_tool_use_id":"tool"`), start("helper", `,"agent_id":"worker","parent_tool_use_id":"tool"`) + "\n" + helperDelta, "", true, true},
		{"missing helper ID then late delta", missingID(`,"agent_id":"worker","parent_tool_use_id":"tool"`), helperDelta, "", true, true},
		{"old helper ID with unproven content", "", start("helper", `,"parent_tool_use_id":"tool"`) + "\n" + strings.ReplaceAll(helperDelta, `"agent_id":"worker",`, ""), helperAssistant, true, true},
		{"stream replay", start("old", `,"parent_tool_use_id":null`) + "\n" + rootDelta + "\n" + rootStop, start("old", `,"parent_tool_use_id":null`) + "\n" + rootDelta + "\n" + rootStop, "", true, false},
		{"assistant ID without stream proof", `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old"}}`, start("old", `,"parent_tool_use_id":null`) + "\n" + delta + "\n" + stop, "", false, true},
		{"resumed ID without stream proof", "", start("old", `,"parent_tool_use_id":null`) + "\n" + delta + "\n" + stop, rootAssistant, false, true},
		{"resumed empty replay", "", start("old", `,"parent_tool_use_id":null`) + "\n" + stop, rootStart, true, false},
		{"fresh result", "", start("new", `,"parent_tool_use_id":null`) + "\n" + delta + "\n" + stop + "\n" + successfulResult, "", true, false},
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
				if errors.Is(o.err, aicli.ErrOutputLimit) || (o.err == nil) != tc.want || o.completed != tc.want || (o.result.Answer == "final answer") != tc.want || o.result.UsageComplete != (tc.want && !tc.incomplete) || o.result.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}) {
					t.Fatalf("stale stream completion: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
				}
			})
		}
	}
}

func TestLateNestedUsageDoesNotAttributeOldCounters(t *testing.T) {
	for _, nested := range []string{
		`{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_delta","delta":{"usage":{"output_tokens":100}}}}`,
		`{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"old","usage":{"output_tokens":100}}}}`,
		`{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_delta","usage":{"output_tokens":100}}}`,
		`{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_delta","usage":{"output_tokens":9223372036854775807}}}`,
	} {
		o := jsonlOutput{}
		writeChunks(t, &o, successfulResult+"\n"+nested+"\n"+nested+"\n")
		o.finish()
		if o.err != nil || !o.completed || o.result.Answer != "final answer" || o.result.UsageComplete || o.result.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}) {
			t.Fatalf("late nested usage was attributed to this call: result=%+v error=%v", o.result, o.err)
		}
	}
	o := jsonlOutput{}
	const root = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"output_tokens":12}}}`
	writeChunks(t, &o, successfulResult+"\n"+root+"\n")
	o.finish()
	if o.err == nil || o.completed || o.result.Answer != "" || o.result.UsageComplete {
		t.Fatalf("late root usage retained an old answer: result=%+v completed=%t error=%v", o.result, o.completed, o.err)
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
			if (o.err != nil) != tc.fails || o.completed == tc.fails || o.result.UsageComplete == tc.fails || o.result.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: tc.output}) {
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
				if (o.err != nil) != fails || o.result.UsageComplete == fails || o.result.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}) {
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
		if o.err == nil || o.result.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}) {
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
				want := usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 2}
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
