package codexcli

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
)

// The rollout persists response_item discussion in both history modes. Its
// event_msg items can repeat that discussion in paginated history.
func rolloutCompletedItem(ordinal int, thread, item string) string {
	return rolloutCompletedItemTurn(ordinal, thread, "current", item)
}

func rolloutCompletedItemTurn(ordinal int, thread, turn, item string) string {
	return fmt.Sprintf(`{"ordinal":%d,"type":"event_msg","payload":{"type":"item_completed","thread_id":%q,"turn_id":%q,"item":%s}}`, ordinal, thread, turn, item)
}

func rolloutMessage(ordinal int, id, text string) string {
	return fmt.Sprintf(`{"ordinal":%d,"type":"response_item","payload":{"type":"message","id":%q,"role":"assistant","content":[{"type":"output_text","text":%q}]}}`, ordinal, id, text)
}

func rolloutMessageWithTurn(ordinal int, id, text, turn string) string {
	return fmt.Sprintf(`{"ordinal":%d,"type":"response_item","payload":{"type":"message","id":%q,"role":"assistant","content":[{"type":"output_text","text":%q}],"internal_chat_message_metadata_passthrough":{"turn_id":%q}}}`, ordinal, id, text, turn)
}

func setActivityHistoryMode(t *testing.T, path, mode string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	withMode := strings.Replace(string(data), `"session_id":"root"`, fmt.Sprintf(`"history_mode":%q,"session_id":"root"`, mode), 1)
	if withMode == string(data) {
		t.Fatal("rollout header had no session_id")
	}
	if err := os.WriteFile(path, []byte(withMode), 0600); err != nil {
		t.Fatal(err)
	}
}

func rolloutSpawn(ordinal int, child string) string {
	return fmt.Sprintf(`{"ordinal":%d,"type":"event_msg","payload":{"type":"sub_agent_activity","event_id":"spawn","kind":"started","agent_thread_id":%q,"agent_path":"/root/helper"}}`, ordinal, child)
}

func rolloutInteracted(ordinal int, child string) string {
	return fmt.Sprintf(`{"ordinal":%d,"type":"event_msg","payload":{"type":"sub_agent_activity","event_id":"observe","kind":"interacted","agent_thread_id":%q,"agent_path":"/root/helper"}}`, ordinal, child)
}

func newActivityTestRecorder(t *testing.T, home string, started time.Time) (*rolloutActivity, *[]aicli.StreamEvent, *int) {
	t.Helper()
	var events []aicli.StreamEvent
	progress := 0
	activity := newRolloutActivity(t.Context(), home, started, nil, nil, nil, nil,
		func(event aicli.StreamEvent) { events = append(events, event) },
		func() { progress++ })
	return activity, &events, &progress
}

func TestRolloutActivityRootAnnouncesQuietHelperDiscussion(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "",
		rolloutMessage(8, "root-message", "root already streamed on stdout"),
		rolloutSpawn(10, "child"))
	start := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &start,
		rolloutMessage(8, "inherited", "inherited parent text"),
		rolloutMessage(9, "message", "helper is working"),
		`{"ordinal":10,"type":"response_item","payload":{"type":"reasoning","id":"summary","summary":[{"type":"summary_text","text":"visible summary"}],"content":[{"type":"reasoning_text","text":"hidden chain"}]}}`,
		`{"ordinal":11,"type":"response_item","payload":{"type":"function_call","id":"call","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}","call_id":"call-1"}}`,
		`{"ordinal":12,"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"PASS"}}`)

	activity, events, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.poll()
	activity.poll() // A scan of unchanged files must not replay content or progress.
	if len(*events) != 4 {
		t.Fatalf("helper discussion events = %+v; want message, summary, tool call and output", *events)
	}
	if (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "helper is working"}) ||
		(*events)[1] != (aicli.StreamEvent{NativeID: "child", Kind: "reasoning_summary", Text: "visible summary"}) {
		t.Fatalf("helper message or summary lost: %+v", *events)
	}
	for _, tool := range (*events)[2:] {
		if tool.NativeID != "child" || tool.Kind != "tool" || !json.Valid([]byte(tool.Text)) {
			t.Fatalf("helper tool item lost: %+v", tool)
		}
	}
	if !strings.Contains((*events)[2].Text, `"name":"exec_command"`) || !strings.Contains((*events)[3].Text, `"output":"PASS"`) {
		t.Fatalf("tool call/output payload lost: %+v", (*events)[2:])
	}
	for _, event := range *events {
		if strings.Contains(event.Text, "hidden chain") || strings.Contains(event.Text, "inherited parent text") ||
			strings.Contains(event.Text, "root already streamed") {
			t.Fatalf("private, inherited, or root text emitted: %+v", event)
		}
	}
	if *progress == 0 {
		t.Fatal("quiet helper entries did not signal progress")
	}
	progressBefore := *progress
	activity.poll()
	if *progress != progressBefore || len(*events) != 4 {
		t.Fatalf("unchanged scan replayed entries: events=%+v progress=%d", *events, *progress)
	}
}

func TestRolloutActivityLegacyEventMsgCapturesHelperContent(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		`{"ordinal":9,"type":"event_msg","payload":{"type":"agent_message","message":"legacy helper reply"}}`,
		`{"ordinal":10,"type":"event_msg","payload":{"type":"agent_reasoning","text":"visible legacy summary"}}`,
		`{"ordinal":11,"type":"event_msg","payload":{"type":"agent_reasoning_raw_content","text":"private reasoning"}}`,
		`{"ordinal":12,"type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"tool-1","turn_id":"current","result":{"Ok":{"content":[{"type":"text","text":"PASS"}]}}}}`)
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	activity.finish()
	if len(*events) != 3 ||
		(*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant_event_msg", Text: "legacy helper reply"}) ||
		(*events)[1] != (aicli.StreamEvent{NativeID: "child", Kind: "reasoning_summary_event_msg", Text: "visible legacy summary"}) ||
		(*events)[2].NativeID != "child" || (*events)[2].Kind != "tool_event_msg" ||
		!json.Valid([]byte((*events)[2].Text)) || !strings.Contains((*events)[2].Text, `"PASS"`) {
		t.Fatalf("legacy helper content missing, duplicated, or leaked raw reasoning: %+v", *events)
	}
}

func TestRolloutActivityLegacyCompletedToolOutputAndUnsupportedUserMessage(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	output := fmt.Sprintf(`{"type":"FunctionCallOutput","id":"output","output":%q}`, strings.Repeat("PASS\n", 100))
	response := `{"type":"function_call_output","call_id":"call-1","output":"response source"}`
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutCompletedItem(9, "child", output),
		fmt.Sprintf(`{"ordinal":10,"type":"response_item","payload":%s}`, response),
		rolloutCompletedItem(11, "child", `{"type":"UserMessage","id":"unsupported","content":[{"type":"Text","text":"not a legacy item"}]}`))
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	activity.finish()
	want := []aicli.StreamEvent{
		{NativeID: "child", Kind: "tool_item_completed", Text: output},
		{NativeID: "child", Kind: "tool", Text: response},
		{NativeID: "child", Kind: "content_missing"},
	}
	if !slices.Equal(*events, want) {
		t.Fatalf("legacy completed tool, response source, or unsupported item = %+v; want %+v", *events, want)
	}
}

func TestRolloutActivityCompletedPlanVisibleInBothHistoryModes(t *testing.T) {
	for _, mode := range []string{"legacy", "paginated"} {
		t.Run(mode, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			boundary := int64(9)
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
				rolloutCompletedItem(9, "child", `{"type":"Plan","id":"plan","text":"Inspect tests, then patch."}`))
			setActivityHistoryMode(t, child, mode)
			activity, events, _ := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			activity.finish()
			want := []aicli.StreamEvent{{NativeID: "child", Kind: "plan", Text: "Inspect tests, then patch."}}
			if !slices.Equal(*events, want) {
				t.Fatalf("%s completed plan = %+v; want %+v", mode, *events, want)
			}
		})
	}
}

func TestRolloutActivityLegacyUserAttachmentsLeaveCoverageGap(t *testing.T) {
	for _, test := range []struct {
		name, payload, text string
	}{
		{"text and image", `{"type":"user_message","message":"review this image","images":["image-ref"]}`, "review this image"},
		{"file without text", `{"type":"user_message","file_ids":["file-1"]}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			boundary := int64(9)
			writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
				fmt.Sprintf(`{"ordinal":9,"type":"event_msg","payload":%s}`, test.payload))
			activity, events, _ := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			activity.finish()
			var textCount, gaps int
			for _, event := range *events {
				switch event.Kind {
				case "user_event_msg":
					if event.NativeID != "child" || event.Text != test.text {
						t.Fatalf("wrong visible user text: %+v", event)
					}
					textCount++
				case "content_missing":
					if event.NativeID != "child" || event.Text != "" {
						t.Fatalf("wrong attachment gap: %+v", event)
					}
					gaps++
				default:
					t.Fatalf("attachment content leaked: %+v", event)
				}
			}
			wantText := 0
			if test.text != "" {
				wantText = 1
			}
			if gaps != 1 || textCount != wantText {
				t.Fatalf("attachment text/gap count = %d/%d; events=%+v", textCount, gaps, *events)
			}
		})
	}
}

func TestRolloutActivityLegacyAgentQuestionsLeaveCoverageGap(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		`{"ordinal":9,"type":"event_msg","payload":{"type":"agent_message","message":"Please clarify the scope.","questions":[{"question":"Which target?"}]}}`)
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	activity.finish()
	var textCount, gaps int
	for _, event := range *events {
		switch event {
		case aicli.StreamEvent{NativeID: "child", Kind: "assistant_event_msg", Text: "Please clarify the scope."}:
			textCount++
		case aicli.StreamEvent{NativeID: "child", Kind: "content_missing"}:
			gaps++
		default:
			t.Fatalf("unexpected agent question event: %+v", event)
		}
	}
	if textCount != 1 || gaps != 1 {
		t.Fatalf("agent question text/gap count = %d/%d; events=%+v", textCount, gaps, *events)
	}
}

func TestRolloutActivityPaginatedRetainsBothPersistedSources(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutCompletedItem(9, "child", `{"type":"AgentMessage","id":"message","content":[{"type":"Text","text":"paginated reply"}]}`),
		rolloutMessage(10, "message", "paginated reply"),
		rolloutCompletedItem(11, "child", `{"type":"Reasoning","id":"summary","summary_text":["visible summary"],"raw_content":["hidden reasoning"]}`),
		`{"ordinal":12,"type":"response_item","payload":{"type":"reasoning","id":"summary","summary":[{"type":"summary_text","text":"visible summary"}]}}`,
		rolloutCompletedItem(13, "child", `{"type":"CommandExecution","id":"tool","command":["go","test"],"status":"completed","aggregated_output":"PASS"}`),
		`{"ordinal":14,"type":"response_item","payload":{"type":"function_call","id":"tool","name":"exec_command","arguments":"{}","call_id":"call-1"}}`)
	setActivityHistoryMode(t, child, "paginated")
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	activity.finish()
	if len(*events) != 6 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "paginated reply"}) ||
		(*events)[1] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant_response_item", Text: "paginated reply"}) ||
		(*events)[2] != (aicli.StreamEvent{NativeID: "child", Kind: "reasoning_summary", Text: "visible summary"}) ||
		(*events)[3] != (aicli.StreamEvent{NativeID: "child", Kind: "reasoning_summary_response_item", Text: "visible summary"}) ||
		(*events)[4].Kind != "tool" || !json.Valid([]byte((*events)[4].Text)) ||
		!strings.Contains((*events)[4].Text, `"aggregated_output":"PASS"`) ||
		(*events)[5].Kind != "tool_response_item" || !json.Valid([]byte((*events)[5].Text)) ||
		!strings.Contains((*events)[5].Text, `"call_id":"call-1"`) {
		t.Fatalf("paginated persisted sources = %+v", *events)
	}
	for _, event := range *events {
		if strings.Contains(event.Text, "hidden reasoning") {
			t.Fatalf("raw reasoning leaked: %+v", event)
		}
	}
}

func TestRolloutActivityPaginatedRetainsUnpairedSource(t *testing.T) {
	for _, test := range []struct {
		name, record, kind, text string
	}{
		{"response only", rolloutMessage(9, "unpaired", "only in response"), "assistant_response_item", "only in response"},
		{"completed only", rolloutCompletedItem(9, "child", `{"type":"AgentMessage","id":"unpaired","content":[{"type":"Text","text":"only in completed"}]}`), "assistant", "only in completed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			boundary := int64(9)
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, test.record)
			setActivityHistoryMode(t, child, "paginated")
			activity, events, _ := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			activity.finish()
			if len(*events) != 1 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: test.kind, Text: test.text}) {
				t.Fatalf("unpaired persisted content = %+v", *events)
			}
		})
	}
}

func TestRolloutActivityPaginatedContextCompactionIsMetadata(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutCompletedItem(9, "child", `{"type":"ContextCompaction","id":"compaction"}`),
		rolloutMessage(10, "after-compaction", "helper resumed discussion"))
	setActivityHistoryMode(t, child, "paginated")
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	activity.finish()
	want := []aicli.StreamEvent{{NativeID: "child", Kind: "assistant_response_item", Text: "helper resumed discussion"}}
	if !slices.Equal(*events, want) {
		t.Fatalf("compaction metadata caused a gap or hid discussion: got %+v; want %+v", *events, want)
	}
}

func TestRolloutActivityPaginatedHelperUserPromptSources(t *testing.T) {
	completed := rolloutCompletedItem(9, "child", `{"type":"UserMessage","id":"prompt","content":[{"type":"text","text":"helper task"}]}`)
	response := `{"ordinal":10,"type":"response_item","payload":{"type":"message","id":"prompt","role":"user","content":[{"type":"input_text","text":"helper task"}]}}`
	for _, test := range []struct {
		name    string
		records []string
		want    []aicli.StreamEvent
	}{
		{"both sources", []string{completed, response}, []aicli.StreamEvent{
			{NativeID: "child", Kind: "user", Text: "helper task"},
			{NativeID: "child", Kind: "user_response_item", Text: "helper task"},
		}},
		{"completed only", []string{completed}, []aicli.StreamEvent{
			{NativeID: "child", Kind: "user", Text: "helper task"},
		}},
		{"response only", []string{response}, []aicli.StreamEvent{
			{NativeID: "child", Kind: "user_response_item", Text: "helper task"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			boundary := int64(9)
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, test.records...)
			setActivityHistoryMode(t, child, "paginated")
			activity, events, _ := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			activity.finish()
			if !slices.Equal(*events, test.want) {
				t.Fatalf("paginated helper prompt = %+v; want %+v", *events, test.want)
			}
		})
	}
}

func TestRolloutActivityPaginatedCompletedMessageTextWithoutContent(t *testing.T) {
	for _, test := range []struct {
		name, itemType, content, kind string
	}{
		{"agent missing content", "AgentMessage", "", "assistant"},
		{"agent empty content", "AgentMessage", `,"content":[]`, "assistant"},
		{"user missing content", "UserMessage", "", "user"},
		{"user empty content", "UserMessage", `,"content":[]`, "user"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			boundary := int64(9)
			item := fmt.Sprintf(`{"type":%q,"id":"message","text":"visible text"%s}`, test.itemType, test.content)
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
				rolloutCompletedItem(9, "child", item))
			setActivityHistoryMode(t, child, "paginated")
			activity, events, _ := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			activity.finish()
			want := []aicli.StreamEvent{{NativeID: "child", Kind: test.kind, Text: "visible text"}}
			if !slices.Equal(*events, want) {
				t.Fatalf("completed %s text = %+v; want %+v", test.itemType, *events, want)
			}
		})
	}
}

func TestRolloutActivityPaginatedHelperUserPromptResumeSkipsOldTurn(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "old-root-response", 1)...)
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		completedUsageTurn("child", "old-child", "old-child-response", 1)...)
	setActivityHistoryMode(t, child, "paginated")
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	appendUsageEvents(t, child,
		rolloutCompletedItemTurn(20, "child", "old-child", `{"type":"UserMessage","id":"late-old","content":[{"type":"text","text":"old helper task"}]}`),
		`{"ordinal":21,"type":"response_item","payload":{"type":"message","id":"late-old","role":"user","content":[{"type":"input_text","text":"old helper task"}],"internal_chat_message_metadata_passthrough":{"turn_id":"old-child"}}}`,
		rolloutCompletedItemTurn(22, "child", "new-child", `{"type":"UserMessage","id":"new-prompt","content":[{"type":"text","text":"current helper task"}]}`),
		`{"ordinal":23,"type":"response_item","payload":{"type":"message","id":"new-prompt","role":"user","content":[{"type":"input_text","text":"current helper task"}],"internal_chat_message_metadata_passthrough":{"turn_id":"new-child"}}}`)
	var events []aicli.StreamEvent
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) }, func() {})
	activity.root("root")
	activity.child("child", false)
	activity.poll()
	activity.finish()
	want := []aicli.StreamEvent{
		{NativeID: "child", Kind: "user", Text: "current helper task"},
		{NativeID: "child", Kind: "user_response_item", Text: "current helper task"},
	}
	if !slices.Equal(events, want) {
		t.Fatalf("resumed helper prompts = %+v; want %+v", events, want)
	}
}

func TestRolloutActivityPaginatedAgentMessage(t *testing.T) {
	for _, test := range []struct {
		name, content string
		want          aicli.StreamEvent
	}{
		{"plaintext", `{"type":"input_text","text":"helper progress"}`,
			aicli.StreamEvent{NativeID: "child", Kind: "inter_agent_message_response_item", Text: "child -> root\nhelper progress"}},
		{"encrypted", `{"type":"encrypted_content","encrypted_content":"opaque-ciphertext"}`,
			aicli.StreamEvent{NativeID: "child", Kind: "content_missing"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			boundary := int64(9)
			line := fmt.Sprintf(`{"ordinal":9,"type":"response_item","payload":{"type":"agent_message","author":"child","recipient":"root","content":[%s]}}`, test.content)
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary, line)
			setActivityHistoryMode(t, child, "paginated")
			activity, events, _ := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			activity.finish()
			if len(*events) != 1 {
				t.Fatalf("%s agent message produced %d events; want one", test.name, len(*events))
			}
			if got := (*events)[0]; got != test.want {
				if test.name == "encrypted" {
					t.Fatalf("encrypted agent message yielded id=%q kind=%q text_bytes=%d; want one child gap", got.NativeID, got.Kind, len(got.Text))
				}
				t.Fatalf("plaintext agent message = %+v; want %+v", got, test.want)
			}
		})
	}
}

func TestRolloutActivityInterAgentCommunication(t *testing.T) {
	for _, mode := range []string{"legacy", "paginated"} {
		t.Run(mode, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			boundary := int64(9)
			child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
				`{"ordinal":9,"type":"inter_agent_communication_metadata","payload":{"author":"child","recipient":"root","trigger_turn":true}}`,
				`{"ordinal":10,"type":"inter_agent_communication","payload":{"author":"child","recipient":"root","content":"visible update","trigger_turn":true}}`,
				`{"ordinal":11,"type":"inter_agent_communication","payload":{"author":"child","recipient":"root","encrypted_content":"opaque-ciphertext","trigger_turn":true}}`)
			if mode == "paginated" {
				setActivityHistoryMode(t, child, mode)
			}
			activity, events, _ := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			activity.finish()
			if len(*events) != 2 {
				for i, event := range *events {
					t.Logf("event %d: id=%q kind=%q text_bytes=%d", i, event.NativeID, event.Kind, len(event.Text))
				}
				t.Fatalf("%s communication produced %d events; want plaintext and one gap", mode, len(*events))
			}
			if got := (*events)[0]; got != (aicli.StreamEvent{NativeID: "child", Kind: "inter_agent_communication", Text: "child -> root\nvisible update"}) {
				t.Fatalf("%s plaintext communication yielded id=%q kind=%q text_bytes=%d", mode, got.NativeID, got.Kind, len(got.Text))
			}
			if got := (*events)[1]; got != (aicli.StreamEvent{NativeID: "child", Kind: "content_missing"}) {
				t.Fatalf("%s encrypted communication yielded id=%q kind=%q text_bytes=%d", mode, got.NativeID, got.Kind, len(got.Text))
			}
		})
	}
}

func TestRolloutActivityUnknownHistoryModeKeepsVisibleResponseWithGap(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "message", "available helper reply"))
	setActivityHistoryMode(t, child, "future_mode")
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	activity.finish()
	var replies, gaps int
	for _, event := range *events {
		switch event {
		case aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "available helper reply"}:
			replies++
		case aicli.StreamEvent{NativeID: "child", Kind: "content_missing"}:
			gaps++
		default:
			t.Fatalf("unexpected unknown-mode event: %+v", event)
		}
	}
	if replies != 1 || gaps != 1 {
		t.Fatalf("unknown mode should keep available content and one gap: %+v", *events)
	}
}

func TestRolloutActivityNestedSpawnRequiresParentAnnouncement(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutSpawn(9, "grandchild"),
		rolloutMessage(10, "child-message", "child discussion"))
	writeUsageRolloutBoundary(t, home, "grandchild", "child", &boundary,
		rolloutMessage(9, "grandchild-message", "grandchild discussion"))

	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.poll() // Metadata links alone must not admit either helper.
	if len(*events) != 0 {
		t.Fatalf("metadata-only helper emitted: %+v", *events)
	}
	activity.child("child", true) // A current stdout spawn proves the direct child.
	activity.poll()
	activity.poll() // Allow a breadth-first pass if discovery happened mid-poll.
	got := map[string]string{}
	for _, event := range *events {
		if event.Kind != "assistant" {
			t.Fatalf("unexpected nested event: %+v", event)
		}
		got[event.NativeID] = event.Text
	}
	if len(*events) != 2 || got["child"] != "child discussion" || got["grandchild"] != "grandchild discussion" {
		t.Fatalf("nested helper discussion = %+v", *events)
	}
}

func TestRolloutActivityFreshModernHelperWithoutHistoryStartAnnouncesGrandchild(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	writeUsageRolloutBoundary(t, home, "child", "root", nil,
		nativeActivityEvent(9, "child", "current-child", "started", "grandchild"))
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "grandchild", "child", &boundary,
		rolloutMessage(9, "message", "grandchild discussion"))

	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	activity.finish()
	want := []aicli.StreamEvent{{NativeID: "grandchild", Kind: "assistant", Text: "grandchild discussion"}}
	if !slices.Equal(*events, want) {
		t.Fatalf("modern helper spawn without history start = %+v; want %+v", *events, want)
	}
}

func TestRolloutActivityMetadataAndObservationDoNotAdmitNewHelper(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "message", "private concurrent work"))

	activity, events, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.poll()
	baselineProgress := *progress
	activity.child("child", false) // `wait`/`close_agent` can observe unrelated helpers.
	activity.poll()
	if len(*events) != 0 || *progress != baselineProgress {
		t.Fatalf("metadata/observation admitted helper: events=%+v progress=%d", *events, *progress)
	}
	activity.child("child", true)
	activity.poll()
	if len(*events) != 1 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "private concurrent work"}) || *progress == 0 {
		t.Fatalf("proved spawn did not admit helper: events=%+v progress=%d", *events, *progress)
	}
}

func TestRolloutActivityLegacyInteractedDoesNotAdmitFreshHelper(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", rolloutInteracted(9, "child"))
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "message", "unannounced helper text"))
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.poll()
	activity.finish()
	for _, event := range *events {
		if event.NativeID == "child" {
			t.Fatalf("legacy interaction admitted a fresh helper: %+v", *events)
		}
	}
}

func TestRolloutActivityNativeCompletionDoesNotAdmitFreshHelper(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	root := writeUsageRollout(t, home, "root", "",
		usageEvent("task_started", "root-turn"),
		nativeActivityEvent(9, "root", "root-turn", "completed", "child"))
	setActivityHistoryMode(t, root, "paginated")
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "message", "unproved helper text"))
	setActivityHistoryMode(t, child, "paginated")
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.poll()
	activity.finish()
	want := []aicli.StreamEvent{{NativeID: "root", Kind: "content_missing"}}
	if !slices.Equal(*events, want) {
		t.Fatalf("completed-only helper should gap parent without admitting helper: got %+v; want %+v", *events, want)
	}
}

func TestRolloutActivityKnownHelperObservationRequiresPriorProof(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "root-response", 1)...)
	boundary := int64(9)
	unproved := writeUsageRolloutBoundary(t, home, "unproved", "root", &boundary,
		completedUsageTurn("unproved", "old-unproved", "unproved-response", 1)...)
	proved := writeUsageRolloutBoundary(t, home, "proved", "root", &boundary,
		completedUsageTurn("proved", "old-proved", "proved-response", 1)...)
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	if session.known["unproved"].path != unproved || session.known["proved"].path != proved {
		t.Fatalf("fixture did not retain both metadata-linked helpers: %+v", session.known)
	}
	appendUsageEvents(t, unproved, rolloutMessageWithTurn(20, "unproved-new", "private unproved suffix", "new-unproved"))
	appendUsageEvents(t, proved, rolloutMessageWithTurn(20, "proved-new", "proved suffix", "new-proved"))

	var events []aicli.StreamEvent
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"proved": true}, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) }, func() {})
	activity.root("root")
	activity.child("unproved", false)
	activity.child("proved", false)
	activity.poll()
	activity.finish()
	want := []aicli.StreamEvent{
		{NativeID: "unproved", Kind: "content_missing"},
		{NativeID: "proved", Kind: "assistant", Text: "proved suffix"},
	}
	if !slices.Equal(events, want) {
		t.Fatalf("known helper proof boundary = %+v; want %+v", events, want)
	}
}

func TestRolloutActivityNativeCompletionReactivatesKnownHelper(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "old-root-response", 1)...)
	setActivityHistoryMode(t, root, "paginated")
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		completedUsageTurn("child", "old-child", "old-child-response", 1)...)
	setActivityHistoryMode(t, child, "paginated")
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	appendUsageEvents(t, root, nativeActivityEvent(20, "root", "new-root", "completed", "child"))
	appendUsageEvents(t, child, rolloutMessageWithTurn(20, "message", "current helper text", "new-child"))
	var events []aicli.StreamEvent
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) }, func() {})
	activity.root("root")
	activity.poll()
	activity.finish()
	want := []aicli.StreamEvent{{NativeID: "child", Kind: "assistant_response_item", Text: "current helper text"}}
	if !slices.Equal(events, want) {
		t.Fatalf("known helper completion failed to capture suffix: got %+v; want %+v", events, want)
	}
}

func TestRolloutActivityStdoutSpawnWorksBeforeRootRolloutAppears(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "message", "active without root file"))
	activity, events, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")         // Native stdout identified the root.
	activity.child("child", true) // Native stdout completed a current spawn.
	activity.poll()
	if len(*events) != 1 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "active without root file"}) || *progress == 0 {
		t.Fatalf("stdout-announced helper depended on root rollout file: events=%+v progress=%d", *events, *progress)
	}
}

func TestRolloutActivityOversizedCurrentSpawnGapsParent(t *testing.T) {
	tooLong := strings.Repeat("x", maxItemIDBytes+1)
	for _, source := range []string{"stdout", "root rollout", "child rollout"} {
		t.Run(source, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			parent := "root"
			switch source {
			case "root rollout":
				writeUsageRollout(t, home, "root", "", rolloutSpawn(9, tooLong))
			case "child rollout":
				parent = "child"
				boundary := int64(9)
				writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
					rolloutSpawn(9, tooLong))
			}
			activity, events, _ := newActivityTestRecorder(t, home, started)
			activity.root("root")
			if source == "stdout" {
				activity.child(tooLong, true)
			} else if source == "child rollout" {
				activity.child("child", true)
			}
			activity.poll()
			activity.finish()
			if len(*events) != 1 || (*events)[0] != (aicli.StreamEvent{NativeID: parent, Kind: "content_missing"}) {
				t.Fatalf("oversized %s spawn should gap its parent once: %+v", source, *events)
			}
		})
	}
}

func TestRolloutActivityMissingAnnouncedHelperGapsOnlyAtFinish(t *testing.T) {
	for _, announced := range []bool{false, true} {
		t.Run(fmt.Sprint(announced), func(t *testing.T) {
			activity, events, _ := newActivityTestRecorder(t, t.TempDir(), time.Now())
			activity.root("root")
			if announced {
				activity.child("child", true)
			}
			activity.poll()
			if len(*events) != 0 {
				t.Fatalf("missing helper gapped before final discovery: %+v", *events)
			}
			activity.finish()
			if !announced && len(*events) != 0 {
				t.Fatalf("no helper and no rollout should not gap: %+v", *events)
			}
			if announced && (len(*events) != 1 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "content_missing"})) {
				t.Fatalf("missing current helper should gap once at finish: %+v", *events)
			}
		})
	}
}

func TestRolloutActivityAmbiguousHelperWithholdsContentAtFinish(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	path := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "message", "ambiguous helper content"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".jsonl", data, 0600); err != nil {
		t.Fatal(err)
	}
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	for _, event := range *events {
		if event.Kind != "content_missing" || event.Text != "" {
			t.Fatalf("ambiguous helper content emitted before final scan: %+v", *events)
		}
	}
	activity.finish()
	var childGaps, rootGaps int
	for _, event := range *events {
		if event.Kind != "content_missing" || event.Text != "" {
			t.Fatalf("ambiguous helper content emitted: %+v", *events)
		}
		switch event.NativeID {
		case "child":
			childGaps++
		case "root":
			rootGaps++
		default:
			t.Fatalf("unrelated gap: %+v", event)
		}
	}
	if childGaps != 1 || rootGaps > 1 {
		t.Fatalf("ambiguous helper should have one child gap: %+v", *events)
	}
}

func TestRolloutActivityDiscoveryGapsOnlyRelevantIncompleteTrees(t *testing.T) {
	for _, scenario := range []string{"no sessions", "incomplete recent tree", "ambiguous root"} {
		t.Run(scenario, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			switch scenario {
			case "incomplete recent tree":
				root := writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
				if err := os.WriteFile(root+".bad.jsonl", []byte("{\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "ambiguous root":
				root := writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
				data, err := os.ReadFile(root)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(root+".duplicate.jsonl", data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			activity, events, _ := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.poll()
			activity.finish()
			if scenario == "no sessions" {
				if len(*events) != 0 {
					t.Fatalf("missing sessions tree invented gap: %+v", *events)
				}
			} else if len(*events) != 1 || (*events)[0] != (aicli.StreamEvent{NativeID: "root", Kind: "content_missing"}) {
				t.Fatalf("incomplete root discovery lacked one root gap: %+v", *events)
			}
		})
	}
}

func TestRolloutActivityCachesDiscoveryAfterExpectedPathsAppear(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	root := writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "message", "captured helper"))
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if len(*events) != 1 || (*events)[0].Text != "captured helper" {
		t.Fatalf("setup did not find expected paths: %+v", *events)
	}
	if err := os.WriteFile(root+".late.jsonl", []byte("{\n"), 0600); err != nil {
		t.Fatal(err)
	}
	activity.lastDiscover = time.Now().Add(-time.Hour) // A normal 5s retry is now due.
	activity.poll()
	if len(*events) != 1 {
		t.Fatalf("complete discovery kept walking unrelated recent files: %+v", *events)
	}
}

func TestRolloutActivityNewAnnouncementRefreshesCachedDiscovery(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.poll() // Cache a complete tree before the helper exists.
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "message", "newly announced helper"))
	activity.child("child", true)
	activity.poll()
	if len(*events) != 1 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "newly announced helper"}) {
		t.Fatalf("new announcement did not refresh discovery: %+v", *events)
	}
}

func TestRolloutActivityRetriesTransientPartialHelperHeaderLive(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "message", "helper appeared live"))
	complete, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	headerEnd := strings.IndexByte(string(complete), '\n')
	if headerEnd < 2 {
		t.Fatal("fixture has no session_meta header")
	}
	if err := os.WriteFile(child, complete[:headerEnd/2], 0600); err != nil {
		t.Fatal(err)
	}
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	for range 3 {
		activity.lastDiscover = time.Now().Add(-time.Hour) // Make each incomplete retry due without sleeping.
		activity.poll()
	}
	for _, event := range *events {
		if event.NativeID == "child" && event.Kind == "assistant" {
			t.Fatalf("incomplete header emitted helper content: %+v", *events)
		}
	}
	if err := os.WriteFile(child, complete, 0600); err != nil {
		t.Fatal(err)
	}
	activity.lastDiscover = time.Now().Add(-time.Hour) // Make the bounded retry due now.
	activity.poll()
	var live int
	for _, event := range *events {
		if event == (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "helper appeared live"}) {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("completed helper header was not captured before finish: %+v", *events)
	}
	activity.poll()
	activity.finish()
	live = 0
	for _, event := range *events {
		if event == (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "helper appeared live"}) {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("live/final scans duplicated or lost helper content: %+v", *events)
	}
}

func TestRolloutActivityRejectsHeaderReplacedAfterDiscovery(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "original", "original helper text"))
	files, ambiguous, complete := usageFiles(t.Context(), home, "root", started, nil)
	if !complete || len(ambiguous) != 0 || files["child"].path != child {
		t.Fatalf("fixture discovery failed: files=%+v ambiguous=%+v complete=%t", files, ambiguous, complete)
	}
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.files, activity.ambiguous = files, ambiguous
	activity.lastDiscover = time.Now()
	activity.refresh = false // Hold the discovered header while the file changes before scan.
	foreign := writeUsageRolloutBoundary(t, home, "other", "root", &boundary,
		rolloutMessage(9, "foreign", "private foreign helper text"))
	if err := os.Rename(foreign, child); err != nil {
		t.Fatal(err)
	}
	activity.poll()
	want := []aicli.StreamEvent{{NativeID: "child", Kind: "content_missing"}}
	if !slices.Equal(*events, want) {
		t.Fatalf("replaced discovered header admitted foreign helper text: got %+v; want %+v", *events, want)
	}
}

func TestRolloutActivityRejectsSameInodeHeaderRewriteBeforeSuffix(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "first", "first helper text"))
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	first := aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "first helper text"}
	if !slices.Equal(*events, []aicli.StreamEvent{first}) {
		t.Fatalf("fixture did not establish helper cursor: %+v", *events)
	}
	before, err := os.Stat(child)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := strings.Replace(string(data), `"id":"child"`, `"id":"other"`, 1)
	if rewritten == string(data) {
		t.Fatal("fixture did not rewrite helper identity")
	}
	if err := os.WriteFile(child, []byte(rewritten), 0600); err != nil {
		t.Fatal(err)
	}
	appendUsageEvents(t, child, rolloutMessage(10, "foreign", "private foreign suffix"))
	after, err := os.Stat(child)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("fixture replaced inode instead of rewriting it")
	}
	activity.lastDiscover = time.Now() // Exercise the existing cursor before periodic discovery.
	activity.poll()
	want := []aicli.StreamEvent{first, {NativeID: "child", Kind: "content_missing"}}
	if !slices.Equal(*events, want) {
		t.Fatalf("same-inode header rewrite admitted foreign suffix: got %+v; want %+v", *events, want)
	}
}

func TestRolloutActivitySecondHelperHeaderWithHistoryStartStopsForeignSuffix(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "first", "owned helper text"))
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	first := aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "owned helper text"}
	if !slices.Equal(*events, []aicli.StreamEvent{first}) {
		t.Fatalf("fixture did not capture first helper entry: %+v", *events)
	}
	appendUsageEvents(t, child,
		`{"type":"session_meta","payload":{"id":"foreign","session_id":"root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"root"}}},"subagent_history_start_ordinal":9}}`,
		rolloutMessage(10, "foreign", "private foreign suffix"))
	activity.poll()
	activity.finish()
	want := []aicli.StreamEvent{first, {NativeID: "child", Kind: "content_missing"}}
	if !slices.Equal(*events, want) {
		t.Fatalf("second helper header admitted foreign suffix: got %+v; want %+v", *events, want)
	}
}

func TestRolloutActivityConcurrentAnnouncementAndPoll(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "message", "one helper entry"))
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			activity.poll()
		}
	}()
	for range 100 {
		activity.child("child", true)
	}
	<-done
	activity.poll()
	if len(*events) != 1 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "one helper entry"}) {
		t.Fatalf("concurrent announcement replayed or lost entry: %+v", *events)
	}
}

func TestRolloutActivityUnknownHistoryBoundaryLabelsPossibleDuplicate(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	writeUsageRolloutBoundary(t, home, "child", "root", nil,
		rolloutMessage(8, "uncertain-message", "attribution uncertain"))

	activity, events, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	activity.poll()
	var labeled, missing int
	for _, event := range *events {
		if event.NativeID != "child" {
			t.Fatalf("wrong native identity: %+v", event)
		}
		switch event.Kind {
		case "assistant_possibly_duplicate":
			if event.Text != "attribution uncertain" {
				t.Fatalf("possibly duplicate message text = %q", event.Text)
			}
			labeled++
		case "content_missing":
			missing++
		default:
			t.Fatalf("unexpected event: %+v", event)
		}
	}
	if labeled != 1 || missing != 1 || *progress == 0 {
		t.Fatalf("unknown boundary events=%+v progress=%d; want labeled content and one coverage gap", *events, *progress)
	}
}

func TestRolloutActivityPositiveBoundaryTreatsMissingOrdinalsAsUncertain(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(0, "zero", "zero ordinal"),
		`{"type":"response_item","payload":{"type":"message","id":"missing","role":"assistant","content":[{"type":"output_text","text":"missing ordinal"}]}}`)
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	var labeled, gaps int
	for _, event := range *events {
		switch event.Kind {
		case "assistant_possibly_duplicate":
			if event.Text != "zero ordinal" && event.Text != "missing ordinal" {
				t.Fatalf("unexpected uncertain content: %+v", event)
			}
			labeled++
		case "content_missing":
			gaps++
		default:
			t.Fatalf("unknown ordinal silently trusted: %+v", event)
		}
	}
	if labeled != 2 || gaps != 1 {
		t.Fatalf("zero/missing ordinal = %+v; want two labels and one gap", *events)
	}
}

func TestRolloutActivityFinalScanCompletesPartialLineOnce(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	path := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		usageEventAt(8, "task_started", "inherited-turn"))
	line := rolloutMessage(9, "message", "arrived after last live scan")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(line[:len(line)/2])
	if err := joinClose(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	activity, events, progress := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if len(*events) != 0 {
		t.Fatalf("partial JSONL line emitted: events=%+v progress=%d", *events, *progress)
	}
	scanned := activity.scanned
	for range 3 {
		activity.poll()
	}
	if activity.scanned != scanned {
		t.Fatalf("unchanged partial line spent scan budget: first=%d now=%d", scanned, activity.scanned)
	}
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr = f.WriteString(line[len(line)/2:] + "\n")
	if err := joinClose(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	activity.finish()
	if len(*events) != 1 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "arrived after last live scan"}) || *progress == 0 {
		t.Fatalf("final scan missed completed line: events=%+v progress=%d", *events, *progress)
	}
}

func joinClose(writeErr, closeErr error) error {
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func TestRolloutActivityResumeSkipsVerifiedPrefix(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "old-root-response", 1)...)
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		usageEventAt(9, "task_started", "old-child"),
		rolloutMessage(10, "old-message", "historical helper text"),
		usageRecordAt(11, "child", "old-child", "old-child-response", 1, 0, 0),
		usageEventAt(12, "task_complete", "old-child"))
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	appendUsageEvents(t, child, rolloutMessageWithTurn(13, "new-message", "current helper text", "new-child"))

	var events []aicli.StreamEvent
	progress := 0
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) },
		func() { progress++ })
	activity.root("root")
	activity.child("child", false) // A known resumed helper needs only a wait/close observation.
	activity.poll()
	if len(events) != 1 || events[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "current helper text"}) || progress == 0 {
		t.Fatalf("resume replayed baseline or missed suffix: events=%+v progress=%d", events, progress)
	}
}

func TestRolloutActivityLegacyInteractedReactivatesKnownHelper(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "old-root-response", 1)...)
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		usageEventAt(9, "task_started", "old-child"),
		rolloutMessage(10, "old-message", "historical helper text"),
		usageRecordAt(11, "child", "old-child", "old-child-response", 1, 0, 0),
		usageEventAt(12, "task_complete", "old-child"))
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	appendUsageEvents(t, root, rolloutInteracted(21, "child"))
	appendUsageEvents(t, child, rolloutMessageWithTurn(13, "new-message", "current helper text", "new-child"))

	var events []aicli.StreamEvent
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) }, func() {})
	activity.root("root")
	activity.poll()
	activity.finish()
	var childMessages, rootGaps int
	for _, event := range events {
		switch event {
		case aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "current helper text"}:
			childMessages++
		case aicli.StreamEvent{NativeID: "root", Kind: "content_missing"}:
			rootGaps++ // The persisted observation has no turn ID.
		default:
			t.Fatalf("legacy interaction replayed history or emitted unexpected content: %+v", events)
		}
	}
	if childMessages != 1 || rootGaps > 1 {
		t.Fatalf("known helper suffix was not captured once: %+v", events)
	}
}

func TestRolloutActivityResumeRejectsChangedPrefix(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "old-root-response", 1)...)
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		usageEventAt(9, "task_started", "old-child"),
		rolloutMessage(10, "old-message", "old text"),
		usageRecordAt(11, "child", "old-child", "old-child-response", 1, 0, 0),
		usageEventAt(12, "task_complete", "old-child"))
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	data, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), data...)
	data = []byte(strings.Replace(string(data), "old text", "tampered", 1)) // Same length keeps the byte offset plausible.
	if err := os.WriteFile(child, data, 0600); err != nil {
		t.Fatal(err)
	}
	appendUsageEvents(t, child, rolloutMessage(13, "new-message", "should not be trusted"))
	var events []aicli.StreamEvent
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) }, func() {})
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if err := os.WriteFile(child, original, 0600); err != nil {
		t.Fatal(err)
	}
	appendUsageEvents(t, child, rolloutMessage(14, "retry-message", "restored suffix must stay blocked"))
	activity.poll()
	if len(events) != 1 || events[0] != (aicli.StreamEvent{NativeID: "child", Kind: "content_missing"}) {
		t.Fatalf("changed historical prefix should gap once without admitting either suffix: %+v", events)
	}
}

func TestRolloutActivityResumeWithoutForkBoundaryUsesVerifiedPrefix(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "old-root-response", 1)...)
	child := writeUsageRolloutBoundary(t, home, "child", "root", nil,
		usageEventAt(9, "task_started", "old-child"),
		rolloutMessage(10, "old-message", "historical helper text"),
		usageRecordAt(11, "child", "old-child", "old-child-response", 1, 0, 0),
		usageEventAt(12, "task_complete", "old-child"))
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	appendUsageEvents(t, child, rolloutMessageWithTurn(13, "new-message", "verified suffix", "new-child"))

	var events []aicli.StreamEvent
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) }, func() {})
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if len(events) != 1 || events[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "verified suffix"}) {
		t.Fatalf("verified resumed suffix mislabeled or replayed: %+v", events)
	}
}

func TestRolloutActivityResumeWithoutForkBoundarySecondHeaderStopsSuffix(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "old-root-response", 1)...)
	child := writeUsageRolloutBoundary(t, home, "child", "root", nil,
		usageEventAt(9, "task_started", "old-child"),
		rolloutMessageWithTurn(10, "old-message", "historical helper text", "old-child"),
		usageRecordAt(11, "child", "old-child", "old-child-response", 1, 0, 0),
		usageEventAt(12, "task_complete", "old-child"))
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	appendUsageEvents(t, child,
		`{"type":"session_meta","payload":{"id":"child","session_id":"root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"root"}}}}}`,
		rolloutMessageWithTurn(13, "new-message", "unknown turn after second header", "new-child"))
	var events []aicli.StreamEvent
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) }, func() {})
	activity.root("root")
	activity.child("child", false)
	activity.poll()
	activity.finish()
	want := []aicli.StreamEvent{{NativeID: "child", Kind: "content_missing"}}
	if !slices.Equal(events, want) {
		t.Fatalf("second header after verified prefix = %+v; want %+v", events, want)
	}
}

func TestRolloutActivityResumeUsesResponseTurnMetadata(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "old-root-response", 1)...)
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		usageEventAt(9, "task_started", "old-child"),
		rolloutMessageWithTurn(10, "old-message", "historical text", "old-child"),
		usageRecordAt(11, "child", "old-child", "old-child-response", 1, 0, 0),
		usageEventAt(12, "task_complete", "old-child"))
	var session usageSession
	session.begin(t.Context(), home, "root", started)
	appendUsageEvents(t, child,
		rolloutMessageWithTurn(13, "late-old", "late old turn", "old-child"),
		rolloutMessage(14, "missing-turn", "uncertain turn"),
		rolloutMessageWithTurn(15, "new-message", "current turn", "new-child"))
	var events []aicli.StreamEvent
	activity := newRolloutActivity(t.Context(), home, started, session.known, map[string]bool{"child": true}, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) }, func() {})
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	var uncertain, current, gaps int
	for _, event := range events {
		if strings.Contains(event.Text, "late old turn") || strings.Contains(event.Text, "historical text") {
			t.Fatalf("old turn was replayed: %+v", events)
		}
		switch event.Kind {
		case "assistant_possibly_duplicate":
			if event.Text != "uncertain turn" {
				t.Fatalf("wrong uncertain turn: %+v", event)
			}
			uncertain++
		case "assistant":
			if event.Text != "current turn" {
				t.Fatalf("wrong current turn: %+v", event)
			}
			current++
		case "content_missing":
			gaps++
		default:
			t.Fatalf("unexpected resumed event: %+v", event)
		}
	}
	if uncertain != 1 || current != 1 || gaps != 1 {
		t.Fatalf("resumed turn attribution = %+v; want one uncertain, one current, one gap", events)
	}
}

func TestRolloutActivityCapturesHelperUserMessage(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutCompletedItem(9, "child", `{"type":"UserMessage","id":"input","content":[{"type":"text","text":"helper task"}]}`),
		rolloutMessage(10, "reply", "helper reply"))
	setActivityHistoryMode(t, child, "paginated")
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	want := []aicli.StreamEvent{
		{NativeID: "child", Kind: "user", Text: "helper task"},
		{NativeID: "child", Kind: "assistant_response_item", Text: "helper reply"},
	}
	if !slices.Equal(*events, want) {
		t.Fatalf("helper prompt or reply lost: got %+v; want %+v", *events, want)
	}
}

func TestRolloutActivityIgnoresKnownMetadata(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	events := []string{}
	for ordinal, kind := range []string{"additional_tools", "configuration_update", "compaction", "context_compaction"} {
		events = append(events, fmt.Sprintf(`{"ordinal":%d,"type":"response_item","payload":{"type":%q}}`, ordinal+9, kind))
	}
	for ordinal, kind := range []string{"turn_context", "token_usage_record", "world_state", "compacted"} {
		events = append(events, fmt.Sprintf(`{"ordinal":%d,"type":%q,"payload":{}}`, ordinal+13, kind))
	}
	events = append(events, rolloutMessage(17, "message", "discussion after metadata"))
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary, events...)
	activity, got, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if len(*got) != 1 || (*got)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "discussion after metadata"}) {
		t.Fatalf("response metadata caused discussion gap or hid later content: %+v", *got)
	}
}

func TestRolloutActivityUnknownResponseMessageRoleLeavesGap(t *testing.T) {
	for _, test := range []struct {
		role string
		want []aicli.StreamEvent
	}{
		{"future_assistant", []aicli.StreamEvent{{NativeID: "child", Kind: "content_missing"}}},
		{"developer", nil},
		{"tool", nil},
	} {
		t.Run(test.role, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			boundary := int64(9)
			line := fmt.Sprintf(`{"ordinal":9,"type":"response_item","payload":{"type":"message","role":%q,"content":[{"type":"output_text","text":"visible unknown role"}]}}`, test.role)
			writeUsageRolloutBoundary(t, home, "child", "root", &boundary, line)
			activity, events, _ := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			activity.finish()
			if !slices.Equal(*events, test.want) {
				t.Fatalf("%s response message = %+v; want %+v", test.role, *events, test.want)
			}
		})
	}
}

func TestRolloutActivityUnknownRecordTypeGapsEligibleHelper(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		`{"ordinal":9,"type":"future_record","payload":{"text":"possibly visible"}}`,
		rolloutMessage(10, "message", "discussion after unknown record"))
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	activity.finish()
	if len(*events) != 2 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "content_missing"}) ||
		(*events)[1] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "discussion after unknown record"}) {
		t.Fatalf("unknown record should gap once and preserve following discussion: %+v", *events)
	}
}

func TestRolloutActivityLineAtEventByteLimit(t *testing.T) {
	const prefix = `{"ordinal":9,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`
	const suffix = `"}]}}`
	for _, ending := range []string{"\n", "\r\n"} {
		for _, extra := range []int{0, 1} {
			name := fmt.Sprintf("ending_bytes=%d/extra=%d", len(ending), extra)
			t.Run(name, func(t *testing.T) {
				home, started := t.TempDir(), time.Now()
				writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
				boundary := int64(9)
				child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
					usageEventAt(8, "task_started", "inherited-turn"))
				textBytes := aicli.MaxEventBytes - len(prefix) - len(suffix) - len(ending) + 1 + extra // LF is the delimiter, not part of the JSONL line.
				f, err := os.OpenFile(child, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				var writeErr error
				for _, part := range []string{prefix, strings.Repeat("x", textBytes), suffix, ending, rolloutMessage(10, "after-limit", "valid after limit") + "\n"} {
					if _, writeErr = f.WriteString(part); writeErr != nil {
						break
					}
				}
				if err := joinClose(writeErr, f.Close()); err != nil {
					t.Fatal(err)
				}
				activity, events, _ := newActivityTestRecorder(t, home, started)
				activity.root("root")
				activity.child("child", true)
				activity.poll()
				activity.finish()
				if len(*events) != 2 {
					t.Fatalf("line bytes=%d: got %d events; want two", aicli.MaxEventBytes+extra, len(*events))
				}
				first := (*events)[0]
				if extra == 0 {
					if first.NativeID != "child" || first.Kind != "assistant" || len(first.Text) != textBytes || strings.Trim(first.Text, "x") != "" {
						t.Fatalf("exact line not captured: id=%q kind=%q text_bytes=%d", first.NativeID, first.Kind, len(first.Text))
					}
				} else if first != (aicli.StreamEvent{NativeID: "child", Kind: "content_missing"}) {
					t.Fatalf("oversized line not gapped: id=%q kind=%q text_bytes=%d", first.NativeID, first.Kind, len(first.Text))
				}
				if (*events)[1] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "valid after limit"}) {
					t.Fatalf("following line lost: %+v", (*events)[1])
				}
			})
		}
	}
}

func TestRolloutActivityExactScanCapGapsOnlyUnreadBytes(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutMessage(9, "first", "already captured"))
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if len(*events) != 1 || (*events)[0].Text != "already captured" {
		t.Fatalf("setup did not capture helper: %+v", *events)
	}
	activity.scanned = maxUsageScanBytes
	activity.poll()
	if len(*events) != 1 {
		t.Fatalf("exact cap at EOF invented a gap: %+v", *events)
	}
	appendUsageEvents(t, child, rolloutMessage(10, "unread", "beyond cap"))
	activity.poll()
	if len(*events) != 2 || (*events)[1] != (aicli.StreamEvent{NativeID: "child", Kind: "content_missing"}) {
		t.Fatalf("unread bytes beyond cap lacked one gap: %+v", *events)
	}
}

func TestRolloutActivityFinalScanMarksUnrepresentedContent(t *testing.T) {
	for _, test := range []struct {
		name      string
		events    []string
		immediate bool
	}{
		{"completed item without response item", []string{
			rolloutCompletedItem(9, "child", `{"type":"AgentMessage","id":"message","content":[{"type":"Text","text":"only here"}]}`),
		}, false},
		{"unexpected persisted started and updated", []string{
			`{"ordinal":9,"type":"event_msg","payload":{"type":"item_started","thread_id":"child","item":{"type":"AgentMessage","id":"pending","content":[{"type":"Text","text":"partial"}]}}}`,
			`{"ordinal":10,"type":"event_msg","payload":{"type":"item_updated","thread_id":"child","item":{"type":"AgentMessage","id":"pending","content":[{"type":"Text","text":"still partial"}]}}}`,
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, started := t.TempDir(), time.Now()
			writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
			boundary := int64(9)
			writeUsageRolloutBoundary(t, home, "child", "root", &boundary, test.events...)
			activity, events, _ := newActivityTestRecorder(t, home, started)
			activity.root("root")
			activity.child("child", true)
			activity.poll()
			if test.immediate && (len(*events) != 1 || (*events)[0].Kind != "content_missing") {
				t.Fatalf("unexpected persisted partial item did not immediately withhold coverage: %+v", *events)
			}
			activity.finish()
			if len(*events) != 1 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "content_missing"}) {
				t.Fatalf("unrepresented content should withhold coverage once: %+v", *events)
			}
		})
	}
}
