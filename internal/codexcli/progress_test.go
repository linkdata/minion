package codexcli

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
