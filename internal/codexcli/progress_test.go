package codexcli

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
)

func TestJSONLOutputProgressRejectsOversizedCompletedItemID(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	_, _ = output.Write([]byte(`{"type":"thread.started","thread_id":"root"}` + "\n"))
	oversized := strings.Repeat("x", maxItemIDBytes+1)
	_, _ = output.Write([]byte(fmt.Sprintf(`{"type":"item.completed","item":{"id":%q,"type":"command_execution"}}`, oversized) + "\n"))
	if progress != 0 || len(output.progressItems) != 0 {
		t.Fatalf("oversized completed item entered progress: count=%d items=%d", progress, len(output.progressItems))
	}
	_, _ = output.Write([]byte(`{"type":"item.completed","item":{"id":"valid","type":"command_execution"}}` + "\n"))
	if progress != 1 || len(output.progressItems) != 1 {
		t.Fatalf("valid completed item lost progress: count=%d items=%d", progress, len(output.progressItems))
	}
}

func TestJSONLOutputCompletedHiddenReasoningRenewsProgressOnce(t *testing.T) {
	progress := 0
	var events []aicli.StreamEvent
	output := jsonlOutput{
		onEvent:    func(event aicli.StreamEvent) { events = append(events, event) },
		onProgress: func() { progress++ },
	}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"thread.started","thread_id":"root"}`, 0},
		{`{"type":"item.started","item":{"id":"reason","type":"reasoning","content":"hidden one"}}`, 0},
		{`{"type":"item.updated","item":{"id":"reason","type":"reasoning","content":"hidden one"}}`, 0},
		{`{"type":"item.completed","item":{"id":"reason","type":"reasoning","content":"hidden one"}}`, 1},
		{`{"type":"item.completed","item":{"id":"reason","type":"reasoning","content":"hidden replay"}}`, 1},
		{`{"type":"item.completed","item":{"id":"next","type":"reasoning","content":"hidden one"}}`, 2},
		{`{"type":"item.completed","item":{"type":"reasoning","content":"legacy one"}}`, 3},
		{`{"type":"item.completed","item":{"type":"reasoning","content":"legacy one","elapsed_time_ms":100}}`, 3},
		{`{"type":"item.completed","item":{"type":"reasoning","content":"legacy two"}}`, 3},
		{`{"type":"turn.completed"}`, 3},
		{`{"type":"item.completed","item":{"id":"late","type":"reasoning","content":"hidden late"}}`, 3},
	} {
		_, _ = output.Write([]byte(step.line + "\n"))
		if progress != step.want {
			t.Fatalf("step %d: progress=%d, want %d", i, progress, step.want)
		}
	}
	if len(output.progressItems) != 3 {
		t.Fatalf("hidden reasoning changed progress identities: %d", len(output.progressItems))
	}
	for key := range output.progressItems {
		if strings.Contains(key, "hidden") || strings.Contains(key, "legacy one") || strings.Contains(key, "legacy two") {
			t.Fatal("hidden reasoning entered a progress identity")
		}
	}
	for _, event := range events {
		if strings.Contains(event.Text, "hidden") || strings.Contains(event.Text, "legacy") {
			t.Fatal("hidden reasoning entered provider discussion")
		}
	}
}

func TestJSONLOutputProgressRequiresNewRootWork(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	for _, step := range []struct {
		line string
		want int
	}{
		{`{"type":"thread.started","thread_id":"root"}`, 0},
		{`{"type":"turn.started"}`, 0},
		{`{"type":"token_count","usage":{"input_tokens":1}}`, 0},
		{`{"type":"item.started","item":{"id":"message","type":"agent_message","text":""}}`, 0},
		{`{"type":"item.updated","item":{"id":"message","type":"agent_message","text":"partial"}}`, 1},
		{`{"type":"item.updated","item":{"id":"message","type":"agent_message","text":"partial"}}`, 1},
		{`{"type":"item.updated","metadata":"poll","item":{"id":"message","type":"agent_message","text":"partial"}}`, 1},
		{`{"type":"item.updated","item":{"id":"message","type":"agent_message","text":"more output"}}`, 2},
		{`{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"more output"}}`, 3},
		{`{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"replayed"}}`, 3},
		{`{"type":"item.started","item":{"id":"tool","type":"command_execution","command":"go test"}}`, 4},
		{`{"type":"item.updated","item":{"id":"tool","type":"command_execution","command":"go test"}}`, 4},
		{`{"type":"item.updated","item":{"id":"tool","type":"command_execution","command":"go test","aggregated_output":"PASS"}}`, 5},
		{`{"type":"item.completed","item":{"id":"tool","type":"command_execution","command":"go test","aggregated_output":"PASS"}}`, 6},
		{`{"type":"item.completed","item":{"id":"tool","type":"command_execution","command":"go test","aggregated_output":"PASS"}}`, 6},
		{`{"type":"item.started","item":{"id":"poll","type":"collab_tool_call","tool":"wait"}}`, 6},
		{`{"type":"item.completed","item":{"id":"poll","type":"collab_tool_call","tool":"wait"}}`, 6},
		{`{"type":"item.started","thread_id":"foreign","item":{"id":"foreign","type":"command_execution"}}`, 6},
		{`{"type":"item.completed","item":{"id":"todo","type":"todo_list","items":[]}}`, 6},
		{`{"type":"error","message":"retrying"}`, 6},
		{`{"type":"item.completed","item":{"type":"agent_message","text":"legacy output"}}`, 7},
		{`{"type":"item.completed","item":{"type":"agent_message","text":"legacy output"}}`, 7},
		{`{"type":"item.started","item":{"type":"agent_message","text":"legacy partial"}}`, 8},
		{`{"type":"item.updated","item":{"type":"agent_message","text":"legacy partial"}}`, 8},
		{`{"type":"item.updated","item":{"type":"agent_message","text":"legacy more"}}`, 9},
		{`{"type":"item.started","item":{"type":"command_execution","command":"legacy tool"}}`, 10},
		{`{"type":"item.updated","item":{"type":"command_execution","command":"legacy tool","aggregated_output":"PASS"}}`, 11},
		{`{"type":"item.completed","item":{"type":"command_execution","command":"legacy tool","aggregated_output":"PASS"}}`, 12},
		{`{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":1}}`, 12},
		{`{"type":"item.completed","item":{"type":"command_execution","command":"late tool"}}`, 12},
	} {
		_, _ = output.Write([]byte(step.line + "\n"))
		if progress != step.want {
			t.Fatalf("after %s: progress=%d, want %d", step.line, progress, step.want)
		}
	}
}

func TestJSONLOutputMetadataOnlyUpdatesDoNotRenewProgress(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"thread.started","thread_id":"root"}`, 0},
		{`{"type":"item.started","item":{"id":"tool","type":"command_execution","command":"go test"}}`, 1},
		{`{"type":"item.updated","item":{"id":"tool","type":"command_execution","command":"go test","elapsed_time_ms":100}}`, 1},
		{`{"type":"item.updated","item":{"id":"tool","type":"command_execution","command":"go test","elapsed_time_ms":200}}`, 1},
		{`{"type":"item.updated","item":{"id":"tool","type":"command_execution","command":"go test","aggregated_output":"PASS"}}`, 2},
		{`{"type":"item.updated","item":{"id":"tool","type":"command_execution","command":"go test","aggregated_output":"PASS","elapsed_time_ms":300}}`, 2},
		{`{"type":"item.completed","item":{"id":"tool","type":"command_execution","command":"go test","aggregated_output":"PASS"}}`, 3},
		{`{"type":"item.started","item":{"type":"command_execution","command":"legacy tool"}}`, 4},
		{`{"type":"item.updated","item":{"type":"command_execution","command":"legacy tool","elapsed_time_ms":100}}`, 4},
		{`{"type":"item.updated","item":{"type":"command_execution","command":"legacy tool","aggregated_output":"done"}}`, 5},
	} {
		_, _ = output.Write([]byte(step.line + "\n"))
		if progress != step.want {
			t.Fatalf("step %d progress=%d, want %d", i, progress, step.want)
		}
	}
}

func TestJSONLOutputNonAdjacentSnapshotReplayDoesNotRenewProgress(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"thread.started","thread_id":"root"}`, 0},
		{`{"type":"item.started","item":{"id":"command","type":"command_execution","aggregated_output":"A"}}`, 1},
		{`{"type":"item.updated","item":{"id":"command","type":"command_execution","aggregated_output":"AB"}}`, 2},
		{`{"type":"item.updated","item":{"id":"command","type":"command_execution","aggregated_output":"A"}}`, 2},
		{`{"type":"item.updated","item":{"id":"command","type":"command_execution","aggregated_output":"AB"}}`, 2},
		{`{"type":"item.updated","item":{"id":"command","type":"command_execution","aggregated_output":"ABC"}}`, 3},
		{`{"type":"item.completed","item":{"id":"command","type":"command_execution","aggregated_output":"ABC"}}`, 4},
	} {
		_, _ = output.Write([]byte(step.line + "\n"))
		if progress != step.want {
			t.Fatalf("step %d progress=%d, want %d", i, progress, step.want)
		}
	}
}

func TestJSONLOutputOldSnapshotReplayDoesNotRenewProgress(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	_, _ = output.Write([]byte(`{"type":"thread.started","thread_id":"root"}` + "\n"))
	_, _ = output.Write([]byte(`{"type":"item.started","item":{"id":"command","type":"command_execution","aggregated_output":"A"}}` + "\n"))
	for i := 0; i < 10; i++ {
		line := fmt.Sprintf(`{"type":"item.updated","item":{"id":"command","type":"command_execution","aggregated_output":"A%d"}}`, i)
		_, _ = output.Write([]byte(line + "\n"))
	}
	if progress != 11 {
		t.Fatalf("fresh updates progress=%d, want 11", progress)
	}
	_, _ = output.Write([]byte(`{"type":"item.updated","item":{"id":"command","type":"command_execution","aggregated_output":"A"}}` + "\n"))
	_, _ = output.Write([]byte(`{"type":"item.updated","item":{"id":"command","type":"command_execution","aggregated_output":"A0"}}` + "\n"))
	if progress != 11 {
		t.Fatalf("old snapshots renewed progress: %d", progress)
	}
}

func TestJSONLOutputIDLessRepeatedToolCycles(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	for i, step := range []struct {
		line string
		want int
	}{
		{`{"type":"thread.started","thread_id":"root"}`, 0},
		{`{"type":"item.started","item":{"type":"command_execution","command":"same command"}}`, 1},
		{`{"type":"item.updated","item":{"type":"command_execution","command":"same command","aggregated_output":"PASS"}}`, 2},
		{`{"type":"item.completed","item":{"type":"command_execution","command":"same command","aggregated_output":"PASS"}}`, 3},
		{`{"type":"item.completed","item":{"type":"command_execution","command":"same command","aggregated_output":"PASS"}}`, 3},
		{`{"type":"item.started","item":{"type":"command_execution","command":"same command"}}`, 4},
		{`{"type":"item.updated","item":{"type":"command_execution","command":"same command","aggregated_output":"PASS"}}`, 5},
		{`{"type":"item.completed","item":{"type":"command_execution","command":"same command","aggregated_output":"PASS"}}`, 6},
		{`{"type":"item.completed","item":{"type":"command_execution","command":"same command","aggregated_output":"PASS"}}`, 6},
	} {
		_, _ = output.Write([]byte(step.line + "\n"))
		if progress != step.want {
			t.Fatalf("step %d progress=%d, want %d", i, progress, step.want)
		}
	}
}

func TestJSONLOutputIDLessCompletedOnlyCommandReplayDoesNotStopRun(t *testing.T) {
	const completed = `{"type":"item.completed","item":{"type":"command_execution","command":"same command","aggregated_output":"PASS"}}`
	for _, replay := range []struct {
		name string
		line string
	}{
		{"identical", completed},
		{"metadata only", `{"type":"item.completed","item":{"type":"command_execution","command":"same command","aggregated_output":"PASS","elapsed_time_ms":300}}`},
	} {
		t.Run(replay.name, func(t *testing.T) {
			progress, cancellations := 0, 0
			var events []aicli.StreamEvent
			output := jsonlOutput{
				cancel:     func() { cancellations++ },
				onEvent:    func(event aicli.StreamEvent) { events = append(events, event) },
				onProgress: func() { progress++ },
			}
			for _, line := range []string{
				`{"type":"thread.started","thread_id":"root"}`,
				completed,
				replay.line,
				`{"type":"item.started","item":{"id":"next","type":"command_execution","command":"later command"}}`,
			} {
				_, _ = output.Write([]byte(line + "\n"))
			}
			if progress != 2 || cancellations != 0 || output.failed || output.stopErr != nil ||
				len(events) != 3 || events[1].Kind != "tool" || events[2].Kind != "tool" {
				t.Fatalf("ambiguous completion stopped the run or renewed progress: progress=%d cancellations=%d stop=%v events=%+v", progress, cancellations, output.stopErr, events)
			}
		})
	}
}

func TestJSONLOutputProgressIdentityCapStopsRun(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }, progressItemLimit: 1}
	for _, line := range []string{
		`{"type":"thread.started","thread_id":"root"}`,
		`{"type":"item.started","item":{"id":"first","type":"command_execution"}}`,
		`{"type":"item.started","item":{"id":"second","type":"command_execution"}}`,
	} {
		_, _ = output.Write([]byte(line + "\n"))
	}
	if progress != 1 || !errors.Is(output.stopErr, aicli.ErrOutputLimit) {
		t.Fatalf("identity cap: progress=%d stop=%v", progress, output.stopErr)
	}
}

func TestRolloutActivityProgressRequiresNewHelperWork(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	root := writeUsageRollout(t, home, "root", "", usageEventAt(9, "task_started", "root-turn"))
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		usageEventAt(9, "task_started", "child-turn"))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("helper start progress=%d, want 1", *progress)
	}
	appendUsageEvents(t, child,
		usageSnapshot(10, 20, 0, 1),
		`{"ordinal":11,"type":"event_msg","payload":{"type":"thread_goal_updated","turn_id":"child-turn"}}`,
		`{"ordinal":12,"type":"event_msg","payload":{"type":"sub_agent_activity","kind":"interacted","agent_thread_id":"grandchild","turn_id":"child-turn"}}`,
		`{"ordinal":13,"type":"response_item","payload":{"type":"additional_tools","id":"tools"}}`,
		rolloutCompletedItemTurn(14, "child", "child-turn", `{"type":"CollabAgentToolCall","tool":"wait","status":"completed"}`))
	activity.poll()
	if *progress != 1 {
		t.Fatalf("helper metadata renewed progress: %d", *progress)
	}
	message := `{"type":"message","id":"reply","role":"assistant","content":[{"type":"output_text","text":"working"}],"internal_chat_message_metadata_passthrough":{"turn_id":"child-turn"}}`
	appendUsageEvents(t, child, fmt.Sprintf(`{"ordinal":15,"type":"response_item","payload":%s}`, message))
	activity.poll()
	if *progress != 2 {
		t.Fatalf("helper output progress=%d, want 2", *progress)
	}
	appendUsageEvents(t, child, fmt.Sprintf(`{"ordinal":16,"type":"response_item","payload":%s}`, message))
	activity.poll()
	if *progress != 2 {
		t.Fatalf("replayed helper output renewed progress: %d", *progress)
	}
	tool := `{"type":"item_completed","thread_id":"child","turn_id":"child-turn","item":{"type":"CommandExecution","id":"tool","aggregated_output":"PASS"}}`
	appendUsageEvents(t, child, fmt.Sprintf(`{"ordinal":17,"type":"event_msg","payload":%s}`, tool))
	activity.poll()
	if *progress != 3 {
		t.Fatalf("helper tool completion progress=%d, want 3", *progress)
	}
	appendUsageEvents(t, child, fmt.Sprintf(`{"ordinal":18,"type":"event_msg","payload":%s}`, tool))
	appendUsageEvents(t, root, usageEventAt(19, "task_complete", "root-turn"), usageSnapshot(20, 50, 0, 2))
	activity.poll()
	activity.poll()
	if *progress != 3 {
		t.Fatalf("replay, root lifecycle, or polling renewed progress: %d", *progress)
	}
	appendUsageEvents(t, child,
		`{"ordinal":21,"type":"event_msg","payload":{"type":"mcp_tool_call_end","turn_id":"child-turn","call_id":"call"}}`)
	activity.poll()
	if *progress != 4 {
		t.Fatalf("helper tool event progress=%d, want 4", *progress)
	}
}

func TestRolloutActivityCompletedHelperItemMetadataDoesNotRenewProgress(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	boundary := int64(9)
	completion := func(ordinal int, id, output string, elapsed int) string {
		item := fmt.Sprintf(`{"type":"CommandExecution","id":%q,"command":["go","test"],"status":"completed","aggregated_output":%q,"elapsed_time_ms":%d}`, id, output, elapsed)
		return rolloutCompletedItemTurn(ordinal, "child", "child-turn", item)
	}
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		completion(9, "tool", "PASS", 100))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("first helper completion progress=%d, want 1", *progress)
	}
	for _, step := range []struct {
		line string
		want int
	}{
		{completion(10, "tool", "PASS", 200), 1},
		{completion(11, "tool", "PASS\nmore output", 300), 2},
		{completion(12, "next-tool", "PASS\nmore output", 400), 3},
	} {
		appendUsageEvents(t, child, step.line)
		activity.poll()
		if *progress != step.want {
			t.Fatalf("after %s: progress=%d, want %d", step.line, *progress, step.want)
		}
	}
}

func TestRolloutActivityResponseMetadataDoesNotRenewProgress(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	boundary := int64(9)
	response := func(ordinal int, payload string) string {
		return fmt.Sprintf(`{"ordinal":%d,"type":"response_item","payload":%s}`, ordinal, payload)
	}
	message := func(id, text string, elapsed int) string {
		return fmt.Sprintf(`{"type":"message","id":%q,"role":"assistant","content":[{"type":"output_text","text":%q,"annotations":[{"page":%d}],"logprobs":[{"score":%d}]}],"internal_chat_message_metadata_passthrough":{"turn_id":"current"},"elapsed_time_ms":%d}`, id, text, elapsed, elapsed, elapsed)
	}
	reasoning := func(summary, hidden string, elapsed int) string {
		return fmt.Sprintf(`{"type":"reasoning","id":"thought","summary":[{"type":"summary_text","text":%q}],"content":[{"type":"reasoning_text","text":%q}],"internal_chat_message_metadata_passthrough":{"turn_id":"current"},"elapsed_time_ms":%d}`, summary, hidden, elapsed)
	}
	toolOutput := func(callID, output string, elapsed int) string {
		return fmt.Sprintf(`{"type":"function_call_output","call_id":%q,"output":%q,"internal_chat_message_metadata_passthrough":{"turn_id":"current"},"elapsed_time_ms":%d}`, callID, output, elapsed)
	}
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		response(9, message("reply", "working", 100)))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("first response progress=%d, want 1", *progress)
	}
	for i, step := range []struct {
		line string
		want int
	}{
		{response(10, message("reply", "working", 200)), 1},
		{response(11, message("reply", "more work", 300)), 2},
		{response(12, message("next", "more work", 400)), 3},
		{response(13, reasoning("summary", "private one", 100)), 4},
		{response(14, reasoning("summary", "private two", 200)), 4},
		{response(15, reasoning("new summary", "private two", 300)), 5},
		{response(16, toolOutput("call-1", "PASS", 100)), 6},
		{response(17, toolOutput("call-1", "PASS", 200)), 6},
		{response(18, toolOutput("call-1", "MORE", 300)), 7},
		{response(19, toolOutput("call-2", "MORE", 400)), 8},
	} {
		appendUsageEvents(t, child, step.line)
		activity.poll()
		if *progress != step.want {
			t.Fatalf("step %d: progress=%d, want %d", i, *progress, step.want)
		}
	}
}

func TestRolloutActivityResponseVisibleContentRenewsProgress(t *testing.T) {
	for _, test := range []struct {
		name, first, changed string
	}{
		{"image URL", `{"type":"input_image","image_url":"data:image/png;base64,one","detail":"high"}`, `{"type":"input_image","image_url":"data:image/png;base64,two","detail":"high"}`},
		{"image file ID", `{"type":"input_image","file_id":"image-one"}`, `{"type":"input_image","file_id":"image-two"}`},
		{"image detail", `{"type":"input_image","file_id":"image-one","detail":"low"}`, `{"type":"input_image","file_id":"image-one","detail":"high"}`},
		{"audio URL", `{"type":"input_audio","audio_url":"data:audio/wav;base64,one"}`, `{"type":"input_audio","audio_url":"data:audio/wav;base64,two"}`},
		{"audio file ID", `{"type":"input_audio","file_id":"audio-one"}`, `{"type":"input_audio","file_id":"audio-two"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
			boundary := int64(9)
			response := func(ordinal, elapsed int, content string) string {
				return fmt.Sprintf(`{"ordinal":%d,"type":"response_item","payload":{"type":"message","id":"reply","role":"user","content":[{"type":"input_text","text":"working"},%s],"elapsed_time_ms":%d}}`, ordinal, content, elapsed)
			}
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, response(9, 100, test.first))
			activity, _, progress := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			appendUsageEvents(t, child, response(10, 200, test.first))
			activity.poll()
			if *progress != 1 {
				t.Fatalf("metadata replay progress=%d, want 1", *progress)
			}
			appendUsageEvents(t, child, response(11, 300, test.changed))
			activity.poll()
			if *progress != 2 {
				t.Fatalf("changed visible content progress=%d, want 2", *progress)
			}
		})
	}
}

func TestRolloutActivityAgentMessageHiddenContentDoesNotRenewProgress(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	boundary := int64(9)
	response := func(ordinal, elapsed int, text, encrypted string) string {
		return fmt.Sprintf(`{"ordinal":%d,"type":"response_item","payload":{"type":"agent_message","author":"child","recipient":"root","content":[{"type":"input_text","text":%q,"elapsed_time_ms":%d},{"type":"encrypted_content","encrypted_content":%q}],"elapsed_time_ms":%d}}`, ordinal, text, elapsed, encrypted, elapsed)
	}
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, response(9, 100, "working", "private one"))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	for _, step := range []struct {
		line string
		want int
	}{
		{response(10, 200, "working", "private two"), 1},
		{response(11, 300, "more work", "private two"), 2},
	} {
		appendUsageEvents(t, child, step.line)
		activity.poll()
		if *progress != step.want {
			t.Fatalf("agent message progress=%d, want %d", *progress, step.want)
		}
	}
}

func TestRolloutActivityCompletedQuestionsOnlyMessageRenewsProgress(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	boundary := int64(9)
	completion := func(ordinal, elapsed int) string {
		item := fmt.Sprintf(`{"type":"AgentMessage","id":"question","content":[],"questions":[{"question":"Which target?"}],"elapsed_time_ms":%d}`, elapsed)
		return rolloutCompletedItemTurn(ordinal, "child", "child-turn", item)
	}
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, completion(9, 100))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("first structured question progress=%d, want 1", *progress)
	}
	appendUsageEvents(t, child, completion(10, 200))
	activity.poll()
	if *progress != 1 {
		t.Fatalf("replayed structured question renewed progress: %d", *progress)
	}
}

func TestRolloutActivityCompletedItemHiddenReplayDoesNotRenewProgress(t *testing.T) {
	for _, tc := range []struct {
		name, first, replay, changed string
	}{
		{
			name:    "agent message",
			first:   `{"type":"AgentMessage","id":"message","content":[{"type":"Text","text":"working","elapsed_time_ms":1},{"type":"Encrypted","encrypted_content":"cipher one"}]}`,
			replay:  `{"type":"AgentMessage","id":"message","content":[{"type":"Text","text":"working","elapsed_time_ms":2},{"type":"Encrypted","encrypted_content":"cipher two"}]}`,
			changed: `{"type":"AgentMessage","id":"message","content":[{"type":"Text","text":"more work","elapsed_time_ms":2},{"type":"Encrypted","encrypted_content":"cipher two"}]}`,
		},
		{
			name:    "reasoning",
			first:   `{"type":"Reasoning","id":"thought","content":[{"type":"Encrypted","encrypted_content":"cipher one"}],"summary_text":["summary"]}`,
			replay:  `{"type":"Reasoning","id":"thought","content":[{"type":"Encrypted","encrypted_content":"cipher two"}],"summary_text":["summary"]}`,
			changed: `{"type":"Reasoning","id":"thought","content":[{"type":"Encrypted","encrypted_content":"cipher two"}],"summary_text":["new summary"]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
			boundary := int64(9)
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
				rolloutCompletedItemTurn(9, "child", "child-turn", tc.first))
			activity, _, progress := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			if *progress != 1 {
				t.Fatalf("initial progress=%d, want 1", *progress)
			}
			appendUsageEvents(t, child, rolloutCompletedItemTurn(10, "child", "child-turn", tc.replay))
			activity.poll()
			if *progress != 1 {
				t.Fatalf("hidden replay renewed progress: %d", *progress)
			}
			appendUsageEvents(t, child, rolloutCompletedItemTurn(11, "child", "child-turn", tc.changed))
			activity.poll()
			if *progress != 2 {
				t.Fatalf("changed visible content progress=%d, want 2", *progress)
			}
		})
	}
}

func TestRolloutActivityCompletedUserMessageMetadataReplayDoesNotRenewProgress(t *testing.T) {
	for _, tc := range []struct {
		name, first, replay, changed string
	}{
		{
			name:    "text elements",
			first:   `{"type":"UserMessage","id":"prompt","content":[{"type":"text","text":"inspect this","text_elements":[{"start":0,"end":7,"kind":"plain"}]}]}`,
			replay:  `{"type":"UserMessage","id":"prompt","content":[{"type":"text","text":"inspect this","text_elements":[{"start":0,"end":7,"kind":"styled"}],"elapsed_time_ms":2}]}`,
			changed: `{"type":"UserMessage","id":"prompt","content":[{"type":"text","text":"inspect that","text_elements":[{"start":0,"end":7,"kind":"styled"}]}]}`,
		},
		{
			name:    "attachment identity",
			first:   `{"type":"UserMessage","id":"prompt","content":[{"type":"text","text":"inspect"},{"type":"image","fileId":"image-one","detail":"high","display_name":"first"}]}`,
			replay:  `{"type":"UserMessage","id":"prompt","content":[{"type":"text","text":"inspect"},{"type":"image","fileId":"image-one","detail":"high","display_name":"second"}]}`,
			changed: `{"type":"UserMessage","id":"prompt","content":[{"type":"text","text":"inspect"},{"type":"image","fileId":"image-two","detail":"high","display_name":"second"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
			boundary := int64(9)
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
				rolloutCompletedItemTurn(9, "child", "child-turn", tc.first))
			activity, _, progress := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			if *progress != 1 {
				t.Fatalf("initial progress=%d, want 1", *progress)
			}
			appendUsageEvents(t, child, rolloutCompletedItemTurn(10, "child", "child-turn", tc.replay))
			activity.poll()
			if *progress != 1 {
				t.Fatalf("metadata replay renewed progress: %d", *progress)
			}
			appendUsageEvents(t, child, rolloutCompletedItemTurn(11, "child", "child-turn", tc.changed))
			activity.poll()
			if *progress != 2 {
				t.Fatalf("changed content progress=%d, want 2", *progress)
			}
		})
	}
}

func TestRolloutActivityCompletedToolResultMetadataReplayDoesNotRenewProgress(t *testing.T) {
	for _, tc := range []struct {
		name, first, replay, changed string
	}{
		{
			name:    "function call encrypted output",
			first:   `{"type":"FunctionCallOutput","id":"output","output":[{"type":"input_text","text":"working"},{"type":"encrypted_content","encrypted_content":"cipher one"}]}`,
			replay:  `{"type":"FunctionCallOutput","id":"output","output":[{"type":"input_text","text":"working"},{"type":"encrypted_content","encrypted_content":"cipher two"}]}`,
			changed: `{"type":"FunctionCallOutput","id":"output","output":[{"type":"input_text","text":"more work"},{"type":"encrypted_content","encrypted_content":"cipher two"}]}`,
		},
		{
			name:    "MCP result metadata",
			first:   `{"type":"McpToolCall","id":"tool","status":"completed","result":{"_meta":{"trace":"one"},"content":[{"type":"text","text":"working","annotations":{"priority":0.5},"_meta":{"trace":"one"}}],"structuredContent":{"status":"one"}}}`,
			replay:  `{"type":"McpToolCall","id":"tool","status":"completed","result":{"_meta":{"trace":"two"},"content":[{"type":"text","text":"working","annotations":{"priority":0.9},"_meta":{"trace":"two"}}],"structuredContent":{"status":"one"}}}`,
			changed: `{"type":"McpToolCall","id":"tool","status":"completed","result":{"_meta":{"trace":"two"},"content":[{"type":"text","text":"working","annotations":{"priority":0.9},"_meta":{"trace":"two"}}],"structuredContent":{"status":"two"}}}`,
		},
		{
			name:    "MCP nested structured content",
			first:   `{"type":"McpToolCall","id":"tool","status":"completed","result":{"content":[{"type":"text","text":"working"}],"structuredContent":{"_meta":{"meaning":"one"}}}}`,
			replay:  `{"type":"McpToolCall","id":"tool","status":"completed","result":{"_meta":{"trace":"two"},"content":[{"type":"text","text":"working"}],"structuredContent":{"_meta":{"meaning":"one"}}}}`,
			changed: `{"type":"McpToolCall","id":"tool","status":"completed","result":{"_meta":{"trace":"two"},"content":[{"type":"text","text":"working"}],"structuredContent":{"_meta":{"meaning":"two"}}}}`,
		},
		{
			name:    "MCP arbitrary block content",
			first:   `{"type":"McpToolCall","id":"tool","status":"completed","result":{"content":[{"type":"text","text":"working","custom":{"_meta":{"meaning":"one"}},"annotations":{"priority":0.5}}]}}`,
			replay:  `{"type":"McpToolCall","id":"tool","status":"completed","result":{"content":[{"type":"text","text":"working","custom":{"_meta":{"meaning":"one"}},"annotations":{"priority":0.9}}]}}`,
			changed: `{"type":"McpToolCall","id":"tool","status":"completed","result":{"content":[{"type":"text","text":"working","custom":{"_meta":{"meaning":"two"}},"annotations":{"priority":0.9}}]}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
			boundary := int64(9)
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
				rolloutCompletedItemTurn(9, "child", "child-turn", tc.first))
			activity, _, progress := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			if *progress != 1 {
				t.Fatalf("initial progress=%d, want 1", *progress)
			}
			appendUsageEvents(t, child, rolloutCompletedItemTurn(10, "child", "child-turn", tc.replay))
			activity.poll()
			if *progress != 1 {
				t.Fatalf("metadata replay renewed progress: %d", *progress)
			}
			appendUsageEvents(t, child, rolloutCompletedItemTurn(11, "child", "child-turn", tc.changed))
			activity.poll()
			if *progress != 2 {
				t.Fatalf("changed output progress=%d, want 2", *progress)
			}
		})
	}
}

func TestRolloutActivityResponseFunctionOutputEncryptedReplayDoesNotRenewProgress(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	boundary := int64(9)
	response := func(ordinal int, text, cipher, summary string) string {
		return fmt.Sprintf(`{"ordinal":%d,"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":%q},{"type":"encrypted_content","encrypted_content":%q}],"summary":[{"type":"summary_text","text":%q}]}}`, ordinal, text, cipher, summary)
	}
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, response(9, "working", "cipher one", "summary one"))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("initial progress=%d, want 1", *progress)
	}
	appendUsageEvents(t, child, response(10, "working", "cipher two", "summary one"))
	activity.poll()
	if *progress != 1 {
		t.Fatalf("ciphertext replay renewed progress: %d", *progress)
	}
	appendUsageEvents(t, child, response(11, "working", "cipher two", "summary two"))
	activity.poll()
	if *progress != 1 {
		t.Fatalf("summary replay renewed progress: %d", *progress)
	}
	appendUsageEvents(t, child, response(12, "more work", "cipher two", "summary two"))
	activity.poll()
	if *progress != 2 {
		t.Fatalf("changed output progress=%d, want 2", *progress)
	}
}

func TestRolloutActivityEncryptedOnlyAgentMessageRenewsOnce(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	boundary := int64(9)
	message := func(ordinal int, encrypted string) string {
		return fmt.Sprintf(`{"ordinal":%d,"type":"response_item","payload":{"type":"agent_message","author":"child","recipient":"root","content":[{"type":"encrypted_content","encrypted_content":%q}]}}`, ordinal, encrypted)
	}
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, message(9, "private one"))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("first encrypted helper message progress=%d, want 1", *progress)
	}
	appendUsageEvents(t, child, message(10, "private two"))
	activity.poll()
	if *progress != 1 {
		t.Fatalf("encrypted-only replay renewed progress: %d", *progress)
	}
}

func TestRolloutActivityProgressCountsRepeatedOutputAtDistinctOrdinals(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	boundary := int64(9)
	first := `{"ordinal":9,"type":"event_msg","payload":{"type":"exec_command_output","thread_id":"child","turn_id":"current","chunk":"tick\n"}}`
	second := `{"ordinal":10,"type":"event_msg","payload":{"type":"exec_command_output","thread_id":"child","turn_id":"current","chunk":"tick\n"}}`
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, first)
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("first output progress=%d, want 1", *progress)
	}
	appendUsageEvents(t, child, second)
	activity.poll()
	if *progress != 2 {
		t.Fatalf("second output progress=%d, want 2", *progress)
	}
	appendUsageEvents(t, child, second)
	activity.poll()
	if *progress != 2 {
		t.Fatalf("replayed ordinal renewed progress: %d", *progress)
	}
}

func TestRolloutActivityRootCommandOutputRenewsProgress(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	root := writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.poll()
	if *progress != 0 {
		t.Fatalf("root metadata renewed progress: %d", *progress)
	}
	chunk := `{"ordinal":9,"type":"event_msg","payload":{"type":"exec_command_output","thread_id":"root","turn_id":"root-turn","chunk":"still working"}}`
	appendUsageEvents(t, root, chunk)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("root output progress=%d, want 1", *progress)
	}
	appendUsageEvents(t, root, chunk,
		`{"ordinal":10,"type":"event_msg","payload":{"type":"exec_command_output","thread_id":"root","turn_id":"root-turn","chunk":""}}`)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("replayed or empty root output renewed progress: %d", *progress)
	}
}

func TestRolloutActivityOrdinalProgressDoesNotFillIdentityCap(t *testing.T) {
	activity := rolloutActivity{progress: func() {}}
	event := rolloutEvent{Type: "event_msg", Payload: []byte(`{"type":"exec_command_output","chunk":"tick"}`)}
	progressed := false
	activity.newProgress("helper", event, 1, &progressed)
	if !progressed || activity.progressOrdinal["helper"] != 1 || len(activity.progressEvents) != 0 {
		t.Fatalf("first ordinal progress: changed=%t, ordinal=%d, identities=%d", progressed, activity.progressOrdinal["helper"], len(activity.progressEvents))
	}
	progressed = false
	activity.newProgress("helper", event, 1, &progressed)
	if progressed {
		t.Fatal("replayed ordinal renewed progress")
	}
	activity.newProgress("helper", event, maxUsageRecords+1, &progressed)
	if !progressed || activity.progressOrdinal["helper"] != maxUsageRecords+1 || len(activity.progressEvents) != 0 {
		t.Fatalf("later ordinal progress: changed=%t, ordinal=%d, identities=%d", progressed, activity.progressOrdinal["helper"], len(activity.progressEvents))
	}
}

func TestRolloutActivityUnorderedIdentityCapStopsRun(t *testing.T) {
	stops := 0
	activity := rolloutActivity{progress: func() {}, stopRun: func() { stops++ }, progressEventLimit: 1}
	progressed := false
	first := rolloutEvent{Type: "event_msg", Payload: []byte(`{"chunk":"first"}`)}
	second := rolloutEvent{Type: "event_msg", Payload: []byte(`{"chunk":"second"}`)}
	activity.newProgress("helper", first, 0, &progressed)
	if !progressed || activity.progressLimit || stops != 0 {
		t.Fatalf("first unordered event: progressed=%t limit=%t stops=%d", progressed, activity.progressLimit, stops)
	}
	progressed = false
	activity.newProgress("helper", second, 0, &progressed)
	activity.newProgress("helper", second, 0, &progressed)
	if progressed || !activity.progressLimit || stops != 1 {
		t.Fatalf("overflowed unordered events: progressed=%t limit=%t stops=%d", progressed, activity.progressLimit, stops)
	}
}

func TestRolloutActivityProgressCountsDistinctIdenticalHelperMessages(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	boundary := int64(9)
	message := func(ordinal int) string {
		return fmt.Sprintf(`{"ordinal":%d,"type":"inter_agent_communication","payload":{"author":"child","recipient":"root","content":"still working"}}`, ordinal)
	}
	first, second := message(9), message(10)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, first)
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("first helper message progress=%d, want 1", *progress)
	}
	appendUsageEvents(t, child, second)
	activity.poll()
	if *progress != 2 {
		t.Fatalf("second distinct helper message progress=%d, want 2", *progress)
	}
	appendUsageEvents(t, child, second)
	activity.poll()
	if *progress != 2 {
		t.Fatalf("replayed helper message renewed progress: %d", *progress)
	}
}

func TestRolloutActivityProvenNativeEventsCountDistinctOrdinals(t *testing.T) {
	for _, test := range []struct{ eventType, content string }{
		{"user_message", `,"message":"same work"`},
		{"agent_message", `,"message":"same work"`},
		{"agent_reasoning", `,"text":"same work"`},
		{"agent_reasoning_raw_content", `,"text":"same work"`},
	} {
		t.Run(test.eventType, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
			boundary := int64(9)
			payload := fmt.Sprintf(`{"type":%q,"thread_id":"child","turn_id":"current"%s}`, test.eventType, test.content)
			event := func(ordinal int) string {
				return fmt.Sprintf(`{"ordinal":%d,"type":"event_msg","payload":%s}`, ordinal, payload)
			}
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, event(9))
			activity, _, progress := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			if *progress != 1 {
				t.Fatalf("first native event progress=%d, want 1", *progress)
			}
			appendUsageEvents(t, child, event(10))
			activity.poll()
			if *progress != 2 {
				t.Fatalf("distinct native event progress=%d, want 2", *progress)
			}
			appendUsageEvents(t, child, event(10))
			activity.poll()
			if *progress != 2 {
				t.Fatalf("replayed ordinal renewed progress: %d", *progress)
			}
		})
	}
}

func TestRolloutActivityToolLifecycleRequiresNewWork(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	boundary := int64(9)
	toolEnd := func(ordinal int, callID, result string, elapsed int) string {
		return fmt.Sprintf(`{"ordinal":%d,"type":"event_msg","payload":{"type":"mcp_tool_call_end","thread_id":"child","turn_id":"current","call_id":%q,"result":%q,"elapsed_time_ms":%d}}`, ordinal, callID, result, elapsed)
	}
	commandBegin := func(ordinal int, command string) string {
		return fmt.Sprintf(`{"ordinal":%d,"type":"event_msg","payload":{"type":"exec_command_begin","thread_id":"child","turn_id":"current","command":%q}}`, ordinal, command)
	}
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		toolEnd(9, "call-1", "PASS", 100))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("first tool end progress=%d, want 1", *progress)
	}
	for i, step := range []struct {
		line string
		want int
	}{
		{toolEnd(10, "call-1", "PASS", 100), 1},
		{toolEnd(11, "call-1", "PASS", 200), 1},
		{toolEnd(12, "call-2", "PASS", 100), 2},
		{toolEnd(13, "call-2", "MORE", 200), 3},
		{commandBegin(14, "go test"), 4},
		{commandBegin(15, "go test"), 4},
		{commandBegin(16, "go vet"), 5},
	} {
		appendUsageEvents(t, child, step.line)
		activity.poll()
		if *progress != step.want {
			t.Fatalf("step %d: progress=%d, want %d", i, *progress, step.want)
		}
	}
}

func TestRolloutActivityToolLifecycleNativeOutputRenewsProgress(t *testing.T) {
	for _, test := range []struct {
		name, eventType, first, changed string
	}{
		{"MCP invocation", "mcp_tool_call_end", `"invocation":{"server":"repo","tool":"search","arguments":{"query":"one"}}`, `"invocation":{"server":"repo","tool":"search","arguments":{"query":"two"}}`},
		{"exec stdout", "exec_command_end", `"stdout":"one"`, `"stdout":"two"`},
		{"exec stderr", "exec_command_end", `"stderr":"one"`, `"stderr":"two"`},
		{"exec aggregate", "exec_command_end", `"aggregated_output":"one"`, `"aggregated_output":"two"`},
		{"exec formatted output", "exec_command_end", `"formatted_output":"one"`, `"formatted_output":"two"`},
		{"patch success", "patch_apply_end", `"success":false`, `"success":true`},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
			boundary := int64(9)
			toolEnd := func(ordinal, elapsed int, content string) string {
				return fmt.Sprintf(`{"ordinal":%d,"type":"event_msg","payload":{"type":%q,"thread_id":"child","turn_id":"current","call_id":"call-1",%s,"elapsed_time_ms":%d}}`, ordinal, test.eventType, content, elapsed)
			}
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, toolEnd(9, 100, test.first))
			activity, _, progress := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			appendUsageEvents(t, child, toolEnd(10, 200, test.first))
			activity.poll()
			if *progress != 1 {
				t.Fatalf("metadata replay progress=%d, want 1", *progress)
			}
			appendUsageEvents(t, child, toolEnd(11, 300, test.changed))
			activity.poll()
			if *progress != 2 {
				t.Fatalf("changed native output progress=%d, want 2", *progress)
			}
		})
	}
}

func TestRolloutMCPToolResultMetadataDoesNotRenewProgress(t *testing.T) {
	for _, tc := range []struct {
		name, first, replay, changed string
	}{
		{
			name:    "Ok result",
			first:   `{"type":"mcp_tool_call_end","turn_id":"turn","call_id":"call","result":{"Ok":{"_meta":{"trace":"one"},"content":[{"type":"text","text":"same","annotations":{"priority":0.1},"_meta":{"trace":"one"}}],"structuredContent":{"status":"same"}}}}`,
			replay:  `{"type":"mcp_tool_call_end","turn_id":"turn","call_id":"call","result":{"Ok":{"_meta":{"trace":"two"},"content":[{"type":"text","text":"same","annotations":{"priority":0.9},"_meta":{"trace":"two"}}],"structuredContent":{"status":"same"}}}}`,
			changed: `{"type":"mcp_tool_call_end","turn_id":"turn","call_id":"call","result":{"Ok":{"_meta":{"trace":"two"},"content":[{"type":"text","text":"same","annotations":{"priority":0.9},"_meta":{"trace":"two"}}],"structuredContent":{"status":"changed"}}}}`,
		},
		{
			name:    "direct result",
			first:   `{"type":"mcp_tool_call_end","turn_id":"turn","call_id":"call","result":{"_meta":{"trace":"one"},"content":[{"type":"text","text":"same","annotations":{"priority":0.1}}]}}`,
			replay:  `{"type":"mcp_tool_call_end","turn_id":"turn","call_id":"call","result":{"_meta":{"trace":"two"},"content":[{"type":"text","text":"same","annotations":{"priority":0.9}}]}}`,
			changed: `{"type":"mcp_tool_call_end","turn_id":"turn","call_id":"call","result":{"_meta":{"trace":"two"},"content":[{"type":"text","text":"changed","annotations":{"priority":0.9}}]}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &rolloutActivity{progress: func() {}}
			for i, step := range []struct {
				raw  string
				want bool
			}{{tc.first, true}, {tc.replay, false}, {tc.changed, true}} {
				progressed := false
				a.newToolProgress("child", []byte(step.raw), &progressed)
				if progressed != step.want {
					t.Fatalf("step %d: progressed=%t, want %t", i, progressed, step.want)
				}
			}
		})
	}
}

func TestRolloutCustomToolOutputCiphertextDoesNotRenewProgress(t *testing.T) {
	a := &rolloutActivity{progress: func() {}}
	for i, step := range []struct {
		raw  string
		want bool
	}{
		{`{"type":"custom_tool_call_output","id":"output","call_id":"call","output":[{"type":"input_text","text":"same"},{"type":"encrypted_content","encrypted_content":"cipher-one"}]}`, true},
		{`{"type":"custom_tool_call_output","id":"output","call_id":"call","output":[{"type":"input_text","text":"same"},{"type":"encrypted_content","encrypted_content":"cipher-two"}]}`, false},
		{`{"type":"custom_tool_call_output","id":"output","call_id":"call","output":[{"type":"input_text","text":"changed"},{"type":"encrypted_content","encrypted_content":"cipher-two"}]}`, true},
	} {
		progressed := false
		a.newResponseProgress("child", []byte(step.raw), &progressed)
		if progressed != step.want {
			t.Fatalf("step %d: progressed=%t, want %t", i, progressed, step.want)
		}
	}
}

func TestRolloutCustomToolFileOutputRenewsProgress(t *testing.T) {
	a := &rolloutActivity{progress: func() {}}
	for i, step := range []struct {
		raw  string
		want bool
	}{
		{`{"type":"custom_tool_call_output","id":"output","call_id":"call","output":[{"type":"input_file","file_data":"first","file_url":"https://example.invalid/first","filename":"one.txt"}]}`, true},
		{`{"type":"custom_tool_call_output","id":"output","call_id":"call","output":[{"type":"input_file","file_data":"first","file_url":"https://example.invalid/first","filename":"one.txt","prompt_cache_breakpoint":{"ttl":"30m"}}]}`, false},
		{`{"type":"custom_tool_call_output","id":"output","call_id":"call","output":[{"type":"input_file","file_data":"second","file_url":"https://example.invalid/first","filename":"one.txt"}]}`, true},
		{`{"type":"custom_tool_call_output","id":"output","call_id":"call","output":[{"type":"input_file","file_data":"second","file_url":"https://example.invalid/second","filename":"one.txt"}]}`, true},
		{`{"type":"custom_tool_call_output","id":"output","call_id":"call","output":[{"type":"input_file","file_data":"second","file_url":"https://example.invalid/second","filename":"two.txt"}]}`, true},
	} {
		progressed := false
		a.newResponseProgress("child", []byte(step.raw), &progressed)
		if progressed != step.want {
			t.Fatalf("step %d: progressed=%t, want %t", i, progressed, step.want)
		}
	}
}

func TestRolloutActivityTaskLifecycleDedupesAcrossOrdinals(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		usageEventAt(9, "task_started", "child-turn"))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("first helper task start progress=%d, want 1", *progress)
	}
	for _, step := range []struct {
		line string
		want int
	}{
		{usageEventAt(10, "task_started", "child-turn"), 1},
		{`{"ordinal":11,"type":"event_msg","payload":{"type":"task_started","turn_id":"child-turn","elapsed_time_ms":500}}`, 1},
		{usageEventAt(12, "task_complete", "child-turn"), 2},
		{usageEventAt(13, "task_complete", "child-turn"), 2},
		{usageEventAt(14, "task_started", "next-turn"), 3},
		{usageEventAt(15, "turn_aborted", "next-turn"), 4},
		{usageEventAt(16, "turn_aborted", "next-turn"), 4},
	} {
		appendUsageEvents(t, child, step.line)
		activity.poll()
		if *progress != step.want {
			t.Fatalf("after %s: progress=%d, want %d", step.line, *progress, step.want)
		}
	}
}

func TestRolloutActivityAttributableNestedSpawnRenewsOnce(t *testing.T) {
	for _, test := range []struct {
		name, payload string
	}{
		{"native spawn end", `{"type":"collab_agent_spawn_end","thread_id":"child","turn_id":"current","new_thread_id":"grandchild"}`},
		{"native activity", `{"type":"sub_agent_activity","thread_id":"child","turn_id":"current","kind":"started","agent_thread_id":"grandchild"}`},
		{"completed activity", `{"type":"item_completed","thread_id":"child","turn_id":"current","item":{"type":"SubAgentActivity","kind":"started","agent_thread_id":"grandchild"}}`},
		{"completed spawn tool", `{"type":"item_completed","thread_id":"child","turn_id":"current","item":{"type":"CollabAgentToolCall","id":"spawn","tool":"spawn_agent","status":"completed","receiver_thread_ids":["grandchild"]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
			boundary := int64(9)
			event := func(ordinal int) string {
				return fmt.Sprintf(`{"ordinal":%d,"type":"event_msg","payload":%s}`, ordinal, test.payload)
			}
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, event(9))
			activity, _, progress := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			if *progress != 1 {
				t.Fatalf("first nested spawn progress=%d, want 1", *progress)
			}
			appendUsageEvents(t, child, event(10))
			activity.poll()
			if *progress != 1 {
				t.Fatalf("replayed nested spawn renewed progress: %d", *progress)
			}
		})
	}
}

func TestRolloutActivityNativeEventWithoutOrdinalDedupesPayload(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	payload := `{"type":"agent_message","thread_id":"child","turn_id":"current","message":"same work"}`
	zero := fmt.Sprintf(`{"ordinal":0,"type":"event_msg","payload":%s}`, payload)
	missing := fmt.Sprintf(`{"type":"event_msg","payload":%s}`, payload)
	child := writeUsageRolloutBoundary(t, home, "child", "root", nil, zero)
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("first no-ordinal event progress=%d, want 1", *progress)
	}
	appendUsageEvents(t, child, missing)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("replayed no-ordinal event renewed progress: %d", *progress)
	}
}

func TestRolloutActivityFreshHelperCommunicationWithoutTurnRenewsProgress(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", rolloutSpawn(9, "child"))
	child := writeUsageRolloutBoundary(t, home, "child", "root", nil,
		`{"ordinal":8,"type":"inter_agent_communication","payload":{"author":"root","recipient":"child","content":"copied parent history"}}`)
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.poll() // The root spawn is work; copied helper history is not.
	if *progress != 1 {
		t.Fatalf("root spawn progress=%d, want 1", *progress)
	}
	message := func(ordinal int, author string) string {
		return fmt.Sprintf(`{"ordinal":%d,"type":"inter_agent_communication","payload":{"author":%q,"recipient":"root","content":"still working"}}`, ordinal, author)
	}
	first := message(10, "child")
	appendUsageEvents(t, child, first)
	activity.poll()
	if *progress != 2 {
		t.Fatalf("first child-authored message progress=%d, want 2", *progress)
	}
	appendUsageEvents(t, child, message(11, "child"))
	activity.poll()
	if *progress != 3 {
		t.Fatalf("second child-authored message progress=%d, want 3", *progress)
	}
	appendUsageEvents(t, child, first, message(12, "other"))
	activity.poll()
	if *progress != 3 {
		t.Fatalf("replayed or foreign-authored message renewed progress: %d", *progress)
	}
}

func TestRolloutActivityRootSpawnAfterStdoutDoesNotRenewProgress(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", rolloutSpawn(9, "child"))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true) // Root stdout already reported this spawn.
	activity.poll()
	if *progress != 0 {
		t.Fatalf("duplicate root rollout spawn renewed progress: %d", *progress)
	}
}

func TestRootSpawnProgressDedupesRolloutAndStdout(t *testing.T) {
	for _, test := range []struct {
		name, first          string
		lifecycle, delayedID bool
		want                 int
	}{
		{"rollout first", "rollout", false, false, 1},
		{"rollout first lifecycle", "rollout", true, false, 1},
		{"stdout first", "stdout", false, false, 1},
		{"stdout only", "", false, false, 1},
		{"stdout lifecycle", "", true, false, 2},
		{"stdout delayed child ID", "", true, true, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			rootEvents := []string{usageEventAt(8, "task_started", "root-turn")}
			if test.first != "" {
				rootEvents = append(rootEvents, rolloutSpawn(9, "child"))
			}
			writeUsageRollout(t, home, "root", "", rootEvents...)
			activity, _, progress := newActivityTestRecorder(t, home, started)
			activity.root("root")
			output := jsonlOutput{onProgress: func() { *progress++ }, onChild: func(ids []string, spawned bool) bool {
				fresh := false
				for _, id := range ids {
					fresh = activity.child(id, spawned) || fresh
				}
				return fresh
			}}
			_, _ = output.Write([]byte(`{"type":"thread.started","thread_id":"root"}` + "\n"))
			stdout := func() {
				if test.lifecycle {
					start := `{"type":"item.started","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent","receiver_thread_ids":["child"]}}`
					if test.delayedID {
						start = `{"type":"item.started","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent"}}`
					}
					_, _ = output.Write([]byte(start + "\n"))
				}
				_, _ = output.Write([]byte(`{"type":"item.completed","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent","receiver_thread_ids":["child"]}}` + "\n"))
			}
			if test.first == "rollout" {
				activity.poll()
				stdout()
			} else {
				stdout()
				activity.poll()
			}
			if *progress != test.want {
				t.Fatalf("root spawn progress=%d, want %d", *progress, test.want)
			}
		})
	}
}

func TestRootSpawnProgressDoesNotReplayAfterStdoutStartWithoutChildID(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", rolloutSpawn(9, "child"))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	output := jsonlOutput{onProgress: func() { *progress++ }, onChild: func(ids []string, spawned bool) bool {
		fresh := false
		for _, id := range ids {
			fresh = activity.child(id, spawned) || fresh
		}
		return fresh
	}}
	for _, line := range []string{
		`{"type":"thread.started","thread_id":"root"}`,
		`{"type":"item.started","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent"}}`,
	} {
		_, _ = output.Write([]byte(line + "\n"))
	}
	if *progress != 1 {
		t.Fatalf("stdout tool start progress=%d, want 1", *progress)
	}
	activity.poll()
	if *progress != 2 {
		t.Fatalf("rollout child start progress=%d, want 2", *progress)
	}
	_, _ = output.Write([]byte(`{"type":"item.completed","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent","receiver_thread_ids":["child"]}}` + "\n"))
	if *progress != 2 {
		t.Fatalf("stdout completion replayed rollout child start: %d", *progress)
	}
}

func TestRootSpawnProgressCreditsDelayedChildIDOnce(t *testing.T) {
	for _, test := range []struct {
		name         string
		rolloutFirst bool
	}{
		{name: "stdout first"},
		{name: "rollout first", rolloutFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			writeUsageRollout(t, home, "root", "", rolloutSpawn(9, "child"))
			activity, _, progress := newActivityTestRecorder(t, home, started)
			activity.root("root")
			output := jsonlOutput{onProgress: func() { *progress++ }, onChild: func(ids []string, spawned bool) bool {
				fresh := false
				for _, id := range ids {
					fresh = activity.child(id, spawned) || fresh
				}
				return fresh
			}}
			write := func(line string) {
				t.Helper()
				if _, err := output.Write([]byte(line + "\n")); err != nil {
					t.Fatal(err)
				}
			}
			write(`{"type":"thread.started","thread_id":"root"}`)
			write(`{"type":"item.started","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent"}}`)
			if *progress != 1 {
				t.Fatalf("spawn start progress=%d, want 1", *progress)
			}
			if test.rolloutFirst {
				activity.poll()
			}
			update := `{"type":"item.updated","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent","receiver_thread_ids":["child"]}}`
			write(update)
			if *progress != 2 {
				t.Fatalf("first child admission progress=%d, want 2", *progress)
			}
			write(update)
			activity.poll()
			if *progress != 2 {
				t.Fatalf("replayed child admission progress=%d, want 2", *progress)
			}
			write(`{"type":"item.completed","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent","receiver_thread_ids":["child"]}}`)
			want := 3
			if test.rolloutFirst {
				want = 2
			}
			if *progress != want {
				t.Fatalf("spawn completion progress=%d, want %d", *progress, want)
			}
		})
	}
}

func TestRootSpawnProgressCreditsEachNewReceiverID(t *testing.T) {
	progress := 0
	children := make(map[string]bool)
	output := jsonlOutput{
		onProgress: func() { progress++ },
		onChild: func(ids []string, spawned bool) bool {
			fresh := false
			for _, id := range ids {
				if !children[id] {
					children[id], fresh = true, true
				}
			}
			return fresh
		},
	}
	write := func(line string, want int) {
		t.Helper()
		if _, err := output.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		if progress != want {
			t.Fatalf("after %s: progress=%d, want %d", line, progress, want)
		}
	}
	write(`{"type":"thread.started","thread_id":"root"}`, 0)
	write(`{"type":"item.started","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent"}}`, 1)
	first := `{"type":"item.updated","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent","receiver_thread_ids":["child-1"]}}`
	write(first, 2)
	write(first, 2)
	second := `{"type":"item.updated","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent","receiver_thread_ids":["child-1","child-2"]}}`
	write(second, 3)
	write(second, 3)
}

func TestRootSpawnProgressWithoutChildObserverDoesNotReplayReceiverIDs(t *testing.T) {
	progress := 0
	output := jsonlOutput{onProgress: func() { progress++ }}
	for _, line := range []string{
		`{"type":"thread.started","thread_id":"root"}`,
		`{"type":"item.started","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent"}}`,
		`{"type":"item.updated","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent","receiver_thread_ids":["child-1"]}}`,
		`{"type":"item.updated","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent","receiver_thread_ids":["child-1"]}}`,
		`{"type":"item.updated","item":{"type":"collab_tool_call","id":"spawn","tool":"spawn_agent","receiver_thread_ids":["child-1","child-2"]}}`,
	} {
		if _, err := output.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if progress != 2 {
		t.Fatalf("unobserved receiver IDs renewed progress: got %d, want 2", progress)
	}
}

func TestRolloutActivityResumedHelperCommunicationRequiresTurn(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "root-response", 1)...)
	child := writeUsageRolloutBoundary(t, home, "child", "root", nil,
		usageEventAt(9, "task_started", "old-child"),
		usageRecordAt(10, "child", "old-child", "child-response", 1, 0, 0),
		usageEventAt(11, "task_complete", "old-child"))
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	if session.before["child"] == nil {
		t.Fatal("fixture did not establish a resumed helper prefix")
	}
	appendUsageEvents(t, child,
		`{"ordinal":12,"type":"inter_agent_communication","payload":{"author":"child","recipient":"root","content":"unattributed suffix"}}`)
	progress := 0
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil, nil,
		func() { progress++ })
	activity.root("root")
	activity.child("child", false)
	activity.poll()
	if progress != 0 {
		t.Fatalf("resumed message without turn renewed progress: %d", progress)
	}
	appendUsageEvents(t, child,
		`{"ordinal":13,"type":"inter_agent_communication","payload":{"author":"child","recipient":"root","content":"current suffix","internal_chat_message_metadata_passthrough":{"turn_id":"new-child"}}}`)
	activity.poll()
	if progress != 1 {
		t.Fatalf("resumed message with current turn progress=%d, want 1", progress)
	}
}

func TestRolloutActivityResumedHelperOldCommunicationTurnDoesNotRenewProgress(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "root-response", 1)...)
	boundary := int64(9)
	message := func(ordinal int, turn string) string {
		return fmt.Sprintf(`{"ordinal":%d,"type":"inter_agent_communication","payload":{"author":"child","recipient":"root","content":"still working","internal_chat_message_metadata_passthrough":{"turn_id":%q}}}`, ordinal, turn)
	}
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		message(8, "inherited-child"),
		`{"ordinal":9,"type":"inter_agent_communication","payload":{"author":"root","recipient":"child","content":"instructions","internal_chat_message_metadata_passthrough":{"turn_id":"foreign-root"}}}`,
		message(10, "old-child"))
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	if before := session.before["child"]; before == nil || !before.turns["old-child"] || before.turns["inherited-child"] || before.turns["foreign-root"] {
		t.Fatalf("baseline inter-agent turn ownership: %+v", before)
	}
	appendUsageEvents(t, child, message(11, "old-child"))
	progress := 0
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil, nil,
		func() { progress++ })
	activity.root("root")
	activity.child("child", false)
	activity.poll()
	if progress != 0 {
		t.Fatalf("reappended old inter-agent turn renewed progress: %d", progress)
	}
	appendUsageEvents(t, child, message(12, "new-child"))
	activity.poll()
	if progress != 1 {
		t.Fatalf("new inter-agent turn progress=%d, want 1", progress)
	}
}

func TestRolloutActivityProgressWithoutOrdinalDedupesPayload(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEventAt(8, "task_started", "root-turn"))
	zero := `{"ordinal":0,"type":"event_msg","payload":{"type":"exec_command_output","thread_id":"child","turn_id":"current","chunk":"tick\n"}}`
	missing := `{"type":"event_msg","payload":{"type":"exec_command_output","thread_id":"child","turn_id":"current","chunk":"tick\n"}}`
	child := writeUsageRolloutBoundary(t, home, "child", "root", nil, zero)
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("zero ordinal progress=%d, want 1", *progress)
	}
	appendUsageEvents(t, child, missing)
	activity.poll()
	if *progress != 1 {
		t.Fatalf("missing ordinal renewed progress: %d", *progress)
	}
}

func TestRolloutActivityFreshHelperResponseRequiresProvenTurn(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	response := func(ordinal int, id, turn, text string) string {
		return fmt.Sprintf(`{"ordinal":%d,"type":"response_item","payload":{"type":"message","id":%q,"role":"assistant","content":[{"type":"output_text","text":%q}],"internal_chat_message_metadata_passthrough":{"turn_id":%q}}}`, ordinal, id, text, turn)
	}
	child := writeUsageRolloutBoundary(t, home, "child", "root", nil,
		response(8, "copied", "old-turn", "copied history"))
	activity, _, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if *progress != 0 {
		t.Fatalf("copied helper history renewed progress: %d", *progress)
	}
	appendUsageEvents(t, child,
		`{"ordinal":9,"type":"event_msg","payload":{"type":"thread_goal_updated","turn_id":"current"}}`,
		response(10, "unattributed", "current", "output before turn proof"))
	activity.poll()
	if *progress != 0 {
		t.Fatalf("unattributed helper output renewed progress: %d", *progress)
	}
	appendUsageEvents(t, child,
		`{"ordinal":11,"type":"event_msg","payload":{"type":"thread_goal_updated","thread_id":"child","turn_id":"current"}}`,
		response(12, "current", "current", "new helper output"))
	activity.poll()
	if *progress != 1 {
		t.Fatalf("proven helper response did not renew progress: %d", *progress)
	}
	appendUsageEvents(t, child,
		response(13, "copied", "old-turn", "copied history"),
		response(14, "missing-turn", "", "unattributed output"),
		response(15, "current", "current", "new helper output"))
	activity.poll()
	if *progress != 1 {
		t.Fatalf("copied, unbound or replayed helper output renewed progress: %d", *progress)
	}
	appendUsageEvents(t, child, response(16, "next", "current", "further helper output"))
	activity.poll()
	if *progress != 2 {
		t.Fatalf("later proven helper response did not renew progress: %d", *progress)
	}
	appendUsageEvents(t, child,
		usageRecordAt(17, "root", "foreign", "foreign-response", 1, 0, 1),
		response(18, "foreign", "foreign", "foreign-thread output"),
		usageRecordAt(19, "child", "usage-turn", "usage-response", 1, 0, 1))
	activity.poll()
	if *progress != 2 {
		t.Fatalf("usage records or foreign-thread output renewed progress: %d", *progress)
	}
	appendUsageEvents(t, child, response(20, "usage-output", "usage-turn", "output after own-thread usage"))
	activity.poll()
	if *progress != 3 {
		t.Fatalf("own-thread usage did not prove the helper response turn: %d", *progress)
	}
	appendUsageEvents(t, child, `{"ordinal":21,"type":"event_msg","payload":{"type":"exec_command_output","turn_id":"usage-turn","chunk":"more output"}}`)
	activity.poll()
	if *progress != 4 {
		t.Fatalf("proved helper output without thread ID did not renew progress: %d", *progress)
	}
	appendUsageEvents(t, child,
		`{"ordinal":22,"type":"event_msg","payload":{"type":"exec_command_output","turn_id":"usage-turn","chunk":""}}`,
		`{"ordinal":23,"type":"event_msg","payload":{"type":"exec_command_output","thread_id":"child","turn_id":"usage-turn","chunk":""}}`)
	activity.poll()
	if *progress != 4 {
		t.Fatalf("empty helper output renewed progress: %d", *progress)
	}
	appendUsageEvents(t, child, `{"ordinal":24,"type":"event_msg","payload":{"type":"item_completed","turn_id":"usage-turn","item":{"type":"CommandExecution","id":"finished-tool","aggregated_output":"PASS"}}}`)
	activity.poll()
	if *progress != 5 {
		t.Fatalf("proved helper completion without thread ID did not renew progress: %d", *progress)
	}
}

func TestRolloutActivityUnattributedResumeContentDoesNotRenewProgress(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "root-response", 1)...)
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		usageEventAt(9, "task_started", "old-child"),
		rolloutMessage(10, "old-message", "old output"),
		usageEventAt(11, "task_complete", "old-child"))
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	appendUsageEvents(t, child, rolloutMessage(12, "unattributed", "possibly replayed"))
	progress := 0
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil, nil,
		func() { progress++ })
	activity.root("root")
	activity.child("child", false)
	activity.poll()
	if progress != 0 {
		t.Fatalf("unattributed resumed content renewed progress: %d", progress)
	}
}

func TestRolloutActivityResumeResponseOnlyTurnDoesNotRenewProgress(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "root-response", 1)...)
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessageWithTurn(8, "inherited", "copied history", "inherited-turn"),
		rolloutMessageWithTurn(9, "old", "old helper output", "response-only-turn"))
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	appendUsageEvents(t, child, rolloutMessageWithTurn(10, "old", "old helper output", "response-only-turn"))
	progress := 0
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil, nil,
		func() { progress++ })
	activity.root("root")
	activity.child("child", false)
	activity.poll()
	if progress != 0 {
		t.Fatalf("replayed response-only old turn renewed progress: %d", progress)
	}
	if before := session.before["child"]; before == nil || !before.turns["response-only-turn"] || before.turns["inherited-turn"] {
		t.Fatalf("response-only baseline ignored ownership: %+v", before)
	}
	appendUsageEvents(t, child, rolloutMessageWithTurn(11, "new", "new helper output", "new-turn"))
	activity.poll()
	if progress != 1 {
		t.Fatalf("current helper turn did not renew progress: %d", progress)
	}
}
