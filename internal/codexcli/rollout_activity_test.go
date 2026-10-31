package codexcli

import (
	"encoding/json"
	"fmt"
	"os"
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

func rolloutSpawn(ordinal int, parent, child string) string {
	item := fmt.Sprintf(`{"type":"SubAgentActivity","id":"spawn","kind":"started","agent_thread_id":%q,"agent_path":"/root/helper"}`, child)
	return rolloutCompletedItem(ordinal, parent, item)
}

func newActivityTestRecorder(t *testing.T, home string, started time.Time) (*rolloutActivity, *[]aicli.StreamEvent, *int) {
	t.Helper()
	var events []aicli.StreamEvent
	progress := 0
	activity := newRolloutActivity(t.Context(), home, started, nil, nil, nil,
		func(event aicli.StreamEvent) { events = append(events, event) },
		func() { progress++ })
	return activity, &events, &progress
}

func TestRolloutActivityRootAnnouncesQuietHelperDiscussion(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "",
		rolloutMessage(8, "root-message", "root already streamed on stdout"),
		rolloutSpawn(10, "root", "child"))
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

func TestRolloutActivityPaginatedUsesCompletedItemsWithoutResponseDuplicates(t *testing.T) {
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
	if len(*events) != 3 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "paginated reply"}) ||
		(*events)[1] != (aicli.StreamEvent{NativeID: "child", Kind: "reasoning_summary", Text: "visible summary"}) ||
		(*events)[2].Kind != "tool" || !json.Valid([]byte((*events)[2].Text)) ||
		!strings.Contains((*events)[2].Text, `"aggregated_output":"PASS"`) {
		t.Fatalf("paginated content/duplicates = %+v", *events)
	}
	for _, event := range *events {
		if strings.Contains(event.Text, "hidden reasoning") {
			t.Fatalf("raw reasoning leaked: %+v", event)
		}
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
		rolloutSpawn(9, "child", "grandchild"),
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
	activity.poll()
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
	activity := newRolloutActivity(t.Context(), home, started, session.known, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) },
		func() { progress++ })
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if len(events) != 1 || events[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "current helper text"}) || progress == 0 {
		t.Fatalf("resume replayed baseline or missed suffix: events=%+v progress=%d", events, progress)
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
	activity := newRolloutActivity(t.Context(), home, started, session.known, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) }, func() {})
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if err := os.WriteFile(child, original, 0600); err != nil {
		t.Fatal(err)
	}
	appendUsageEvents(t, child, rolloutMessage(14, "retry-message", "restored suffix must stay blocked"))
	activity.poll()
	for _, event := range events {
		if strings.Contains(event.Text, "should not be trusted") || strings.Contains(event.Text, "restored suffix must stay blocked") {
			t.Fatalf("changed historical prefix admitted new content: %+v", events)
		}
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
	activity := newRolloutActivity(t.Context(), home, started, session.known, session.before, nil,
		func(event aicli.StreamEvent) { events = append(events, event) }, func() {})
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if len(events) != 1 || events[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "verified suffix"}) {
		t.Fatalf("verified resumed suffix mislabeled or replayed: %+v", events)
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
	activity := newRolloutActivity(t.Context(), home, started, session.known, session.before, nil,
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

func TestRolloutActivityIgnoresHelperUserMessage(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		rolloutCompletedItem(9, "child", `{"type":"UserMessage","id":"input","content":[{"type":"text","text":"private user prompt"}]}`),
		rolloutMessage(10, "reply", "helper reply"))
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if len(*events) != 1 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "helper reply"}) {
		t.Fatalf("helper user message leaked or blocked reply: %+v", *events)
	}
}

func TestRolloutActivityIgnoresResponseMetadata(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	boundary := int64(9)
	events := []string{}
	for ordinal, kind := range []string{"additional_tools", "configuration_update", "compaction", "context_compaction"} {
		events = append(events, fmt.Sprintf(`{"ordinal":%d,"type":"response_item","payload":{"type":%q}}`, ordinal+9, kind))
	}
	events = append(events, rolloutMessage(13, "message", "discussion after metadata"))
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary, events...)
	activity, got, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if len(*got) != 1 || (*got)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "discussion after metadata"}) {
		t.Fatalf("response metadata caused discussion gap or hid later content: %+v", *got)
	}
}

func TestRolloutActivityDropsOversizedLineAndKeepsFollowingEntry(t *testing.T) {
	home, started := t.TempDir(), time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"))
	boundary := int64(9)
	tooLarge := `{"ordinal":9,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` +
		strings.Repeat("x", aicli.MaxEventBytes) + `"}]}}`
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary,
		tooLarge, rolloutMessage(10, "after-large", "valid after large line"))
	activity, events, _ := newActivityTestRecorder(t, home, started)
	activity.root("root")
	activity.child("child", true)
	activity.poll()
	if len(*events) != 2 || (*events)[0] != (aicli.StreamEvent{NativeID: "child", Kind: "content_missing"}) ||
		(*events)[1] != (aicli.StreamEvent{NativeID: "child", Kind: "assistant", Text: "valid after large line"}) {
		t.Fatalf("oversized line leaked or stopped later entry: %+v", *events)
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
