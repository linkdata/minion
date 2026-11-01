package codexcli

import (
	"testing"
	"time"
)

func TestResumedHelperHistoryBoundaryDoesNotCreditReappendedNoTurnOutput(t *testing.T) {
	for _, tc := range []struct {
		name, old, fresh string
	}{
		{
			name:  "command output",
			old:   `{"type":"event_msg","payload":{"type":"exec_command_output","thread_id":"child","chunk":"old output"}}`,
			fresh: `{"type":"event_msg","payload":{"type":"exec_command_output","thread_id":"child","turn_id":"current","chunk":"new output"}}`,
		},
		{
			name:  "inter-agent message",
			old:   `{"type":"inter_agent_communication","payload":{"author":"child","recipient":"root","content":"old message"}}`,
			fresh: `{"type":"inter_agent_communication","payload":{"author":"child","recipient":"root","content":"new message","internal_chat_message_metadata_passthrough":{"turn_id":"current"}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "root-response", 1)...)
			boundary := int64(9)
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
				usageEventAt(9, "task_started", "old-child"),
				`{"ordinal":10,`+tc.old[1:],
				usageRecordAt(11, "child", "old-child", "child-response", 1, 0, 0),
				usageEventAt(12, "task_complete", "old-child"))
			var session usageSession
			session.begin(t.Context(), home, "root", started)
			if baseline := session.before["child"]; baseline == nil || !baseline.valid || !baseline.prefixKnown {
				t.Fatalf("fixture lacks verified helper baseline: %+v", baseline)
			}
			appendUsageEvents(t, child, `{"ordinal":13,`+tc.old[1:])
			progress := 0
			activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil, nil,
				func() { progress++ })
			activity.root("root")
			activity.child("child", false)
			activity.poll()
			if progress != 0 {
				t.Fatalf("reappended no-turn output renewed progress: %d", progress)
			}
			appendUsageEvents(t, child, `{"ordinal":14,`+tc.fresh[1:])
			activity.poll()
			if progress != 1 {
				t.Fatalf("current-turn output progress=%d, want 1", progress)
			}
		})
	}
}

func TestResumedHelperParentMessageDoesNotRenewProgress(t *testing.T) {
	for _, tc := range []struct {
		name, parent, child string
	}{
		{
			name:   "inter-agent communication",
			parent: `{"type":"inter_agent_communication","payload":{"author":"root","recipient":"child","content":"new instructions","internal_chat_message_metadata_passthrough":{"turn_id":"current-root"}}}`,
			child:  `{"type":"inter_agent_communication","payload":{"author":"child","recipient":"root","content":"new work","internal_chat_message_metadata_passthrough":{"turn_id":"current-child"}}}`,
		},
		{
			name:   "response agent message",
			parent: `{"type":"response_item","payload":{"type":"agent_message","author":"root","recipient":"child","content":[{"type":"input_text","text":"new instructions"}],"internal_chat_message_metadata_passthrough":{"turn_id":"current-root"}}}`,
			child:  `{"type":"response_item","payload":{"type":"agent_message","author":"child","recipient":"root","content":[{"type":"input_text","text":"new work"}],"internal_chat_message_metadata_passthrough":{"turn_id":"current-child"}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "root-response", 1)...)
			boundary := int64(9)
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
				usageEventAt(9, "task_started", "old-child"),
				`{"ordinal":10,"type":"inter_agent_communication","payload":{"author":"root","recipient":"child","content":"old instructions","internal_chat_message_metadata_passthrough":{"turn_id":"foreign-root"}}}`,
				usageRecordAt(11, "child", "old-child", "child-response", 1, 0, 0),
				usageEventAt(12, "task_complete", "old-child"))
			var session usageSession
			session.begin(t.Context(), home, "root", started)
			if baseline := session.before["child"]; baseline == nil || !baseline.valid || !baseline.prefixKnown || !baseline.turns["old-child"] || baseline.turns["foreign-root"] {
				t.Fatalf("fixture lacks owned helper baseline: %+v", baseline)
			}
			appendUsageEvents(t, child, `{"ordinal":13,`+tc.parent[1:])
			progress := 0
			activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil, nil,
				func() { progress++ })
			activity.root("root")
			activity.child("child", false)
			activity.poll()
			if progress != 0 {
				t.Fatalf("parent-authored message renewed helper progress: %d", progress)
			}
			appendUsageEvents(t, child, `{"ordinal":14,`+tc.child[1:])
			activity.poll()
			if progress != 1 {
				t.Fatalf("child-authored message progress=%d, want 1", progress)
			}
		})
	}
}
