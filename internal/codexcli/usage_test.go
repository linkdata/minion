package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/usage"
)

func writeUsageRollout(t *testing.T, home, id, parent string, events ...string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", time.Now().Format("2006/01/02"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{"id": id, "session_id": "root", "source": "exec"}
	if parent != "" {
		meta["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent}}}
		meta["subagent_history_start_ordinal"] = 9
	}
	first, err := json.Marshal(map[string]any{"type": "session_meta", "payload": meta})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+id+".jsonl")
	if err := os.WriteFile(path, []byte(string(first)+"\n"+strings.Join(events, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func usageEvent(kind, turn string) string {
	return fmt.Sprintf(`{"ordinal":20,"type":"event_msg","payload":{"type":%q,"turn_id":%q}}`, kind, turn)
}

func usageRecord(thread, turn, response string, input, cached, output int64) string {
	return fmt.Sprintf(`{"ordinal":15,"type":"token_usage_record","payload":{"thread_id":%q,"session_id":"root","turn_id":%q,"response_id":%q,"usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}`, thread, turn, response, input, cached, output)
}

func usageSnapshot(ordinal, input, cached, output int64) string {
	return fmt.Sprintf(`{"ordinal":%d,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}}`, ordinal, input, cached, output)
}

func TestCollectUsageIncludesNestedHelpersWithoutInheritedOrDuplicateResponses(t *testing.T) {
	home := t.TempDir()
	started := time.Now()
	rootRecord := usageRecord("root", "root-turn", "response-1", 100, 50, 10)
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), rootRecord,
		usageRecord("root", "root-turn", "response-2", 200, 100, 20),
		usageSnapshot(16, 300, 150, 30), usageEvent("task_complete", "root-turn"))
	childRecord := usageRecord("child", "child-turn", "response-3", 70, 25, 7)
	writeUsageRollout(t, home, "child", "root",
		`{"ordinal":1,"type":"session_meta","payload":{"id":"root","source":"exec"}}`,
		rootRecord, usageSnapshot(8, 1000, 800, 100), usageEvent("task_started", "child-turn"),
		childRecord, childRecord, usageSnapshot(16, 1070, 825, 107), usageEvent("task_complete", "child-turn"))
	writeUsageRollout(t, home, "grandchild", "child", childRecord,
		usageEvent("task_started", "grandchild-turn"), usageRecord("grandchild", "grandchild-turn", "response-4", 40, 0, 4), usageEvent("task_complete", "grandchild-turn"))
	// A valid unrelated header keeps its malformed transcript out of the parser.
	other := writeUsageRollout(t, home, "unrelated", "", `malformed private content`)
	data, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte(strings.Replace(string(data), `"session_id":"root"`, `"session_id":"unrelated"`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	fallback := usage.Tokens{Input: 300, CachedInput: 150, Output: 30}
	got, complete, _ := collectUsage(context.Background(), home, "root", started, &fallback, []string{"child"})
	want := usage.Tokens{Input: 410, CachedInput: 175, Output: 41}
	if got != want || !complete {
		t.Fatalf("collectUsage() = %+v, %v; want %+v, complete", got, complete, want)
	}
}

func TestRepeatedHelperActivityRetainsLaterCountersAndCompletion(t *testing.T) {
	for _, done := range []bool{false, true} {
		t.Run(fmt.Sprint(done), func(t *testing.T) {
			home := t.TempDir()
			activity := `{"ordinal":17,"type":"event_msg","payload":{"type":"item_completed","thread_id":"root","item":{"type":"SubAgentActivity","agent_thread_id":"child"}}}`
			writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "before", 10, 0, 1), strings.TrimSuffix(strings.Repeat(activity+"\n", 300), "\n"), usageRecord("root", "root-turn", "after", 1000, 0, 100), usageEvent("task_complete", "root-turn"))
			child := []string{usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "child-response", 20, 0, 2)}
			if done {
				child = append(child, usageEvent("task_complete", "child-turn"))
			}
			writeUsageRollout(t, home, "child", "root", child...)
			tokens, complete, childError := collectUsage(context.Background(), home, "root", time.Now(), nil, []string{"child"})
			if tokens != (usage.Tokens{Input: 1030, Output: 103}) || complete != done || (childError != nil) == done {
				t.Fatalf("repeated activity lost counters/lifecycle: tokens=%+v complete=%t childError=%v", tokens, complete, childError)
			}
		})
	}
}

func TestUsageAgentBoundsCountUniqueIDsAndRejectOverflow(t *testing.T) {
	t.Run("selected tree retains root and sorted bounded children", func(t *testing.T) {
		home := t.TempDir()
		writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 100, 10, 7), usageEvent("task_complete", "root-turn"))
		want := usage.Tokens{Input: 100, CachedInput: 10, Output: 7}
		for i := maxUsageAgents - 1; i >= 0; i-- {
			id := fmt.Sprintf("child-%03d", i)
			writeUsageRollout(t, home, id, "root", usageEvent("task_started", "turn"), usageRecord(id, "turn", id, int64(i+1), 0, 1), usageEvent("task_complete", "turn"))
			if i < maxCodexChildren {
				want.Add(usage.Tokens{Input: int64(i + 1), Output: 1})
			}
		}
		for _, fallback := range []*usage.Tokens{nil, {Input: 100, CachedInput: 10, Output: 7}} {
			for range 3 {
				tokens, complete, childError := collectUsage(context.Background(), home, "root", time.Now(), fallback, nil)
				if tokens != want || complete || !errors.Is(childError, aicli.ErrOutputLimit) {
					t.Fatalf("overflow discarded or randomized known counters: tokens=%+v want=%+v complete=%t error=%v", tokens, want, complete, childError)
				}
			}
		}
	})
	t.Run("rollout bound retains later counters", func(t *testing.T) {
		home := t.TempDir()
		var events []string
		for i := range maxUsageAgents + 1 {
			events = append(events, fmt.Sprintf(`{"ordinal":17,"type":"event_msg","payload":{"type":"collab_agent_spawn_end","new_thread_id":"child-%d"}}`, i))
		}
		events = append(events, usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "later-response", 1000, 0, 100), usageEvent("task_complete", "root-turn"))
		writeUsageRollout(t, home, "root", "", events...)
		tokens, complete, childError := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
		if tokens != (usage.Tokens{Input: 1000, Output: 100}) || complete || !errors.Is(childError, aicli.ErrOutputLimit) {
			t.Fatalf("overflow lost later counters or accepted work: tokens=%+v complete=%t error=%v", tokens, complete, childError)
		}
	})
}

func TestCollectUsageUsesLatestCumulativeSnapshotAndSubtractsInheritedBaseline(t *testing.T) {
	home := t.TempDir()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"),
		usageSnapshot(12, 100, 20, 10), usageSnapshot(13, 250, 80, 30), usageEvent("task_complete", "root-turn"))
	writeUsageRollout(t, home, "child", "root", usageSnapshot(8, 100, 20, 10),
		usageEvent("task_started", "child-turn"), usageSnapshot(12, 130, 30, 12), usageSnapshot(13, 160, 40, 20), usageEvent("task_complete", "child-turn"))
	got, complete, _ := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
	want := usage.Tokens{Input: 310, CachedInput: 100, Output: 40}
	if got != want || !complete {
		t.Fatalf("collectUsage() = %+v, %v; want %+v, complete", got, complete, want)
	}
}

func TestCollectUsagePartialRunsPreserveKnownTokens(t *testing.T) {
	for _, test := range []struct {
		name   string
		ending string
	}{
		{"interrupted", usageEvent("turn_aborted", "root-turn")},
		{"unterminated", ""},
		{"malformed", "{broken"},
		{"bad counts", usageSnapshot(20, -1, 0, 0)},
		{"oversized", strings.Repeat("x", maxEventBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "known", 60, 40, 3), test.ending)
			got, complete, _ := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
			if got != (usage.Tokens{Input: 60, CachedInput: 40, Output: 3}) || complete {
				t.Fatalf("partial run = %+v, %v", got, complete)
			}
		})
	}
}

func TestCollectUsageMissingLogsAndChildrenUseIncompleteFallback(t *testing.T) {
	fallback := usage.Tokens{Input: 100, CachedInput: 30, Output: 10}
	for _, test := range []struct {
		name, rootID string
		createRoot   bool
		expected     []string
	}{
		{"missing root", "root", false, nil},
		{"missing ID", "", false, nil},
		{"missing child", "root", true, []string{"missing"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			if test.createRoot {
				writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageSnapshot(12, 100, 30, 10), usageEvent("task_complete", "root-turn"))
			}
			got, complete, _ := collectUsage(context.Background(), home, test.rootID, time.Now(), &fallback, test.expected)
			if got != fallback || complete {
				t.Fatalf("collectUsage() = %+v, %v; want fallback, incomplete", got, complete)
			}
		})
	}
}

func TestCollectUsageUnfinishedChildAndTruncatedCompleteEventAreIncomplete(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint(truncated), func(t *testing.T) {
			home := t.TempDir()
			root := writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
			if truncated {
				data, err := os.ReadFile(root)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(root, data[:len(data)-1], 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				writeUsageRollout(t, home, "child", "root", usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "child-response", 20, 0, 2))
			}
			got, complete, _ := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
			if got.Input == 0 || complete {
				t.Fatalf("collectUsage() = %+v, %v; want known usage, incomplete", got, complete)
			}
		})
	}
}

func TestCollectUsageRequiresExplicitTokenFields(t *testing.T) {
	home := t.TempDir()
	record := strings.Replace(usageRecord("root", "turn", "response", 10, 0, 2), `"input_tokens":10,`, "", 1)
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "turn"), record, usageEvent("task_complete", "turn"))
	got, complete, _ := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
	if got != (usage.Tokens{}) || complete {
		t.Fatalf("missing required field = %+v, %v; want unknown", got, complete)
	}
}

func TestCollectUsageDetectsMissingNestedHelper(t *testing.T) {
	for _, spawn := range []string{
		`{"ordinal":17,"type":"event_msg","payload":{"type":"item_completed","thread_id":"child","item":{"type":"SubAgentActivity","kind":"started","agent_thread_id":"missing-grandchild"}}}`,
		`{"ordinal":17,"type":"event_msg","payload":{"type":"collab_agent_spawn_end","new_thread_id":"missing-grandchild"}}`,
	} {
		home := t.TempDir()
		writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
		writeUsageRollout(t, home, "child", "root", usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "child-response", 20, 0, 2), spawn, usageEvent("task_complete", "child-turn"))
		got, complete, _ := collectUsage(context.Background(), home, "root", time.Now(), nil, []string{"child"})
		if got != (usage.Tokens{Input: 30, Output: 3}) || complete {
			t.Fatalf("missing nested helper = %+v, %v; want known tokens, incomplete", got, complete)
		}
	}
}

func TestCollectUsageDetectsMissingResponseRecords(t *testing.T) {
	home := t.TempDir()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "turn"), usageRecord("root", "turn", "response", 10, 0, 1), usageSnapshot(18, 25, 10, 3), usageEvent("task_complete", "turn"))
	got, complete, _ := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
	if got != (usage.Tokens{Input: 25, CachedInput: 10, Output: 3}) || complete {
		t.Fatalf("mismatched cumulative counter = %+v, %v; want snapshot, incomplete", got, complete)
	}
}

func TestCollectUsageNeverAddsUnverifiedInheritedChildCounters(t *testing.T) {
	for _, withRecord := range []bool{false, true} {
		home := t.TempDir()
		writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
		child := []string{`corrupt inherited baseline`, usageEvent("task_started", "child-turn")}
		if withRecord {
			child = append(child, usageRecord("child", "child-turn", "child-response", 20, 5, 2))
		}
		child = append(child, usageSnapshot(18, 1020, 500, 100), usageEvent("task_complete", "child-turn"))
		writeUsageRollout(t, home, "child", "root", child...)
		got, complete, _ := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
		want := usage.Tokens{Input: 10, Output: 1}
		if withRecord {
			want.Add(usage.Tokens{Input: 20, CachedInput: 5, Output: 2})
		}
		if got != want || complete {
			t.Fatalf("unverified child baseline (own records=%v) = %+v, %v; want %+v, incomplete", withRecord, got, complete, want)
		}
	}
}

func TestCollectUsageBoundsScanningAndHonorsCancellation(t *testing.T) {
	home := t.TempDir()
	root := writeUsageRollout(t, home, "root", "", usageEvent("task_started", "turn"), usageRecord("root", "turn", "response", 10, 0, 1), usageEvent("task_complete", "turn"))
	fallback := usage.Tokens{Input: 10, Output: 1}

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, complete, _ := collectUsage(ctx, home, "root", time.Now(), &fallback, nil)
		if got != fallback || complete {
			t.Fatalf("canceled scan = %+v, %t; want fallback, incomplete", got, complete)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		budget := int64(10)
		_, complete, _, _ := readRolloutUsage(context.Background(), rolloutMeta{ID: "root", path: root}, "root", &budget)
		if complete || budget != 0 {
			t.Fatalf("limited rollout scan = complete=%t budget=%d; want incomplete and exhausted", complete, budget)
		}
	})
	t.Run("files", func(t *testing.T) {
		dir := filepath.Dir(root)
		for i := 0; i < maxUsageFiles; i++ {
			path := filepath.Join(dir, fmt.Sprintf("rollout-other-%04d.jsonl", i))
			body := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":\"other-%d\",\"session_id\":\"other\"}}\n", i)
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		got, complete, _ := collectUsage(context.Background(), home, "root", time.Now(), &fallback, nil)
		if got != fallback || complete {
			t.Fatalf("too many rollout files = %+v, %t; want fallback, incomplete", got, complete)
		}
	})
}

func TestRolloutUsageSkipsOversizedLinesAndDebitsBytes(t *testing.T) {
	home := t.TempDir()
	oversized := `{"type":"response_item","payload":"` + strings.Repeat("x", maxEventBytes) + `"}`
	root := writeUsageRollout(t, home, "root", "", usageEvent("task_started", "turn"),
		usageRecord("root", "turn", "before", 10, 5, 1), oversized,
		usageRecord("root", "turn", "after", 20, 10, 2),
		`{"ordinal":17,"type":"event_msg","payload":{"type":"collab_agent_spawn_end","new_thread_id":"late-child"}}`,
		usageEvent("task_complete", "turn"))
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	budget := info.Size() + 1024
	got, complete, children, _ := readRolloutUsage(context.Background(), rolloutMeta{ID: "root", path: root}, "root", &budget)
	want := usage.Tokens{Input: 30, CachedInput: 15, Output: 3}
	if got != want || complete || budget != 1024 || len(children) != 1 || children[0] != "late-child" {
		t.Fatalf("skipped line lost later usage/spawn or bytes: got=%+v complete=%t budget=%d children=%v", got, complete, budget, children)
	}
	writeUsageRollout(t, home, "late-child", "root", usageEvent("task_started", "child-turn"),
		usageRecord("late-child", "child-turn", "child-response", 40, 2, 4), usageEvent("task_complete", "child-turn"))
	got, complete, _ = collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
	want.Add(usage.Tokens{Input: 40, CachedInput: 2, Output: 4})
	if got != want || complete {
		t.Fatalf("later completed helper lost or incomplete reset: got=%+v complete=%t want=%+v", got, complete, want)
	}
}

func TestOversizedChildBaselineCountsOnlyOwnedResponses(t *testing.T) {
	for _, withRecord := range []bool{false, true} {
		t.Run(fmt.Sprint(withRecord), func(t *testing.T) {
			events := []string{strings.Repeat("x", maxEventBytes+1), usageEvent("task_started", "child-turn")}
			want := usage.Tokens{}
			if withRecord {
				events = append(events, usageRecord("child", "child-turn", "own-response", 20, 5, 2))
				want = usage.Tokens{Input: 20, CachedInput: 5, Output: 2}
			}
			events = append(events, usageSnapshot(18, 1020, 500, 100), usageEvent("task_complete", "child-turn"))
			path := writeUsageRollout(t, t.TempDir(), "child", "root", events...)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			budget := info.Size() + 100
			start := int64(9)
			got, complete, _, _ := readRolloutUsage(context.Background(), rolloutMeta{ID: "child", path: path, HistoryStart: &start}, "root", &budget)
			if got != want || complete || budget != 100 {
				t.Fatalf("lost child baseline inflated totals: own=%t got=%+v complete=%t budget=%d want=%+v", withRecord, got, complete, budget, want)
			}
		})
	}
}

func TestRolloutScannerEventBoundaryAndPartialEOF(t *testing.T) {
	const prefix, suffix = `{"type":"response_item","payload":"`, `"}`
	for _, test := range []struct {
		name      string
		size      int
		ending    string
		wantLines int
		complete  bool
	}{
		{"exact boundary", maxEventBytes, "\n", 1, true},
		{"exact partial EOF", maxEventBytes, "", 1, false},
		{"oversized then valid", maxEventBytes + 1, "\n{}\n", 1, false},
		{"oversized partial EOF", maxEventBytes + 1, "", 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			line := prefix + strings.Repeat("x", test.size-len(prefix)-len(suffix)) + suffix
			complete := true
			scanner := rolloutScanner(context.Background(), strings.NewReader(line+test.ending), &complete)
			lines := 0
			for scanner.Scan() {
				lines++
				if !json.Valid(scanner.Bytes()) || (test.size == maxEventBytes && len(scanner.Bytes()) != maxEventBytes) {
					t.Fatalf("complete boundary JSON corrupted: length=%d", len(scanner.Bytes()))
				}
			}
			if scanner.Err() != nil || lines != test.wantLines || complete != test.complete {
				t.Fatalf("boundary scan: lines=%d complete=%t err=%v want=%d/%t", lines, complete, scanner.Err(), test.wantLines, test.complete)
			}
		})
	}
}

func TestOversizedRolloutStillExhaustsOriginalBudget(t *testing.T) {
	path := writeUsageRollout(t, t.TempDir(), "root", "", usageEvent("task_started", "turn"),
		usageRecord("root", "turn", "before", 10, 0, 1), strings.Repeat("x", 2*maxEventBytes+1),
		usageRecord("root", "turn", "unreachable", 1000, 0, 100), usageEvent("task_complete", "turn"))
	budget := int64(maxEventBytes + (256 << 10))
	got, complete, _, _ := readRolloutUsage(context.Background(), rolloutMeta{ID: "root", path: path}, "root", &budget)
	if got != (usage.Tokens{Input: 10, Output: 1}) || complete || budget != 0 {
		t.Fatalf("oversized discard bypassed budget: got=%+v complete=%t budget=%d", got, complete, budget)
	}
}

func TestRolloutScannerCancellationWhileDiscarding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancelingRolloutReader{Reader: strings.NewReader(strings.Repeat("x", 3*maxEventBytes)), cancel: cancel, cancelAfter: maxEventBytes + 2}
	complete := true
	scanner := rolloutScanner(ctx, reader, &complete)
	if scanner.Scan() || !errors.Is(scanner.Err(), context.Canceled) || reader.bytes > 2*(maxEventBytes+2) || complete {
		t.Fatalf("discard ignored cancellation: read=%d complete=%t err=%v", reader.bytes, complete, scanner.Err())
	}
}

type cancelingRolloutReader struct {
	io.Reader
	cancel      context.CancelFunc
	bytes       int
	cancelAfter int
}

func (r *cancelingRolloutReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	if r.bytes > r.cancelAfter {
		r.cancel()
	}
	return n, err
}

func TestCollectUsageMergesRootLowerBoundsByComponent(t *testing.T) {
	for _, source := range []string{"stdout", "snapshot"} {
		t.Run(source, func(t *testing.T) {
			home := t.TempDir()
			events := []string{usageEvent("task_started", "turn"), usageRecord("root", "turn", "response", 100, 10, 1)}
			var fallback *usage.Tokens
			if source == "stdout" {
				fallback = &usage.Tokens{Input: 90, CachedInput: 20, Output: 20}
			} else {
				events = append(events, usageSnapshot(18, 90, 20, 20))
			}
			events = append(events, usageEvent("task_complete", "turn"))
			writeUsageRollout(t, home, "root", "", events...)
			got, complete, err := collectUsage(context.Background(), home, "root", time.Now(), fallback, nil)
			if got != (usage.Tokens{Input: 100, CachedInput: 20, Output: 20}) || complete || err != nil {
				t.Fatalf("component lower bounds lost: %+v complete=%t err=%v", got, complete, err)
			}
		})
	}
}

func TestCollectUsageRetainsValidCountersOnOverflow(t *testing.T) {
	for _, source := range []string{"response", "helper", "stdout maximum", "snapshot maximum"} {
		t.Run(source, func(t *testing.T) {
			home := t.TempDir()
			known := usage.Tokens{Input: math.MaxInt64 - 1, Output: 1}
			events := []string{usageEvent("task_started", "turn"), usageRecord("root", "turn", "response", known.Input, 0, known.Output)}
			var fallback *usage.Tokens
			switch source {
			case "response":
				events = append(events, usageRecord("root", "turn", "response-2", 2, 0, 0))
			case "helper":
				writeUsageRollout(t, home, "child", "root", usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "child-response", 2, 0, 0), usageEvent("task_complete", "child-turn"))
			case "stdout maximum":
				fallback = &usage.Tokens{Input: 1, Output: math.MaxInt64 - 1}
			case "snapshot maximum":
				events = append(events, usageSnapshot(18, 1, 0, math.MaxInt64-1))
			}
			events = append(events, usageEvent("task_complete", "turn"))
			writeUsageRollout(t, home, "root", "", events...)
			got, complete, err := collectUsage(context.Background(), home, "root", time.Now(), fallback, nil)
			if got != known || !got.Valid() || complete || err != nil {
				t.Fatalf("overflow discarded valid counters: %+v complete=%t err=%v", got, complete, err)
			}
		})
	}
}

func TestInterruptedHelperReasonDoesNotProveCompletion(t *testing.T) {
	path := writeUsageRollout(t, t.TempDir(), "child", "root", usageEvent("task_started", "turn"), usageRecord("child", "turn", "response", 20, 0, 2), `{"ordinal":21,"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn","reason":"interrupted"}}`)
	start := int64(9)
	budget := int64(maxUsageScanBytes)
	tokens, complete, _, err := readRolloutUsage(context.Background(), rolloutMeta{ID: "child", path: path, HistoryStart: &start}, "root", &budget)
	if tokens != (usage.Tokens{Input: 20, Output: 2}) || complete || err == nil || !strings.Contains(err.Error(), "helper agent work did not complete") {
		t.Fatalf("interruption reason accepted as completed work: %+v complete=%t err=%v", tokens, complete, err)
	}
}
