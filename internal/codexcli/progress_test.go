package codexcli

import (
	"fmt"
	"testing"
	"time"
)

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
