package claudecli

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/usage"
)

func TestResumedPendingFrameReplayAfterLaterStart(t *testing.T) {
	const startA = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"A"}}}`
	const frameA = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"first"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const startB = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"B"}}}`
	const frameB = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"B","content":[{"type":"text","text":"second"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2}}}`
	const result = `{"type":"result","session_id":"native-id","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":17}}}`
	var events []aicli.StreamEvent
	o := jsonlOutput{resumed: true, sessionID: "native-id", identitySeen: true, baseline: usage.Tokens{Output: 10},
		onEvent: func(event aicli.StreamEvent) { events = append(events, event) }}
	writeChunks(t, &o, startA+"\n"+frameA+"\n"+startB+"\n"+frameB+"\n"+frameA+"\n"+result+"\n")
	o.finish()
	if o.err != nil || !o.result.UsageComplete || o.result.Tokens != (usage.Tokens{Output: 7}) ||
		len(events) != 2 || events[0].Text != "first" || events[1].Text != "second" {
		t.Fatalf("pending frame replay changed usage or content: result=%+v events=%+v error=%v", o.result, events, o.err)
	}
}

func TestResumedPendingFrameReservesContentHistoryBeforeResult(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"A"}}}`
	const frame = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"A","content":[{"type":"text","text":"work"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const result = `{"type":"result","session_id":"native-id","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":15}}}`
	o := jsonlOutput{resumed: true, sessionID: "native-id", identitySeen: true, baseline: usage.Tokens{Output: 10}, progressEntryLimit: 2}
	writeChunks(t, &o, start+"\n"+frame+"\n"+result+"\n")
	o.finish()
	if !errors.Is(o.err, aicli.ErrOutputLimit) || o.progressEntries != 1 ||
		o.result.UsageComplete || o.hasCumulative || o.nativeCostUSD != nil {
		t.Fatalf("limit certified a resumed receipt: entries=%d complete=%t cumulative=%t cost=%v error=%v", o.progressEntries, o.result.UsageComplete, o.hasCumulative, o.nativeCostUSD, o.err)
	}
}

func TestResumedAssistantBlocksShareMessageUsage(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"m2"}}}`
	const textBlock = `[{"type":"text","text":"I will look"}]`
	const toolBlock = `[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"pwd"}}]`
	frame := func(content string, output int) string {
		return fmt.Sprintf(`{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"m2","content":%s,"usage":{"input_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":%d}}}`, content, output)
	}
	for _, tc := range []struct {
		name                  string
		first, second, repeat int
		complete              bool
	}{
		{"same counters and adjacent replay", 7, 7, 7, true},
		{"rising counters", 5, 7, 8, true},
		{"decreasing counters", 7, 5, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []aicli.StreamEvent
			progress := 0
			o := jsonlOutput{resumed: true, sessionID: "native-id", identitySeen: true,
				baseline: usage.Tokens{Input: 10, Output: 5}, costBaselineUSD: 0.5,
				onEvent:    func(event aicli.StreamEvent) { events = append(events, event) },
				onProgress: func() { progress++ }}
			resultOutput := max(tc.first, tc.second, tc.repeat)
			result := fmt.Sprintf(`{"type":"result","session_id":"native-id","subtype":"success","result":"done","total_cost_usd":0.75,"usage":{"input_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":%d},"modelUsage":{"main":{"inputTokens":30,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":%d}}}`, resultOutput, resultOutput+5)
			lines := []string{start, frame(textBlock, tc.first), frame(toolBlock, tc.second)}
			if tc.repeat != 0 {
				lines = append(lines, frame(toolBlock, tc.repeat))
			}
			lines = append(lines, result)
			writeChunks(t, &o, strings.Join(lines, "\n")+"\n")
			o.finish()
			if o.err != nil || o.result.UsageComplete != tc.complete || o.provenanceGap == tc.complete ||
				(o.nativeCostUSD != nil) != tc.complete || o.result.Tokens != (usage.Tokens{Input: 20, Output: int64(resultOutput)}) {
				t.Fatalf("multi-block usage: result=%+v gap=%t cost=%v error=%v", o.result, o.provenanceGap, o.nativeCostUSD, o.err)
			}
			if tc.complete {
				wantProgress := 2
				if tc.repeat > tc.second {
					wantProgress++
				}
				if progress != wantProgress || len(events) != 2 || events[0].Kind != "assistant" || events[0].Text != "I will look" ||
					events[1].Kind != "tool_request" || !strings.Contains(events[1].Text, `"command":"pwd"`) {
					t.Fatalf("block content/progress changed: events=%+v progress=%d", events, progress)
				}
				var replayEvents []aicli.StreamEvent
				bytes, entries := o.progressCheckpoint()
				replayResult := fmt.Sprintf(`{"type":"result","session_id":"native-id","subtype":"success","result":"done","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":30,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":%d}}}`, resultOutput+5)
				replay := jsonlOutput{resumed: true, sessionID: "native-id", identitySeen: true,
					baseline: o.cumulative, costBaselineUSD: 0.75,
					progressSeen: o.progressSeen, progressBytes: bytes, progressEntries: entries,
					assistantHistory: o.assistantHistory,
					onEvent:          func(event aicli.StreamEvent) { replayEvents = append(replayEvents, event) }}
				replayLines := []string{frame(textBlock, tc.first), frame(toolBlock, tc.second)}
				if tc.repeat != 0 {
					replayLines = append(replayLines, frame(toolBlock, tc.repeat))
				}
				replayLines = append(replayLines, replayResult)
				writeChunks(t, &replay, strings.Join(replayLines, "\n")+"\n")
				replay.finish()
				if replay.err != nil || replay.provenanceGap || !replay.result.UsageComplete || len(replayEvents) != 0 {
					t.Fatalf("historical blocks replayed as new work: result=%+v events=%+v error=%v", replay.result, replayEvents, replay.err)
				}
			} else {
				for _, event := range events {
					if event.Text != "" {
						t.Fatalf("unproved block leaked content: %+v", events)
					}
				}
			}
		})
	}
}

func TestResumedAssistantBufferDoesNotConsumeSessionHistory(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	for _, frames := range []int{3, 17} {
		t.Run(strconv.Itoa(frames), func(t *testing.T) {
			var lines strings.Builder
			for i := range frames {
				id := fmt.Sprintf("m%d", i)
				fmt.Fprintf(&lines, `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":%q}}}`+"\n", id)
				fmt.Fprintf(&lines, `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":%q,"content":[{"type":"tool_use","id":"tu%d","name":"Write","input":{"content":%q}}],"usage":{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`+"\n", id, i, big)
			}
			fmt.Fprintf(&lines, `{"type":"result","session_id":"native-id","subtype":"success","result":"done","total_cost_usd":1,"modelUsage":{"main":{"inputTokens":%d,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":%d}}}`+"\n", 100+frames, 100+frames)
			o := jsonlOutput{resumed: true, sessionID: "native-id", identitySeen: true, baseline: usage.Tokens{Input: 100, Output: 100}}
			input := []byte(lines.String())
			if n, err := o.Write(input); n != len(input) || err != nil {
				t.Fatalf("Write=%d,%v; want %d bytes", n, err, len(input))
			}
			o.finish()
			bytes, entries := o.progressCheckpoint()
			if o.err != nil || !o.result.UsageComplete || o.result.Tokens != (usage.Tokens{Input: int64(frames), Output: int64(frames)}) ||
				bytes >= 1<<20 || entries > frames*3 {
				t.Fatalf("temporary frames consumed session history: result=%+v bytes=%d entries=%d error=%v", o.result, bytes, entries, o.err)
			}
		})
	}
}

func TestUnprovedResumedThinkingDoesNotEmitContentGap(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"m2"}}}`
	const thinking = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"m2","content":[{"type":"thinking","thinking":"private"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const underreported = `{"type":"result","session_id":"native-id","subtype":"success","result":"done","total_cost_usd":1,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":12}}}`
	var events []aicli.StreamEvent
	o := jsonlOutput{resumed: true, sessionID: "native-id", identitySeen: true, baseline: usage.Tokens{Output: 10},
		onEvent: func(event aicli.StreamEvent) { events = append(events, event) }}
	writeChunks(t, &o, start+"\n"+thinking+"\n"+underreported+"\n")
	o.finish()
	if o.err != nil || o.result.UsageComplete || !o.provenanceGap || len(events) != 0 {
		t.Fatalf("private thinking produced visible gap: result=%+v events=%+v error=%v", o.result, events, o.err)
	}
}

func TestAssistantContentProofTracksExecutionContent(t *testing.T) {
	for _, tc := range []struct {
		name, original, metadataReplay, changed string
	}{
		{"text", `[{"type":"text","text":"hello","citations":[1]}]`,
			`[{"type":"text","text":"hello","citations":[2]}]`, `[{"type":"text","text":"goodbye"}]`},
		{"thinking", `[{"type":"thinking","thinking":"private","signature":"one"}]`,
			`[{"type":"thinking","thinking":"private","signature":"two"}]`, `[{"type":"thinking","thinking":"new private"}]`},
		{"redacted", `[{"type":"redacted_thinking","data":"opaque","metadata":1}]`,
			`[{"type":"redacted_thinking","data":"opaque","metadata":2}]`, `[{"type":"redacted_thinking","data":"new opaque"}]`},
		{"tool", `[{"type":"tool_use","id":"call","name":"Bash","input":{"command":"pwd"},"caller":{"type":"direct"}}]`,
			`[{"type":"tool_use","id":"call","name":"Bash","input":{"command":"pwd"},"caller":{"type":"code"}}]`,
			`[{"type":"tool_use","id":"call","name":"Bash","input":{"command":"ls"}}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, present, unprojectable := assistantContentProof(json.RawMessage(tc.original))
			replay, replayPresent, replayUnprojectable := assistantContentProof(json.RawMessage(tc.metadataReplay))
			changed, changedPresent, changedUnprojectable := assistantContentProof(json.RawMessage(tc.changed))
			if !present || !replayPresent || !changedPresent || unprojectable || replayUnprojectable || changedUnprojectable || original != replay || original == changed {
				t.Fatalf("metadata must replay; changed content must be fresh: present=%t/%t/%t replay=%t changed=%t",
					present, replayPresent, changedPresent, original == replay, original != changed)
			}
		})
	}
	if _, present, unprojectable := assistantContentProof(json.RawMessage(`[{"type":"thinking","thinking":"","signature":"metadata"}]`)); present || unprojectable {
		t.Fatal("signature-only block was counted as execution content")
	}
	for _, raw := range []string{
		`{invalid`,
		`{"type":"text","text":"unsupported object shape"}`,
		`[{"type":"unknown","metadata":"changed"}]`,
		`["metadata"]`,
		`[{"type":"tool_use","name":"Bash","input":{"command":"pwd"}}]`,
		`[{"type":"text","text":42}]`,
		`[{"type":"thinking","thinking":42}]`,
		`[{"type":"redacted_thinking","data":42}]`,
		`42`,
		`true`,
	} {
		if _, present, unprojectable := assistantContentProof(json.RawMessage(raw)); present || !unprojectable {
			t.Fatalf("unsupported content was projected as exact: %s", raw)
		}
	}
	text, present, _ := assistantContentProof(json.RawMessage(`"text"`))
	changedText, changedPresent, _ := assistantContentProof(json.RawMessage(`"changed text"`))
	if !present || !changedPresent || text == changedText {
		t.Fatal("native string content change was missed")
	}
	textBlock, blockPresent, _ := assistantContentProof(json.RawMessage(`[{"type":"text","text":"text"}]`))
	if !blockPresent || text != textBlock {
		t.Fatal("equivalent string and single text block have different replay identities")
	}
}

func writeChunks(t *testing.T, output *jsonlOutput, data string) {
	t.Helper()
	for len(data) > 0 {
		n := min(7, len(data))
		if wrote, err := output.Write([]byte(data[:n])); wrote != n || err != nil {
			t.Fatalf("Write=%d,%v; want %d bytes", wrote, err, n)
		}
		data = data[n:]
	}
}

func TestOutputInterruptedNestedStreamUsageKeepsKnownFloor(t *testing.T) {
	const rootStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root","usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":1}}}}`
	const rootNoUsageStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root"}}}`
	const rootDelta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"output_tokens":4}}}`
	const rootOlderDelta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"output_tokens":2}}}`
	const rootStop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_stop"}}`
	const rootFull = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":1}}}`
	const changedCacheFull = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const cacheStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root","usage":{"input_tokens":5,"cache_creation_input_tokens":5,"cache_read_input_tokens":0,"output_tokens":1}}}}`
	const cacheReclassified = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":5,"output_tokens":2}}}`
	const helperFull = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","usage":{"input_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}}}`
	const sparseStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root","usage":{"input_tokens":1,"cache_creation_input_tokens":100,"output_tokens":1}}}}`
	const sparseDelta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"input_tokens":2,"output_tokens":2}}}`
	for _, tc := range []struct {
		name, lines string
		resumed     bool
		want        usage.Tokens
	}{
		{"interrupted root and cumulative deltas", rootStart + "\n" + rootDelta + "\n" + rootStop, false,
			usage.Tokens{Input: 15, CacheCreationInput: 2, CachedInput: 3, Output: 4}},
		{"sparse input update retains prior cache", sparseStart + "\n" + sparseDelta, false,
			usage.Tokens{Input: 102, CacheCreationInput: 100, Output: 2}},
		{"sparse equal input retains prior cache", sparseStart + "\n" + `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"input_tokens":1,"output_tokens":2}}}`, false,
			usage.Tokens{Input: 101, CacheCreationInput: 100, Output: 2}},
		{"delta without start usage keeps output", rootNoUsageStart + "\n" + rootDelta, false,
			usage.Tokens{Output: 4}},
		{"delta after stop has no active message", rootStart + "\n" + rootStop + "\n" + rootDelta, false,
			usage.Tokens{Input: 15, CacheCreationInput: 2, CachedInput: 3, Output: 1}},
		{"full helper adds once", rootStart + "\n" + rootDelta + "\n" + helperFull, false,
			usage.Tokens{Input: 17, CacheCreationInput: 2, CachedInput: 3, Output: 7}},
		{"full assistant receipt deduplicates stream", rootStart + "\n" + rootFull + "\n" + rootDelta, false,
			usage.Tokens{Input: 15, CacheCreationInput: 2, CachedInput: 3, Output: 4}},
		{"contradictory full cache cannot be added", cacheStart + "\n" + changedCacheFull + "\n" + rootDelta, false,
			usage.Tokens{Input: 10, CacheCreationInput: 5, Output: 4}},
		{"contradictory nested cache retains output", cacheStart + "\n" + cacheReclassified, false,
			usage.Tokens{Input: 10, CacheCreationInput: 5, Output: 2}},
		{"malformed delta retains start floor", rootStart + "\n" + `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"output_tokens":"bad"}}}`, false,
			usage.Tokens{Input: 15, CacheCreationInput: 2, CachedInput: 3, Output: 1}},
		{"overflowed delta retains start floor", rootStart + "\n" + `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"output_tokens":9223372036854775807}}}`, false,
			usage.Tokens{Input: 15, CacheCreationInput: 2, CachedInput: 3, Output: 1}},
		{"resumed stream cannot prove current usage", rootStart + "\n" + rootDelta, true, usage.Tokens{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{resumed: tc.resumed}
			writeChunks(t, &o, tc.lines+"\n")
			o.finish()
			if o.result.Tokens != tc.want || o.result.UsageComplete || o.nativeCostUSD != nil || o.err == nil {
				t.Fatalf("nested usage floor: tokens=%+v complete=%t cost=%v error=%v", o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.err)
			}
			if tc.want != (usage.Tokens{}) && (o.result.UsageCoverage.Scope != "known_subset" || !o.result.UsageCoverage.PartialReport) {
				t.Fatalf("incomplete known floor lost provenance: %+v", o.result.UsageCoverage)
			}
		})
	}
	o := jsonlOutput{}
	writeChunks(t, &o, rootStart+"\n"+rootDelta+"\n"+rootOlderDelta+"\n")
	o.finish()
	if !o.provenanceGap || o.result.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 2, CachedInput: 3, Output: 4}) {
		t.Fatalf("decreasing cumulative output did not retain prior floor and mark gap: tokens=%+v gap=%t", o.result.Tokens, o.provenanceGap)
	}
	o = jsonlOutput{}
	writeChunks(t, &o, cacheStart+"\n"+cacheReclassified+"\n")
	o.finish()
	if !o.provenanceGap || o.result.Tokens != (usage.Tokens{Input: 10, CacheCreationInput: 5, Output: 2}) {
		t.Fatalf("decreasing cumulative cache counter did not retain prior floor and mark gap: tokens=%+v gap=%t", o.result.Tokens, o.provenanceGap)
	}
	o = jsonlOutput{progressEntryLimit: 1}
	writeChunks(t, &o, rootStart+"\n")
	o.finish()
	if !errors.Is(o.err, aicli.ErrOutputLimit) || o.result.Tokens != (usage.Tokens{}) || o.result.UsageComplete {
		t.Fatalf("nested stream floor escaped progress budget: tokens=%+v complete=%t error=%v", o.result.Tokens, o.result.UsageComplete, o.err)
	}
	o = jsonlOutput{}
	writeChunks(t, &o, helperFull+"\n"+
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"overflow-root","usage":{"output_tokens":9223372036854775807}}}}`+"\n")
	o.finish()
	if o.result.Tokens != (usage.Tokens{Output: math.MaxInt64}) || o.result.UsageComplete || !o.provenanceGap {
		t.Fatalf("overflowed partial lost the largest valid floor: tokens=%+v complete=%t gap=%t error=%v", o.result.Tokens, o.result.UsageComplete, o.provenanceGap, o.err)
	}
	o = jsonlOutput{}
	writeChunks(t, &o, rootStart+"\n"+
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"second","usage":{"output_tokens":9223372036854775807}}}}`+"\n")
	o.finish()
	if o.result.Tokens != (usage.Tokens{Output: math.MaxInt64}) || o.result.UsageComplete || !o.provenanceGap {
		t.Fatalf("overflowed stream sum lost representable message floor: tokens=%+v gap=%t", o.result.Tokens, o.provenanceGap)
	}
	o = jsonlOutput{}
	writeChunks(t, &o, `{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":0,"cache_creation_input_tokens":8,"cache_read_input_tokens":0,"output_tokens":0}}}`+"\n"+
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":9,"output_tokens":0}}}}`+"\n")
	o.finish()
	if o.result.Tokens != (usage.Tokens{Input: 9, CachedInput: 9}) || o.result.UsageComplete {
		t.Fatalf("conflicting unallocated and stream cache floors lost valid subtotal: tokens=%+v complete=%t error=%v", o.result.Tokens, o.result.UsageComplete, o.err)
	}
	o = jsonlOutput{}
	writeChunks(t, &o, `{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":12,"cache_creation_input_tokens":8,"cache_read_input_tokens":0,"output_tokens":0}}}`+"\n"+
		`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root","usage":{"input_tokens":12,"cache_creation_input_tokens":0,"cache_read_input_tokens":8,"output_tokens":0}}}}`+"\n")
	o.finish()
	if o.result.Tokens != (usage.Tokens{Input: 20, CachedInput: 8}) || o.result.UsageComplete {
		t.Fatalf("valid cache-category max invented an unreported combination: tokens=%+v complete=%t error=%v", o.result.Tokens, o.result.UsageComplete, o.err)
	}
}

func TestOutputNestedStreamFloorChecksFinalResultAndCost(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":4}}}}`
	const missing = `{"type":"result","subtype":"success","result_index":0,"result":"done","total_cost_usd":0.5}`
	const malformed = `{"type":"result","subtype":"success","result_index":0,"result":"done","total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":"bad"}}}`
	const underreported = `{"type":"result","subtype":"success","result_index":0,"result":"done","total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const covering = `{"type":"result","subtype":"success","result_index":0,"result":"done","total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":4}}}`
	for _, tc := range []struct {
		name, result string
		complete     bool
	}{
		{"missing usage", missing, false},
		{"malformed usage", malformed, false},
		{"underreported usage", underreported, false},
		{"covering usage", covering, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, start+"\n"+tc.result+"\n")
			o.finish()
			want := usage.Tokens{Input: 10, Output: 4}
			if o.err != nil || o.result.Tokens != want || o.result.UsageComplete != tc.complete ||
				(o.nativeCostUSD != nil) != tc.complete || o.result.EstimatedCostUSD == nil || *o.result.EstimatedCostUSD != 0.5 {
				t.Fatalf("nested floor/result reconciliation: tokens=%+v complete=%t nativeCost=%v estimate=%v error=%v",
					o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.result.EstimatedCostUSD, o.err)
			}
		})
	}
	const cacheStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root","usage":{"input_tokens":5,"cache_creation_input_tokens":5,"output_tokens":1}}}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const result = `{"type":"result","subtype":"success","result_index":0,"result":"done","total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":4}}}`
	o := jsonlOutput{}
	writeChunks(t, &o, cacheStart+"\n"+full+"\n"+result+"\n")
	o.finish()
	if o.err != nil || o.result.UsageComplete || o.nativeCostUSD != nil ||
		o.result.Tokens != (usage.Tokens{Input: 10, CacheCreationInput: 5, Output: 4}) {
		t.Fatalf("full receipt reclassified stream cache without incomplete coverage: tokens=%+v complete=%t cost=%v gap=%t error=%v",
			o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.provenanceGap, o.err)
	}
}

func TestOutputNestedUsageAcrossIndexedResults(t *testing.T) {
	const cacheStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root","usage":{"cache_read_input_tokens":10,"output_tokens":1}}}}`
	const cacheZero = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const sameCacheZero = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const emptyFirst = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.1,"modelUsage":{"model":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`
	const freshStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"after-result","usage":{"input_tokens":10,"output_tokens":1}}}}`
	const freshDelta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"output_tokens":4}}}`
	const unchangedSecond = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.1,"modelUsage":{"model":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`
	const coveringSecond = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.2,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":4}}}`
	const fullStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root","usage":{"input_tokens":10,"output_tokens":1}}}}`
	const fullPlaceholder = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const lateCacheDelta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"cache_read_input_tokens":5,"output_tokens":4}}}`
	const cacheCovered = `{"type":"result","subtype":"success","result":"done","result_index":0,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":5,"outputTokens":4}}}`
	const helperFullCache = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":10,"output_tokens":1}}}`
	const helperMasksRootCache = `{"type":"result","subtype":"success","result":"done","result_index":0,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":10,"outputTokens":2}}}`
	const regularReclassifiedFull = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","usage":{"input_tokens":5,"cache_creation_input_tokens":5,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const regularUnderreported = `{"type":"result","subtype":"success","result":"done","result_index":0,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":5,"cacheCreationInputTokens":5,"cacheReadInputTokens":0,"outputTokens":1}}}`
	for _, tc := range []struct {
		name, events string
		want         usage.Tokens
		complete     bool
	}{
		{"second result cannot erase stream cache", cacheStart + "\n" + cacheZero + "\n" + sameCacheZero,
			usage.Tokens{Input: 10, CachedInput: 10, Output: 1}, false},
		{"helper category cannot mask root stream cache", cacheStart + "\n" + helperFullCache + "\n" + helperMasksRootCache,
			usage.Tokens{Input: 20, CachedInput: 20, Output: 2}, false},
		{"full category cannot mask regular stream input", fullStart + "\n" + regularReclassifiedFull + "\n" + regularUnderreported,
			usage.Tokens{Input: 10, Output: 1}, false},
		{"new stream after provisional result underreported", emptyFirst + "\n" + freshStart + "\n" + freshDelta + "\n" + unchangedSecond,
			usage.Tokens{Input: 10, Output: 4}, false},
		{"new stream after provisional result covered", emptyFirst + "\n" + freshStart + "\n" + freshDelta + "\n" + coveringSecond,
			usage.Tokens{Input: 10, Output: 4}, true},
		{"late cache delta after start-time full frame covered", fullStart + "\n" + fullPlaceholder + "\n" + lateCacheDelta + "\n" + cacheCovered,
			usage.Tokens{Input: 15, CachedInput: 5, Output: 4}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.events+"\n")
			o.finish()
			if o.err != nil || o.result.Tokens != tc.want || o.result.UsageComplete != tc.complete ||
				(o.nativeCostUSD != nil) != tc.complete {
				t.Fatalf("indexed stream usage: tokens=%+v complete=%t cost=%v gap=%t error=%v",
					o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.provenanceGap, o.err)
			}
		})
	}
	const firstOnly = `{"type":"result","subtype":"success","result":"first","result_index":0,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const lateStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"after-result","usage":{"input_tokens":5,"output_tokens":2}}}}`
	const lateFull = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"after-result","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2}}}`
	for _, later := range []string{"", lateFull + "\n"} {
		o := jsonlOutput{}
		writeChunks(t, &o, firstOnly+"\n"+lateStart+"\n"+later)
		o.finish()
		if o.err == nil || o.result.UsageComplete || o.result.Tokens != (usage.Tokens{Input: 15, Output: 3}) {
			t.Fatalf("fresh late stream did not add once to prior proved result: full=%t tokens=%+v complete=%t error=%v",
				later != "", o.result.Tokens, o.result.UsageComplete, o.err)
		}
	}
	const lateCovering = `{"type":"result","subtype":"success","result":"done","result_index":1,"modelUsage":{"model":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":3}}}`
	o := jsonlOutput{}
	writeChunks(t, &o, firstOnly+"\n"+lateStart+"\n"+lateCovering+"\n")
	o.finish()
	if o.err != nil || !o.result.UsageComplete || o.result.Tokens != (usage.Tokens{Input: 15, Output: 3}) {
		t.Fatalf("covering second result did not retire late floor: tokens=%+v complete=%t error=%v",
			o.result.Tokens, o.result.UsageComplete, o.err)
	}
	const noUsageStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"earlier"}}}`
	const oldDelta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"output_tokens":5}}}`
	o = jsonlOutput{}
	writeChunks(t, &o, noUsageStart+"\n"+firstOnly+"\n"+oldDelta+"\n")
	o.finish()
	if o.err == nil || o.result.UsageComplete || o.result.Tokens != (usage.Tokens{Input: 10, Output: 5}) {
		t.Fatalf("old stream start became a new additive post-result ID: tokens=%+v complete=%t error=%v",
			o.result.Tokens, o.result.UsageComplete, o.err)
	}
}

func TestOutputIndexedResultLimitPreservesKnownSubtotal(t *testing.T) {
	const atLimit = `{"type":"result","subtype":"success","result":"done","result_index":127,"modelUsage":{"model":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	o := jsonlOutput{results: maxResultEvents - 1, indexed: true}
	writeChunks(t, &o, atLimit+"\n")
	o.finish()
	if o.err != nil || o.results != maxResultEvents || !o.result.UsageComplete {
		t.Fatalf("last allowed indexed result failed: results=%d complete=%t error=%v", o.results, o.result.UsageComplete, o.err)
	}
	const beyond = `{"type":"result","subtype":"success","result":"ignored","result_index":128,"modelUsage":{"model":{"inputTokens":99,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":99}}}`
	o = jsonlOutput{results: maxResultEvents, indexed: true, result: aicli.Result{Tokens: usage.Tokens{Input: 1, Output: 1}, UsageComplete: true}}
	writeChunks(t, &o, beyond+"\n")
	if !errors.Is(o.err, aicli.ErrOutputLimit) || o.result.Tokens != (usage.Tokens{Input: 1, Output: 1}) || o.result.UsageComplete {
		t.Fatalf("result cap lost known subtotal or accepted excess work: tokens=%+v complete=%t error=%v", o.result.Tokens, o.result.UsageComplete, o.err)
	}
}

// Defensive native shape: current SDK docs describe helper full frames, not helper stream events.
const defensiveHelperStreamUsage = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"helper","usage":{"input_tokens":5,"output_tokens":1}}}}`

func TestOutputRootOnlyResultKeepsDistinctLaterUsage(t *testing.T) {
	const rootOnly = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.25,"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}`
	const underreportedRoot = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.25,"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1},"modelUsage":{"model":{"inputTokens":5,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const lateRoot = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"later","usage":{"input_tokens":5,"output_tokens":1}}}}`
	const lateRootNoUsage = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"later"}}}`
	replacementStart := strings.Replace(lateRootNoUsage, `"id":"later"`, `"id":"replacement"`, 1)
	const lateRootDelta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}` + "\n" + `{"type":"stream_event","uuid":"later-chunk","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"later"}}}`
	const lateRootFull = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"later","content":[{"type":"text","text":"later"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const lateRootNoContentFull = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"later","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const lateRootStop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_stop"}}`
	const lateRootComplete = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_stop","index":0}}` + "\n" + `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"output_tokens":1}}}` + "\n" + lateRootStop
	const lateHelper = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const lateHelperContent = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[{"type":"text","text":"helper work"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const insufficient = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const covering = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	for _, tc := range []struct {
		name, events     string
		complete, failed bool
	}{
		{"late root no later result", rootOnly + "\n" + lateRoot, false, true},
		{"late root underreported result", rootOnly + "\n" + lateRoot + "\n" + insufficient, false, false},
		{"late root covering result", rootOnly + "\n" + lateRoot + "\n" + covering, true, false},
		{"late root stream and full counted once", rootOnly + "\n" + lateRoot + "\n" + lateRootFull + "\n" + insufficient, false, false},
		{"late sparse root underreported result", rootOnly + "\n" + lateRootNoUsage + "\n" + lateRootDelta + "\n" + lateRootFull + "\n" + insufficient, false, false},
		{"late sparse stopped root covering result", rootOnly + "\n" + lateRootNoUsage + "\n" + lateRootDelta + "\n" + lateRootFull + "\n" + lateRootStop + "\n" + covering, true, false},
		{"late sparse completed root covering result", rootOnly + "\n" + lateRootNoUsage + "\n" + lateRootDelta + "\n" + lateRootFull + "\n" + lateRootComplete + "\n" + covering, true, false},
		{"late full-only root underreported result", rootOnly + "\n" + lateRootFull + "\n" + insufficient, false, false},
		{"late full-only root covering result", rootOnly + "\n" + lateRootFull + "\n" + covering, true, false},
		{"late helper underreported result", rootOnly + "\n" + lateHelper + "\n" + insufficient, false, false},
		{"late helper stream underreported result", rootOnly + "\n" + defensiveHelperStreamUsage + "\n" + insufficient, false, false},
		{"late helper stream and full counted once", rootOnly + "\n" + defensiveHelperStreamUsage + "\n" + lateHelper + "\n" + insufficient, false, false},
		{"late helper covering result", rootOnly + "\n" + lateHelper + "\n" + covering, true, false},
		{"content-bearing late helper underreported result", rootOnly + "\n" + lateHelperContent + "\n" + insufficient, false, false},
		{"helper before root-only result", lateHelper + "\n" + rootOnly, false, false},
		{"content-bearing helper before root-only result", lateHelperContent + "\n" + rootOnly, false, false},
		{"helper stream before root-only result", defensiveHelperStreamUsage + "\n" + rootOnly, false, false},
		{"underreported model does not prove helpers", underreportedRoot + "\n" + lateHelper + "\n" + insufficient, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.events+"\n")
			o.finish()
			if (o.err != nil) != tc.failed || o.result.Tokens != (usage.Tokens{Input: 15, Output: 2}) ||
				o.result.UsageComplete != tc.complete || (o.nativeCostUSD != nil) != tc.complete {
				t.Fatalf("root-only known subtotal: tokens=%+v complete=%t cost=%v error=%v",
					o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.err)
			}
		})
	}
	o := jsonlOutput{}
	writeChunks(t, &o, lateRootNoUsage+"\n"+lateRootDelta+"\n"+rootOnly+"\n"+lateRootFull+"\n"+insufficient+"\n")
	o.finish()
	if o.err != nil || o.result.Tokens != (usage.Tokens{Input: 10, Output: 1}) || !o.result.UsageComplete {
		t.Fatalf("pre-result stream full receipt counted again: tokens=%+v complete=%t error=%v",
			o.result.Tokens, o.result.UsageComplete, o.err)
	}
	secondHelper := strings.Replace(lateHelper, `"id":"helper"`, `"id":"helper-2"`, 1)
	const thirdResult = `{"type":"result","subtype":"success","result":"third","result_index":2,"total_cost_usd":0.75,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const thirdCovering = `{"type":"result","subtype":"success","result":"third","result_index":2,"total_cost_usd":0.75,"modelUsage":{"model":{"inputTokens":20,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":3}}}`
	for _, tc := range []struct {
		name, final string
		complete    bool
	}{
		{"interrupted", "", false},
		{"still underreported", thirdResult, false},
		{"covering result", thirdCovering, true},
	} {
		t.Run("chained helpers/"+tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, rootOnly+"\n"+lateHelper+"\n"+insufficient+"\n"+secondHelper+"\n"+tc.final+"\n")
			o.finish()
			if o.err != nil || o.result.Tokens != (usage.Tokens{Input: 20, Output: 3}) ||
				o.result.UsageComplete != tc.complete || (o.nativeCostUSD != nil) != tc.complete {
				t.Fatalf("chained root-only boundary: tokens=%+v complete=%t cost=%v error=%v",
					o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.err)
			}
		})
	}
	for _, tc := range []struct{ name, stream, full string }{
		{"text", lateRootNoUsage + "\n" + lateRootDelta, lateRootFull},
		{"content-free", lateRootNoUsage, lateRootNoContentFull},
	} {
		t.Run("cross-result stream/"+tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, rootOnly+"\n"+tc.stream+"\n"+insufficient+"\n"+tc.full+"\n"+thirdResult+"\n")
			o.finish()
			if o.err != nil || !o.provenanceGap || o.result.UsageComplete || o.nativeCostUSD != nil {
				t.Fatalf("unsupported cross-result stream certified exact usage: tokens=%+v complete=%t cost=%v error=%v",
					o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.err)
			}
		})
	}
	for _, tc := range []struct{ name, events string }{
		{"replaced stream", rootOnly + "\n" + lateRootNoUsage + "\n" + lateRootDelta + "\n" + insufficient + "\n" + replacementStart + "\n" + lateRootFull + "\n" + thirdResult},
		{"replaced before result", rootOnly + "\n" + lateRootNoUsage + "\n" + lateRootDelta + "\n" + replacementStart + "\n" + insufficient + "\n" + lateRootFull + "\n" + thirdResult},
		{"metadata before replacement", rootOnly + "\n" + lateRootNoUsage + "\n" + lateRootDelta + "\n" + `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"later"}}` + "\n" + replacementStart + "\n" + insufficient + "\n" + lateRootFull + "\n" + thirdResult},
		{"start before first result", lateRootNoUsage + "\n" + rootOnly + "\n" + insufficient + "\n" + lateRootNoContentFull + "\n" + thirdResult},
		{"text before first result", lateRootNoUsage + "\n" + lateRootDelta + "\n" + rootOnly + "\n" + insufficient + "\n" + lateRootFull + "\n" + thirdResult},
	} {
		t.Run("unsupported late full/"+tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.events+"\n")
			o.finish()
			if o.err != nil || !o.provenanceGap || o.result.UsageComplete || o.nativeCostUSD != nil || o.result.Tokens != (usage.Tokens{Input: 10, Output: 1}) {
				t.Fatalf("unsupported late full certified or inflated usage: tokens=%+v complete=%t cost=%v error=%v", o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.err)
			}
		})
	}
	t.Run("exact late replay", func(t *testing.T) {
		const thirdSame = `{"type":"result","subtype":"success","result":"third","result_index":2,"total_cost_usd":0.75,"modelUsage":{"model":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
		var events []aicli.StreamEvent
		o := jsonlOutput{onEvent: func(e aicli.StreamEvent) { events = append(events, e) }}
		writeChunks(t, &o, rootOnly+"\n"+lateRootNoUsage+"\n"+lateRootDelta+"\n"+lateRootFull+"\n"+lateRootComplete+"\n"+covering+"\n")
		beforeReplay := len(events)
		writeChunks(t, &o, lateRootFull+"\n"+thirdSame+"\n")
		o.finish()
		if o.err != nil || o.provenanceGap || o.result.Tokens != (usage.Tokens{Input: 15, Output: 2}) || !o.result.UsageComplete || o.nativeCostUSD == nil || len(events) != beforeReplay {
			t.Fatalf("exact late replay changed settled usage or emitted content: tokens=%+v complete=%t cost=%v events=%d/%d error=%v", o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, len(events), beforeReplay, o.err)
		}
	})
	for _, tc := range []struct{ name, changed string }{
		{"usage", strings.Replace(lateRootFull, `"output_tokens":1`, `"output_tokens":2`, 1)},
		{"content", strings.Replace(lateRootFull, `"text":"later"`, `"text":"changed"`, 1)},
	} {
		t.Run("changed late replay/"+tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, rootOnly+"\n"+lateRootNoUsage+"\n"+lateRootDelta+"\n"+lateRootFull+"\n"+lateRootComplete+"\n"+covering+"\n"+tc.changed+"\n"+thirdResult+"\n")
			o.finish()
			if o.err != nil || !o.provenanceGap || o.result.UsageComplete || o.nativeCostUSD != nil {
				t.Fatalf("changed late replay certified exact cost: tokens=%+v complete=%t cost=%v error=%v", o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.err)
			}
		})
	}
}

func TestOutputFullResultCannotCertifyDelayedHelperWithoutCoverage(t *testing.T) {
	const full = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.25,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const helper = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const insufficient = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const covering = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	for _, event := range []struct{ name, value string }{{"full receipt", helper}, {"stream receipt", defensiveHelperStreamUsage}} {
		for _, tc := range []struct {
			name, second string
			want         usage.Tokens
			complete     bool
		}{
			{"underreported", insufficient, usage.Tokens{Input: 10, Output: 1}, false},
			{"covering", covering, usage.Tokens{Input: 15, Output: 2}, true},
		} {
			t.Run(event.name+"/"+tc.name, func(t *testing.T) {
				o := jsonlOutput{}
				writeChunks(t, &o, full+"\n"+event.value+"\n"+tc.second+"\n")
				o.finish()
				if o.err != nil || o.result.Tokens != tc.want || o.result.UsageComplete != tc.complete ||
					(o.nativeCostUSD != nil) != tc.complete {
					t.Fatalf("delayed helper coverage: tokens=%+v complete=%t cost=%v error=%v",
						o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.err)
				}
			})
		}
	}
}

func TestOutputCachedFinalCannotEraseKnownRegularInput(t *testing.T) {
	const root = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.25,"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}`
	const helper = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":5,"output_tokens":1}}}`
	const cached = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":15,"outputTokens":2}}}`
	o := jsonlOutput{}
	writeChunks(t, &o, root+"\n"+helper+"\n"+cached+"\n")
	o.finish()
	if o.err != nil || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 5, Output: 2}) ||
		o.result.UsageComplete || o.nativeCostUSD != nil {
		t.Fatalf("cached report erased regular input floor: tokens=%+v complete=%t cost=%v error=%v",
			o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.err)
	}
}

func TestOutputSameIDContradictoryCacheCannotCertifyCost(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, model string
		want                       usage.Tokens
		wantImpossible             bool
		historyWant                usage.Tokens
	}{
		{"regular to cached", `{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}`, `{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":10,"output_tokens":1}`, `{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":10,"outputTokens":1}`, usage.Tokens{Input: 10, CachedInput: 10, Output: 1}, false, usage.Tokens{}},
		{"incompatible cache tuple", `{"input_tokens":0,"cache_creation_input_tokens":10,"cache_read_input_tokens":0,"output_tokens":1}`, `{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":15,"output_tokens":1}`, `{"inputTokens":0,"cacheCreationInputTokens":10,"cacheReadInputTokens":0,"outputTokens":1}`, usage.Tokens{Input: 15, CachedInput: 15, Output: 1}, true, usage.Tokens{Input: 15, CacheCreationInput: 10, CachedInput: 15, Output: 1}},
		{"equal input, higher output", `{"input_tokens":0,"cache_creation_input_tokens":10,"cache_read_input_tokens":0,"output_tokens":1}`, `{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":10,"output_tokens":2}`, `{"inputTokens":0,"cacheCreationInputTokens":10,"cacheReadInputTokens":0,"outputTokens":1}`, usage.Tokens{Input: 10, CacheCreationInput: 10, Output: 2}, true, usage.Tokens{Input: 10, CacheCreationInput: 10, CachedInput: 10, Output: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"same","content":[{"type":"text","text":"first"}],"usage":` + tc.first + `}}`
			second := `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"same","content":[{"type":"text","text":"second"}],"usage":` + tc.second + `}}`
			result := `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.25,"modelUsage":{"model":` + tc.model + `}}`
			o := jsonlOutput{}
			writeChunks(t, &o, first+"\n"+second+"\n"+result+"\n")
			o.finish()
			if o.err != nil || !o.provenanceGap || o.result.UsageComplete || o.nativeCostUSD != nil || o.result.Tokens != tc.want || o.assistantTotalOverflow != tc.wantImpossible {
				t.Fatalf("contradictory same-ID input lost floor or certified cost: tokens=%+v complete=%t cost=%v error=%v", o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.err)
			}
			if tc.wantImpossible {
				history := o.assistantHistory[streamIdentity{"root", "same"}]
				if !history.hasTokens || history.tokens.Valid() || history.tokens != tc.historyWant {
					t.Fatalf("impossible same-ID history was lost: %+v", history)
				}
			}
		})
	}
}

func TestOutputContradictoryStreamUsageKeepsValidStandaloneFloor(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root","usage":{"input_tokens":0,"cache_creation_input_tokens":10,"cache_read_input_tokens":0}}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_delta","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":15}}}`
	const stop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_stop"}}`
	const result = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.25,"modelUsage":{"model":{"inputTokens":0,"cacheCreationInputTokens":10,"cacheReadInputTokens":0,"outputTokens":0}}}`
	o := jsonlOutput{}
	writeChunks(t, &o, start+"\n"+delta+"\n"+stop+"\n"+result+"\n")
	o.finish()
	if o.err != nil || !o.provenanceGap || o.result.UsageComplete || o.nativeCostUSD != nil || o.result.Tokens != (usage.Tokens{Input: 15, CachedInput: 15}) {
		t.Fatalf("contradictory stream snapshot lost valid floor: tokens=%+v complete=%t cost=%v error=%v", o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.err)
	}
}

func TestOutputRevisedAcceptedIDCannotRecertifyUnderreportedResult(t *testing.T) {
	const rootOnly = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.25,"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}`
	const helper = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[{"type":"text","text":"one"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const revised = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[{"type":"text","text":"one"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2}}}`
	const result = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	const final = `{"type":"result","subtype":"success","result":"third","result_index":2,"total_cost_usd":0.75,"modelUsage":{"model":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root"}}}`
	const rootFull = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","content":[{"type":"text","text":"one"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	rootRevised := strings.Replace(rootFull, `"output_tokens":1`, `"output_tokens":2`, 1)
	replacement := strings.Replace(start, `"id":"root"`, `"id":"replacement"`, 1)
	for _, tc := range []struct{ name, events string }{
		{"helper", rootOnly + "\n" + helper + "\n" + result + "\n" + revised + "\n" + final},
		{"helper before first result", helper + "\n" + rootOnly + "\n" + result + "\n" + revised + "\n" + final},
		{"replaced root", rootOnly + "\n" + start + "\n" + rootFull + "\n" + result + "\n" + replacement + "\n" + rootRevised + "\n" + final},
		{"replayed start", rootOnly + "\n" + start + "\n" + rootFull + "\n" + result + "\n" + replacement + "\n" + start + "\n" + rootRevised + "\n" + final},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.events+"\n")
			o.finish()
			if o.err != nil || !o.provenanceGap || o.result.UsageComplete || o.nativeCostUSD != nil || o.result.Tokens != (usage.Tokens{Input: 15, Output: 2}) {
				t.Fatalf("revised accepted ID recertified underreported cost: tokens=%+v complete=%t cost=%v error=%v", o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.err)
			}
		})
	}
}

func TestOutputChangedPostResultBlockCannotRecertifyCost(t *testing.T) {
	const first = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.25,"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}`
	const helper = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[{"type":"text","text":"one"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const covering = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	const final = `{"type":"result","subtype":"success","result":"third","result_index":2,"total_cost_usd":0.75,"modelUsage":{"model":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	for _, tc := range []struct{ name, usageMember string }{
		{"unchanged counters", `,"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}`},
		{"missing counters", ``},
		{"malformed counters", `,"usage":{"input_tokens":"bad"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revised := `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[{"type":"text","text":"two"}]` + tc.usageMember + `}}`
			o := jsonlOutput{}
			writeChunks(t, &o, first+"\n"+helper+"\n"+covering+"\n"+revised+"\n"+final+"\n")
			o.finish()
			if o.err != nil || !o.provenanceGap || o.result.UsageComplete || o.nativeCostUSD != nil || o.result.Tokens != (usage.Tokens{Input: 15, Output: 2}) {
				t.Fatalf("changed helper block certified exact cost: tokens=%+v complete=%t cost=%v gap=%t error=%v",
					o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.provenanceGap, o.err)
			}
		})
	}
}

func TestOutputReplayedStartCannotDiscardUnacceptedContentProof(t *testing.T) {
	const first = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.25,"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}`
	const startA = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"A"}}}`
	const startB = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"B"}}}`
	const blockB = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`
	const deltaB = `{"type":"stream_event","uuid":"b-delta","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"work"}}}`
	const second = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const fullB = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"B","content":[{"type":"text","text":"work"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const third = `{"type":"result","subtype":"success","result":"third","result_index":2,"total_cost_usd":0.75,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	for _, tc := range []struct{ name, prefix string }{
		{"stream started after R0", first + "\n" + startA + "\n" + startB},
		{"stream started before R0", startA + "\n" + startB + "\n" + first},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.prefix+"\n"+second+"\n"+blockB+"\n"+deltaB+"\n"+startA+"\n"+fullB+"\n"+third+"\n")
			o.finish()
			if o.err != nil || !o.provenanceGap || o.result.UsageComplete || o.nativeCostUSD != nil || o.result.Tokens != (usage.Tokens{Input: 10, Output: 1}) {
				t.Fatalf("replayed start discarded unaccepted B proof: tokens=%+v complete=%t cost=%v gap=%t error=%v",
					o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.provenanceGap, o.err)
			}
		})
	}
}

func TestOutputSameIDBlocksBeforeNextResultCanComplete(t *testing.T) {
	const first = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.25,"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}`
	const blockOne = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[{"type":"text","text":"one"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const blockTwo = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[{"type":"text","text":"two"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2}}}`
	const covering = `{"type":"result","subtype":"success","result":"done","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":3}}}`
	o := jsonlOutput{}
	writeChunks(t, &o, first+"\n"+blockOne+"\n"+blockTwo+"\n"+covering+"\n")
	o.finish()
	if o.err != nil || o.provenanceGap || !o.result.UsageComplete || o.nativeCostUSD == nil || o.result.Tokens != (usage.Tokens{Input: 15, Output: 3}) {
		t.Fatalf("pre-result blocks lost exact coverage: tokens=%+v complete=%t cost=%v gap=%t error=%v",
			o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.provenanceGap, o.err)
	}
}

func TestOutputWeakerCacheRevisionKeepsEarlierValidFloor(t *testing.T) {
	const first = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"same","usage":{"input_tokens":0,"cache_creation_input_tokens":15,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const weaker = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"same","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":10,"output_tokens":1}}}`
	const result = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.25,"modelUsage":{"model":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":10,"outputTokens":1}}}`
	o := jsonlOutput{}
	writeChunks(t, &o, first+"\n"+weaker+"\n"+result+"\n")
	o.finish()
	if o.err != nil || !o.provenanceGap || o.result.UsageComplete || o.nativeCostUSD != nil ||
		o.result.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 15, Output: 1}) {
		t.Fatalf("weaker cache revision erased earlier floor: tokens=%+v complete=%t cost=%v gap=%t error=%v",
			o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.provenanceGap, o.err)
	}
}

func TestOutputSealedDeltaRevisionCannotRecertifyCost(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root"}}}`
	const block = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`
	const delta = `{"type":"stream_event","uuid":"chunk","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"one"}}}`
	const newBlock = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}}`
	const newDelta = `{"type":"stream_event","uuid":"new-chunk","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"two"}}}`
	const newTool = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool","name":"Bash","input":{}}}}`
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","content":[{"type":"text","text":"one"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const first = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.25,"modelUsage":{"model":{"inputTokens":5,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const second = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":5,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	for _, tc := range []struct {
		name, after string
		complete    bool
	}{
		{"exact delta replay", delta, true},
		{"changed sealed delta", strings.Replace(delta, `"text":"one"`, `"text":"two"`, 1), false},
		{"new sealed text block", newBlock + "\n" + newDelta, false},
		{"new sealed tool block", newTool, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, start+"\n"+block+"\n"+delta+"\n"+full+"\n"+first+"\n"+tc.after+"\n"+second+"\n")
			o.finish()
			if o.err != nil || o.result.UsageComplete != tc.complete || (o.nativeCostUSD != nil) != tc.complete ||
				o.provenanceGap == tc.complete || o.result.Tokens != (usage.Tokens{Input: 5, Output: 1}) {
				t.Fatalf("sealed delta settlement: tokens=%+v complete=%t cost=%v gap=%t error=%v",
					o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.provenanceGap, o.err)
			}
		})
	}
}

func TestOutputCutStreamNeedsLargerFinalReport(t *testing.T) {
	const first = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.25,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root"}}}`
	const block = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`
	const delta = `{"type":"stream_event","uuid":"chunk","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"new work"}}}`
	for _, tc := range []struct {
		name, final string
		want        usage.Tokens
		complete    bool
	}{
		{"unchanged result", `{"type":"result","subtype":"success","result":"done","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`, usage.Tokens{Input: 10, Output: 1}, false},
		{"input only growth", `{"type":"result","subtype":"success","result":"done","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`, usage.Tokens{Input: 15, Output: 1}, false},
		{"larger native result", `{"type":"result","subtype":"success","result":"done","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":15,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`, usage.Tokens{Input: 15, Output: 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, first+"\n"+start+"\n"+block+"\n"+delta+"\n"+tc.final+"\n")
			o.finish()
			if o.err != nil || o.result.Tokens != tc.want || o.result.UsageComplete != tc.complete ||
				(o.nativeCostUSD != nil) != tc.complete || o.provenanceGap == tc.complete {
				t.Fatalf("cut stream coverage: tokens=%+v complete=%t cost=%v gap=%t error=%v",
					o.result.Tokens, o.result.UsageComplete, o.nativeCostUSD, o.provenanceGap, o.err)
			}
		})
	}
}

func TestOutputExactFullReplayDoesNotDuplicateTranscript(t *testing.T) {
	const full = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const result = `{"type":"result","subtype":"success","result":"answer","total_cost_usd":0.25,"modelUsage":{"model":{"inputTokens":5,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	var events []aicli.StreamEvent
	o := jsonlOutput{onEvent: func(e aicli.StreamEvent) { events = append(events, e) }}
	writeChunks(t, &o, full+"\n"+result+"\n")
	before := len(events)
	writeChunks(t, &o, full+"\n")
	o.finish()
	if o.err != nil || before != 1 || len(events) != before || o.provenanceGap || !o.result.UsageComplete ||
		o.result.Tokens != (usage.Tokens{Input: 5, Output: 1}) || o.nativeCostUSD == nil || *o.nativeCostUSD != 0.25 {
		t.Fatalf("exact full replay duplicated transcript or accounting: events=%d/%d tokens=%+v complete=%t gap=%t error=%v",
			len(events), before, o.result.Tokens, o.result.UsageComplete, o.provenanceGap, o.err)
	}
}

func TestOutputUnallocatedValidFloorSurvivesCacheConflict(t *testing.T) {
	const first = `{"type":"result","subtype":"success","result":"first","result_index":0,"total_cost_usd":0.25,"usage":{"input_tokens":0,"cache_creation_input_tokens":5,"cache_read_input_tokens":5,"output_tokens":0}}`
	const missingID = `{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":0,"cache_creation_input_tokens":20,"cache_read_input_tokens":0,"output_tokens":0}}}`
	const second = `{"type":"result","subtype":"success","result":"second","result_index":1,"total_cost_usd":0.5,"modelUsage":{"model":{"inputTokens":0,"cacheCreationInputTokens":5,"cacheReadInputTokens":5,"outputTokens":0}}}`
	o := jsonlOutput{}
	writeChunks(t, &o, first+"\n"+missingID+"\n"+second+"\n")
	o.finish()
	if o.err != nil || !o.provenanceGap || o.result.UsageComplete || o.nativeCostUSD != nil || o.result.Tokens != (usage.Tokens{Input: 20, CacheCreationInput: 20}) {
		t.Fatalf("valid unallocated floor was erased: tokens=%+v complete=%t error=%v", o.result.Tokens, o.result.UsageComplete, o.err)
	}
}

func TestOutputIndexedOverflowKeepsStrongestKnownReceipt(t *testing.T) {
	const first = `{"type":"result","subtype":"success","result":"first","result_index":0,"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}`
	small := assistantUsageEvent("small", 10, 0, 0, 0)
	large := assistantUsageEvent("large", math.MaxInt64-5, 0, 0, 0)
	for _, tc := range []struct{ name, receipts string }{
		{"small then large", small + "\n" + large},
		{"large then small", large + "\n" + small},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, first+"\n"+tc.receipts+"\n")
			o.finish()
			if o.result.Tokens != (usage.Tokens{Input: math.MaxInt64 - 5, Output: 1}) ||
				o.result.UsageComplete || !o.provenanceGap {
				t.Fatalf("overflow lost strongest standalone receipt: tokens=%+v complete=%t gap=%t error=%v",
					o.result.Tokens, o.result.UsageComplete, o.provenanceGap, o.err)
			}
		})
	}
	boundary := usage.Tokens{Input: 10, Output: 1}
	largeFloor := usage.Tokens{Input: math.MaxInt64 - 40}
	smallFloor := usage.Tokens{Input: 35}
	for i := 0; i < 100; i++ {
		o := jsonlOutput{results: 1, resultBoundary: boundary,
			assistantUsage: map[streamIdentity]messageUsage{
				{"root", "large"}: {tokens: largeFloor, resultIndex: 1, root: true},
				{"root", "small"}: {tokens: smallFloor, resultIndex: 1, root: true},
			}}
		post, _, valid := o.resultBoundaryEvidence()
		if valid || post != (usage.Tokens{Input: math.MaxInt64 - 30, Output: 1}) {
			t.Fatalf("overflow subtotal changed with map order: post=%+v valid=%t", post, valid)
		}
	}
	const complete = `{"type":"result","subtype":"success","result":"first","result_index":0,"modelUsage":{"model":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	o := jsonlOutput{}
	writeChunks(t, &o, complete+"\n"+assistantUsageEvent("large", math.MaxInt64-40, 0, 0, 0)+"\n"+assistantUsageEvent("small", 35, 0, 0, 0)+"\n")
	preFloor, preValid := o.assistantLowerBound()
	o.finish()
	if o.result.Tokens != (usage.Tokens{Input: math.MaxInt64 - 5, Output: 1}) || o.result.UsageComplete {
		t.Fatalf("valid receipt sum lost to prior-boundary overflow: tokens=%+v assistant=%+v boundary=%+v pre=%+v valid=%t complete=%t error=%v",
			o.result.Tokens, o.assistantTokens, o.resultBoundary, preFloor, preValid, o.result.UsageComplete, o.err)
	}
	for i := 0; i < 100; i++ {
		o := jsonlOutput{streamUsage: map[streamIdentity]streamUsageSample{
			{"root", "created"}: {created: math.MaxInt64 - 10},
			{"root", "cached"}:  {cached: math.MaxInt64 - 10},
		}}
		floor, _, _, valid := o.streamUsageLowerBound()
		if valid || floor != (usage.Tokens{Input: math.MaxInt64 - 10, CacheCreationInput: math.MaxInt64 - 10}) {
			t.Fatalf("stream overflow cache category changed with map order: floor=%+v valid=%t", floor, valid)
		}
	}
	const nearMax = `{"type":"result","subtype":"success","result":"first","result_index":0,"modelUsage":{"model":{"inputTokens":9223372036854775707,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`
	const helperOutput = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper-output","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":50}}}`
	o = jsonlOutput{}
	writeChunks(t, &o, nearMax+"\n"+assistantUsageEvent("root-input", 101, 0, 0, 0)+"\n"+helperOutput+"\n")
	o.finish()
	if o.result.Tokens != (usage.Tokens{Input: math.MaxInt64 - 100, Output: 50}) || o.result.UsageComplete {
		t.Fatalf("overflow erased independent helper output floor: tokens=%+v assistant=%+v error=%v",
			o.result.Tokens, o.assistantTokens, o.err)
	}
}

func TestDiagnosticRedactionAfterBounding(t *testing.T) {
	raw := "capacity reached " + strings.Repeat("A", 8192) + strings.Repeat("B", 8192) + " session resets"
	boundedText := aicli.DiagnosticText(raw)
	join := strings.Index(boundedText, "...")
	if join < 7 || join+10 > len(boundedText) {
		t.Fatalf("bounded diagnostic has no head/tail join: length=%d", len(boundedText))
	}
	key := boundedText[join-7 : join+10]
	if strings.Contains(raw, key) {
		t.Fatalf("test key already appears before bounding: %q", key)
	}
	o := jsonlOutput{apiKey: key}
	o.observe("task_stopped", raw)
	if len(o.result.Diagnostics) != 1 || o.result.Diagnostics[0].Code != "task_stopped" || strings.Contains(o.result.Diagnostics[0].Message, key) ||
		!strings.Contains(o.result.Diagnostics[0].Message, "capacity reached") || !strings.Contains(o.result.Diagnostics[0].Message, "session resets") || len(o.result.Diagnostics[0].Message) > bounded.DiagnosticBytes {
		t.Fatalf("bounded native observation exposed key or lost evidence: %+v", o.result.Diagnostics)
	}
	detail := o.safeResultDetail(raw)
	if strings.Contains(detail, key) || !strings.Contains(detail, "capacity reached") || !strings.Contains(detail, "session resets") || len(detail) > bounded.DiagnosticBytes || len(o.result.Diagnostics) != 1 {
		t.Fatalf("bounded result detail exposed key or lost evidence: detail=%q diagnostics=%+v", detail, o.result.Diagnostics)
	}
}

func TestUnsafeResultDetailMarksDiagnosticGap(t *testing.T) {
	o := jsonlOutput{apiKey: `"x"`}
	if detail := o.safeResultDetail(`{"x":1}`); detail != "native result detail could not be safely displayed" ||
		len(o.result.Diagnostics) != 1 || o.result.Diagnostics[0].Code != "diagnostics_truncated" {
		t.Fatalf("discarded result detail failed to mark incomplete diagnostics: detail=%q diagnostics=%+v", detail, o.result.Diagnostics)
	}
	o = jsonlOutput{apiKey: `"x"`}
	o.observe("task_stopped", `{"x":1}`)
	if len(o.result.Diagnostics) != 2 || o.result.Diagnostics[0].Code != "diagnostics_truncated" ||
		o.result.Diagnostics[1] != (aicli.ProviderDiagnostic{Code: "task_stopped", Message: "native diagnostic could not be safely displayed"}) {
		t.Fatalf("discarded native observation failed to mark incomplete diagnostics: %+v", o.result.Diagnostics)
	}
}

func TestOutputNativeUsageLimit(t *testing.T) {
	// Native events: overage rejection can accompany an allowed warning.
	const warning = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1792200000,"rateLimitType":"five_hour","utilization":0.99,"isUsingOverage":false,"overageStatus":"rejected","overageDisabledReason":"org_level_disabled","overageEnabled":false,"surpassedThreshold":0.9,"unifiedWindows":{"five_hour":{"utilization":0.99,"resetsAt":1792200000},"seven_day":{"utilization":0.24,"resetsAt":1792342800}}}}`
	const rejected = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792200000,"rateLimitType":"five_hour","isUsingOverage":false,"overageStatus":"rejected","overageDisabledReason":"org_level_disabled","unifiedWindows":{"five_hour":{"utilization":1,"resetsAt":1792200000},"seven_day":{"utilization":0.24,"resetsAt":1792342800}}}}`
	// A quota failure can carry subtype=success and cumulative helper usage.
	const failed = `{"type":"result","subtype":"success","is_error":true,"result_index":0,"result":"PRIVATE_RESULT\nforged journal line","errors":["PRIVATE_ERROR"],"modelUsage":{"claude-opus-5-5":{"inputTokens":36,"cacheCreationInputTokens":160092,"cacheReadInputTokens":435540,"outputTokens":11489}}}`
	const limit = "provider usage limit reached; resets at 2026-10-17T01:20:00Z"
	known := usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}
	for _, test := range []struct {
		name, data   string
		want         usage.Tokens
		failsAtWrite bool
		failure      string
	}{
		{"warning permits completion", warning + "\n" + successfulResult, known, false, ""},
		{"paid overage permits completion", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","isUsingOverage":true,"overageStatus":"allowed"}}` + "\n" + successfulResult, known, false, ""},
		{"active overage permits completion", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","overageInUse":true}}` + "\n" + successfulResult, known, false, ""},
		{"native failure keeps helper counters", warning + "\n" + rejected + "\n" + failed, usage.Tokens{Input: 595668, CacheCreationInput: 160092, CachedInput: 435540, Output: 11489}, true, limit},
		{"rejection at EOF", rejected, usage.Tokens{}, false, limit},
		{"later counters parsed before cancellation", rejected + "\n" + successfulResult, known, true, limit},
		{"warning cannot clear rejection", rejected + "\n" + warning + "\n" + successfulResult, known, true, limit},
		{"rejection invalidates earlier answer", successfulResult + "\n" + rejected, known, false, limit},
		{"limit takes precedence over denials", rejected + "\n" + strings.TrimSuffix(successfulResult, "}") + `,"permission_denials":[{"tool_name":"Bash"}]}`, known, true, limit},
		{"limit takes precedence over invalid index", rejected + "\n" + strings.TrimSuffix(successfulResult, "}") + `,"result_index":9}`, known, true, limit},
		{"limit takes precedence over unfinished tasks", rejected + "\n" + `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}`, usage.Tokens{}, false, limit},
	} {
		t.Run(test.name, func(t *testing.T) {
			cancelled := false
			o := jsonlOutput{cancel: func() { cancelled = true }}
			data := test.data + "\n"
			writeChunks(t, &o, data)
			if (o.err != nil) != test.failsAtWrite || cancelled != test.failsAtWrite || o.result.Tokens != test.want {
				t.Fatalf("usage limit interrupted counter collection: tokens=%+v error=%v cancelled=%t", o.result.Tokens, o.err, cancelled)
			}
			o.finish()
			if test.failure == "" {
				if o.err != nil || cancelled || !o.result.UsageComplete || o.result.Answer != "final answer" {
					t.Fatalf("warning refused successful result: %+v error=%v", o.result, o.err)
				}
			} else if o.err == nil || o.err.Error() != test.failure || !cancelled || o.result.UsageComplete || o.result.Answer != "" {
				t.Fatalf("unsafe or missing usage limit failure: tokens=%+v error=%v complete=%t answer=%q", o.result.Tokens, o.err, o.result.UsageComplete, o.result.Answer)
			}
		})
	}
}

func TestOutputRecordsNativeStopSignalsWithoutChangingRecovery(t *testing.T) {
	const quota = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1792200000}}`
	const overage = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","isUsingOverage":true}}`
	const stopped = `{"type":"system","subtype":"task_notification","status":"stopped","summary":"worker stopped"}`
	const root = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"fresh"}}`
	const failed = `{"type":"result","subtype":"error_during_execution","is_error":true,"permission_denials":[{}],"terminal_reason":"aborted_tools","api_error":"usage_limit_reached","api_error_status":429}`
	for _, tc := range []struct {
		name, events string
		codes        []string
		fails        bool
	}{
		{"rejected quota and failed result", quota + "\n" + failed, []string{"rate_limit_rejected", "permission_denied", "terminal_reason", "result_error"}, true},
		{"paid overage", overage + "\n" + successfulResult, []string{"result_detail"}, false},
		{"stopped task assessed by fresh root", stopped + "\n" + root + "\n" + successfulResult, []string{"task_stopped", "result_detail"}, false},
		{"terminal reason abort", strings.TrimSuffix(successfulResult, "}") + `,"terminal_reason":"aborted_tools"}`, []string{"terminal_reason", "result_detail"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.events+"\n")
			o.finish()
			if (o.err != nil) != tc.fails || len(o.result.Diagnostics) != len(tc.codes) {
				t.Fatalf("result=%+v error=%v", o.result, o.err)
			}
			for i, code := range tc.codes {
				if got := o.result.Diagnostics[i]; got.Code != code || got.Message == "" {
					t.Fatalf("diagnostic %d=%+v, want %q", i, got, code)
				}
			}
			if tc.name == "rejected quota and failed result" && (!strings.Contains(o.result.Diagnostics[0].Message, "resets at 2026-10-17T01:20:00Z") || !strings.Contains(o.result.Diagnostics[3].Message, "HTTP status=429")) {
				t.Fatalf("native quota fields lost: %+v", o.result.Diagnostics)
			}
		})
	}
}

func TestOutputKeepsAllStructuredFailuresAfterAnotherFailure(t *testing.T) {
	quota := errors.New("provider usage limit reached")
	pressure := errors.New("background work stopped under provider memory pressure")
	for _, first := range []error{nil, errors.New("invalid JSONL event"), aicli.ErrOutputLimit, fmt.Errorf("wrapped: %w", quota), fmt.Errorf("wrapped: %w", pressure)} {
		t.Run(fmt.Sprint(first), func(t *testing.T) {
			cancels := 0
			o := jsonlOutput{usageLimit: quota, taskFailure: pressure, cancel: func() { cancels++ }}
			if first != nil {
				o.fail(first)
			}
			o.finish()
			if !errors.Is(o.err, quota) || !errors.Is(o.err, pressure) || first != nil && !errors.Is(o.err, first) || cancels != 1 || o.result.Answer != "" || o.result.UsageComplete {
				t.Fatalf("structured cause or original failure lost: error=%v cancels=%d result=%+v", o.err, cancels, o.result)
			}
			if strings.Count(o.err.Error(), quota.Error()) != 1 || strings.Count(o.err.Error(), pressure.Error()) != 1 {
				t.Fatalf("structured cause duplicated: %v", o.err)
			}
		})
	}
}

func TestOutputTaskListsAreScopedAndRequired(t *testing.T) {
	for _, test := range []struct {
		name, event string
		invalid     bool
	}{
		{"unrelated tasks", `{"type":"user","tasks":["unrelated"]}`, false},
		{"unrelated errors", `{"type":"system","errors":{"unrelated":true}}`, false},
		{"missing", `{"type":"system","subtype":"background_tasks_changed"}`, true},
		{"null", `{"type":"system","subtype":"background_tasks_changed","tasks":null}`, true},
		{"object", `{"type":"system","subtype":"background_tasks_changed","tasks":{}}`, true},
		{"string entries", `{"type":"system","subtype":"background_tasks_changed","tasks":["worker"]}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := jsonlOutput{}
			_, _ = o.Write([]byte(successfulResult + "\n" + test.event + "\n"))
			o.finish()
			if (o.err != nil) != test.invalid || (o.result.Answer == "") != test.invalid {
				t.Fatalf("result=%+v error=%v", o.result, o.err)
			}
		})
	}
}

func TestOutputUsageLimitResetValidation(t *testing.T) {
	for _, test := range []struct{ name, reset, suffix string }{
		{"missing", "", ""}, {"zero", "0", ""}, {"negative", "-1", ""}, {"fractional", "1.5", ""},
		{"string", `"PRIVATE_RESET"`, ""}, {"null", "null", ""}, {"object", `{"private":"reset"}`, ""},
		{"integer overflow", "9223372036854775808", ""}, {"time overflow", "9223372036854775807", ""},
		{"outside RFC3339", "253402300800", ""}, {"first positive", "1", "; resets at 1970-01-01T00:00:01Z"},
		{"last RFC3339 second", "253402300799", "; resets at 9999-12-31T23:59:59Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reset := ""
			if test.reset != "" {
				reset = `,"resetsAt":` + test.reset
			}
			data := fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"PRIVATE_TYPE\nforged journal line"%s}}`, reset)
			o := jsonlOutput{}
			_, _ = o.Write([]byte(data))
			o.finish()
			if o.err == nil || o.err.Error() != "provider usage limit reached"+test.suffix || o.result.UsageComplete {
				t.Fatalf("reset was not safely bounded: %v", o.err)
			}
		})
	}
}

// numberResultEvents models the native CLI's contiguous per-turn delivery indices.
func numberResultEvents(data string) string {
	lines := strings.Split(data, "\n")
	index := 0
	for i, line := range lines {
		if strings.HasPrefix(line, `{"type":"result"`) {
			lines[i] = strings.TrimSuffix(line, "}") + `,"result_index":` + strconv.Itoa(index) + `}`
			index++
		}
	}
	return strings.Join(lines, "\n")
}

func TestOutputBackgroundTasks(t *testing.T) {
	const ambient = `{"type":"system","subtype":"background_tasks_changed","uuid":"ambient-list","tasks":[{"task_id":"housekeeping","ambient":true}]}`
	const worker = `{"type":"system","subtype":"background_tasks_changed","uuid":"worker-list","tasks":[{"task_id":"housekeeping","ambient":true},{"task_id":"worker"}]}`
	const pending = `{"type":"system","subtype":"background_tasks_changed","uuid":"reviewer-list","tasks":[{"task_id":"reviewer"}]}`
	const empty = `{"type":"system","subtype":"background_tasks_changed","uuid":"empty-list","tasks":[]}`
	const taggedFinish = `{"type":"system","subtype":"task_notification","task_id":"worker","run_id":"run-1","status":"completed"}`
	const runlessFailure = `{"type":"system","subtype":"task_notification","task_id":"worker","status":"failed"}`
	const syntheticFinish = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>worker</task-id><status>completed</status></task-notification>"}}`
	for _, tc := range []struct {
		name, before, after string
		fails               bool
	}{
		{name: "ambient after final", after: ambient},
		{name: "mixed pending", after: worker, fails: true},
		{name: "pending removed needs new result", after: worker + "\n" + ambient, fails: true},
		{name: "new result after worker completes", before: successfulResult + "\n" + worker + "\n" + ambient},
		{name: "unfinished before result", before: pending, fails: true},
		{name: "unfinished after result", after: pending, fails: true},
		{name: "completion before result", before: pending + "\n" + empty},
		{name: "completion after result needs new answer", before: pending, after: empty, fails: true},
		{name: "answer after completion", before: pending + "\n" + successfulResult + "\n" + empty},
		{name: "work after result needs new answer", after: pending + "\n" + empty, fails: true},
		{name: "answer after new work", before: successfulResult + "\n" + pending + "\n" + empty},
		{name: "failed command recovered", before: `{"type":"system","subtype":"task_notification","task_id":"test","status":"failed"}`},
		{name: "late tagged completion needs new answer", after: taggedFinish, fails: true},
		{name: "late runless failure needs new answer", after: runlessFailure, fails: true},
		{name: "late synthetic completion needs new answer", after: syntheticFinish, fails: true},
		{name: "answer after late tagged completion", after: taggedFinish + "\n" + successfulResult},
		{name: "answer after late runless failure", after: runlessFailure + "\n" + successfulResult},
		{name: "answer after late synthetic completion", after: syntheticFinish + "\n" + successfulResult},
		{name: "tagged finish replay after answer", before: taggedFinish, after: taggedFinish},
		{name: "runless failure without replay ID needs new answer", before: runlessFailure, after: runlessFailure, fails: true},
		{name: "synthetic finish without replay ID needs new answer", before: syntheticFinish, after: syntheticFinish, fails: true},
		{name: "synthetic notice cannot prove tagged finish replay", before: taggedFinish, after: syntheticFinish, fails: true},
		{name: "user text is not task state", after: `{"type":"user","subtype":"background_tasks_changed","tasks":[{"task_id":"reviewer"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cancelled := false
			o := jsonlOutput{cancel: func() { cancelled = true }}
			if _, err := o.Write([]byte(numberResultEvents(tc.before + "\n" + successfulResult + "\n" + tc.after + "\n"))); err != nil {
				t.Fatal(err)
			}
			o.finish()
			if (o.err != nil || !o.completed) != tc.fails || cancelled != (o.err != nil) || o.result.UsageComplete == tc.fails || o.result.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}) {
				t.Fatalf("output = %+v, error %v, cancelled %v", o.result, o.err, cancelled)
			}
		})
	}
}

func TestOutputTaskNoticeRequiresUniqueReplayIdentity(t *testing.T) {
	const bare = `{"type":"system","subtype":"task_notification","task_id":"helper","status":"completed"}`
	const uuidA = `{"type":"system","subtype":"task_notification","uuid":"notice-a","task_id":"helper","status":"completed"}`
	const uuidB = `{"type":"system","subtype":"task_notification","uuid":"notice-b","task_id":"helper","status":"completed"}`
	const tagged = `{"type":"system","subtype":"task_notification","task_id":"helper","run_id":"run-1","status":"completed"}`
	const taggedUUID = `{"type":"system","subtype":"task_notification","uuid":"tagged-notice","task_id":"helper","run_id":"run-1","status":"completed"}`
	const synthetic = `{"type":"user","isSynthetic":true,"message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	const syntheticTaggedUUID = `{"type":"user","isSynthetic":true,"uuid":"tagged-notice","message":{"content":"<task-notification><task-id>helper</task-id><status>completed</status></task-notification>"}}`
	for _, tc := range []struct {
		name, before, after string
		valid               bool
	}{
		{"same uuid replay", uuidA, uuidA, true},
		{"new uuid same task", uuidA, uuidB, false},
		{"missing uuid repeat", bare, bare, false},
		{"synthetic without uuid repeat", synthetic, synthetic, false},
		{"tagged run proves replay and records new uuid", tagged, taggedUUID + "\n" + syntheticTaggedUUID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			writeChunks(t, &o, tc.before+"\n"+successfulResult+"\n"+tc.after+"\n")
			o.finish()
			if o.completed != tc.valid || (o.err == nil) != tc.valid {
				t.Fatalf("notice replay: completed=%t error=%v, valid=%t", o.completed, o.err, tc.valid)
			}
		})
	}
}

func TestOutputNativeTurnResults(t *testing.T) {
	const pending = `{"type":"system","subtype":"background_tasks_changed","uuid":"pending-list","tasks":[{"task_id":"worker"}]}`
	const clear = `{"type":"system","subtype":"background_tasks_changed","uuid":"cleared-list","tasks":[]}`
	const structured = `{"answer":"published","refutations":"private"}`
	waiting := strings.Replace(successfulResult, "final answer", "waiting", 1)
	empty := strings.Replace(successfulResult, "final answer", "", 1)
	formatted := strings.TrimSuffix(empty, "}") + `,"structured_output":` + structured + `}`
	denied := strings.TrimSuffix(empty, "}") + `,"permission_denials":[{"tool_name":"Bash"}]}`
	failed := strings.Replace(successfulResult, `"subtype":"success"`, `"subtype":"error_during_execution"`, 1)
	for _, tc := range []struct {
		name, data, answer, failure string
		structured, unterminated    bool
	}{
		{name: "deferred turns after task completion", data: numberResultEvents(pending + "\n" + clear + "\n" + waiting + "\n" + waiting + "\n" + empty + "\n" + successfulResult), answer: "final answer"},
		{name: "latest empty never falls back", data: numberResultEvents(waiting + "\n" + empty), failure: "missing successful Claude result"},
		{name: "only final structured schema required", data: numberResultEvents(waiting + "\n" + empty + "\n" + formatted), structured: true, answer: structured},
		{name: "latest missing schema never falls back", data: numberResultEvents(formatted + "\n" + waiting), structured: true, failure: "missing Claude structured output"},
		{name: "first error cannot be replaced", data: numberResultEvents(failed + "\n" + successfulResult), failure: "result failed:"},
		{name: "later error cannot be replaced", data: numberResultEvents(waiting + "\n" + failed + "\n" + successfulResult), failure: "result failed:"},
		{name: "first denial cannot be replaced", data: numberResultEvents(denied + "\n" + successfulResult), failure: "denied tool permissions"},
		{name: "empty turn denial cannot be replaced", data: numberResultEvents(waiting + "\n" + denied + "\n" + successfulResult), failure: "denied tool permissions"},
		{name: "duplicate indices", data: numberResultEvents(successfulResult) + "\n" + numberResultEvents(successfulResult), failure: "result_index 0, want 1"},
		{name: "gap in indices", data: numberResultEvents(successfulResult) + "\n" + strings.TrimSuffix(successfulResult, "}") + `,"result_index":2}`, failure: "result_index 2, want 1"},
		{name: "nonzero initial index", data: strings.TrimSuffix(successfulResult, "}") + `,"result_index":1}`, failure: "result_index 1, want 0"},
		{name: "negative index", data: strings.TrimSuffix(successfulResult, "}") + `,"result_index":-1}`, failure: "result_index -1, want 0"},
		{name: "single unindexed result", data: successfulResult, answer: "final answer", unterminated: true},
		{name: "multiple unindexed results", data: successfulResult + "\n" + successfulResult, failure: "require native result_index"},
		{name: "indexed then unindexed", data: numberResultEvents(successfulResult) + "\n" + successfulResult, failure: "require native result_index"},
		{name: "unindexed then indexed", data: successfulResult + "\n" + strings.TrimSuffix(successfulResult, "}") + `,"result_index":1}`, failure: "require native result_index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cancelled := false
			o := jsonlOutput{structured: tc.structured, cancel: func() { cancelled = true }}
			data := tc.data
			if !tc.unterminated {
				data += "\n"
			}
			writeChunks(t, &o, data)
			o.finish()
			if tc.failure != "" {
				if o.err == nil || !strings.Contains(o.err.Error(), tc.failure) || !cancelled || o.result.UsageComplete {
					t.Fatalf("invalid result stream accepted: error=%v cancelled=%v complete=%v", o.err, cancelled, o.result.UsageComplete)
				}
			} else if o.err != nil || cancelled || !o.completed || !o.result.UsageComplete || o.result.Answer != tc.answer {
				t.Fatalf("valid result stream rejected: result=%+v error=%v cancelled=%v", o.result, o.err, cancelled)
			}
		})
	}
}

func TestUsageValidation(t *testing.T) {
	zero, one, negative, maximum := int64(0), int64(1), int64(-1), int64(math.MaxInt64)
	for _, tc := range []struct {
		name  string
		usage eventUsage
		valid bool
	}{
		{"missing", eventUsage{}, false},
		{"zero", eventUsage{Input: &zero, Created: &zero, Cached: &zero, Output: &zero}, true},
		{"negative", eventUsage{Input: &negative, Created: &zero, Cached: &zero, Output: &zero}, false},
		{"input overflow", eventUsage{Input: &maximum, Created: &one, Cached: &zero, Output: &zero}, false},
		{"cached input overflow", eventUsage{Input: &zero, Created: &maximum, Cached: &one, Output: &zero}, false},
		{"total overflow", eventUsage{Input: &maximum, Created: &zero, Cached: &zero, Output: &one}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, valid := tc.usage.tokens(); valid != tc.valid {
				t.Fatalf("valid = %v, want %v", valid, tc.valid)
			}
		})
	}
}

func TestOutputWholeTreeUsage(t *testing.T) {
	const root = `{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}`
	const models = `{"claude-main":{"inputTokens":13,"cacheCreationInputTokens":17,"cacheReadInputTokens":19,"outputTokens":23},"claude-helper":{"inputTokens":29,"cacheCreationInputTokens":31,"cacheReadInputTokens":37,"outputTokens":41}}`
	fallback := usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}
	for _, tc := range []struct {
		name, root, models string
		want               usage.Tokens
		complete           bool
	}{
		{"root and helper models", root, models, usage.Tokens{Input: 146, CacheCreationInput: 48, CachedInput: 56, Output: 64}, true},
		{"aggregate ignores malformed root", `"bad"`, models, usage.Tokens{Input: 146, CacheCreationInput: 48, CachedInput: 56, Output: 64}, true},
		{"zero aggregate with current root usage", root, `{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}`, fallback, false},
		{"zero aggregate with zero root usage", `{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}`, `{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}`, usage.Tokens{}, true},
		{"partial aggregate keeps root counters", root, `{"main":{"inputTokens":20,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}`, usage.Tokens{Input: 20, CacheCreationInput: 5, CachedInput: 7, Output: 11}, false},
		{"absent aggregate", root, "", fallback, false},
		{"empty aggregate", root, "{}", fallback, false},
		{"malformed aggregate", root, `"bad"`, fallback, false},
		{"partially decoded aggregate", root, `{"main":{"inputTokens":"bad","cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}`, fallback, false},
		{"missing counter", root, `{"main":{"inputTokens":1,"outputTokens":2}}`, fallback, false},
		{"aggregate total overflow", root, `{"main":{"inputTokens":9223372036854775806,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0},"helper":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}`, fallback, false},
		{"malformed fallback", `"bad"`, "null", usage.Tokens{}, false},
		{"invalid fallback", `{"input_tokens":9223372036854775807,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}`, "null", usage.Tokens{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := `{"type":"result","subtype":"success","result":"answer"`
			if tc.root != "" {
				data += `,"usage":` + tc.root
			}
			if tc.models != "" {
				data += `,"modelUsage":` + tc.models
			}
			o := jsonlOutput{}
			if _, err := o.Write([]byte(data + "}\n")); err != nil {
				t.Fatal(err)
			}
			o.finish()
			if o.err != nil || o.result.Answer != "answer" || o.result.Tokens != tc.want || o.result.UsageComplete != tc.complete {
				t.Fatalf("output = %+v, %v; want tokens %+v, complete %v", o.result, o.err, tc.want, tc.complete)
			}
			if tc.name == "aggregate ignores malformed root" && o.result.UsageCoverage != (usage.Coverage{Scope: "invocation"}) {
				t.Fatalf("valid model usage inherited malformed root gap: %+v", o.result.UsageCoverage)
			}
		})
	}
}

func TestOutputUsageCoverage(t *testing.T) {
	const root = `{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}`
	const models = `{"main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":11}}`
	for _, tc := range []struct {
		name, fields    string
		baselineUnknown bool
		want            usage.Coverage
	}{
		{"model report", `,"usage":` + root + `,"modelUsage":` + models, false, usage.Coverage{Scope: "invocation"}},
		{"root report only", `,"usage":` + root, false, usage.Coverage{Scope: "root_only", PartialReport: true}},
		{"malformed model fallback", `,"usage":` + root + `,"modelUsage":{"main":{"inputTokens":"bad"}}`, false, usage.Coverage{Scope: "root_only", PartialReport: true, MalformedFinalReport: true}},
		{"missing report", "", false, usage.Coverage{Scope: "unknown", MissingFinalReport: true}},
		{"malformed root report", `,"usage":{"input_tokens":"bad"}`, false, usage.Coverage{Scope: "unknown", MalformedFinalReport: true}},
		{"unattributed cumulative model report", `,"usage":` + root + `,"modelUsage":` + models, true, usage.Coverage{Scope: "root_only", PartialReport: true, CumulativeUnattributed: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{baselineUnknown: tc.baselineUnknown}
			line := `{"type":"result","subtype":"success","result":"answer"` + tc.fields + "}\n"
			if _, err := o.Write([]byte(line)); err != nil {
				t.Fatal(err)
			}
			o.finish()
			if o.err != nil || o.result.UsageCoverage != tc.want {
				t.Fatalf("coverage=%+v result=%+v err=%v, want %+v", o.result.UsageCoverage, o.result, o.err, tc.want)
			}
		})
	}
}

func TestOutputPreservesRecordedUsage(t *testing.T) {
	const pending = `{"type":"system","subtype":"background_tasks_changed","uuid":"pending-list","tasks":[{"task_id":"reviewer"}]}`
	const clear = `{"type":"system","subtype":"background_tasks_changed","uuid":"cleared-list","tasks":[]}`
	const models = `"modelUsage":{"main":{"inputTokens":13,"cacheCreationInputTokens":17,"cacheReadInputTokens":19,"outputTokens":23},"helper":{"inputTokens":29,"cacheCreationInputTokens":31,"cacheReadInputTokens":37,"outputTokens":41}}`
	const aggregate = `{"type":"result","subtype":"success","result":"answer",` + models + `}`
	const fallback = `"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}`
	const partial = `{"type":"result","subtype":"success","result":"provisional",` + fallback + `}`
	known := usage.Tokens{Input: 146, CacheCreationInput: 48, CachedInput: 56, Output: 64}
	for _, tc := range []struct {
		name, first, intermediate, final string
		want                             usage.Tokens
		failed, complete                 bool
	}{
		{name: "error without usage", final: `{"type":"result","subtype":"error_during_execution","is_error":true}`, want: known, failed: true},
		{name: "error with smaller root fallback", final: `{"type":"result","subtype":"error_during_execution","is_error":true,` + fallback + `}`, want: known, failed: true},
		{name: "success without usage", final: `{"type":"result","subtype":"success","result":"final"}`, want: known},
		{name: "success with invalid aggregate and root fallback", final: `{"type":"result","subtype":"success","result":"final","modelUsage":{"main":{"inputTokens":"invalid"}},` + fallback + `}`, want: known},
		{name: "incomparable aggregate keeps each known counter", final: `{"type":"result","subtype":"success","result":"final","modelUsage":{"main":{"inputTokens":100,"cacheCreationInputTokens":0,"cacheReadInputTokens":40,"outputTokens":100}}}`, want: usage.Tokens{Input: 146, CacheCreationInput: 48, CachedInput: 56, Output: 100}},
		{name: "identical cumulative aggregate not summed", intermediate: aggregate, final: aggregate, want: known, complete: true},
		{name: "dominating aggregate restores completeness", intermediate: partial, final: `{"type":"result","subtype":"success","result":"final","modelUsage":{"main":{"inputTokens":110,"cacheCreationInputTokens":50,"cacheReadInputTokens":60,"outputTokens":100}}}`, want: usage.Tokens{Input: 220, CacheCreationInput: 50, CachedInput: 60, Output: 100}, complete: true},
		{name: "component merge refuses total overflow", first: `{"type":"result","subtype":"success","result":"provisional","modelUsage":{"main":{"inputTokens":9223372036854775797,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":9}}}`, final: `{"type":"result","subtype":"success","result":"final","modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":9223372036854775805}}}`, want: usage.Tokens{Input: 9223372036854775797, Output: 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := jsonlOutput{}
			first := tc.first
			if first == "" {
				first = aggregate
			}
			data := numberResultEvents(pending + "\n" + first + "\n" + tc.intermediate + "\n" + clear + "\n" + tc.final + "\n")
			writeChunks(t, &o, data)
			o.finish()
			if o.result.Tokens != tc.want || o.result.UsageComplete != tc.complete || (o.err != nil) != tc.failed {
				t.Fatalf("known counters not preserved: got tokens=%+v complete=%v error=%v; want tokens=%+v complete=%v failed=%v", o.result.Tokens, o.result.UsageComplete, o.err, tc.want, tc.complete, tc.failed)
			}
			if tc.failed && !strings.Contains(o.err.Error(), "result failed:") {
				t.Fatalf("unexpected failure: %v", o.err)
			}
		})
	}
}
