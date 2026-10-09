package codexcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/usage"
)

func writeUsageRollout(t *testing.T, home, id, parent string, events ...string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "10", "09")
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
	got, complete := collectUsage(context.Background(), home, "root", started, &fallback, []string{"child"})
	want := usage.Tokens{Input: 410, CachedInput: 175, Output: 41}
	if got != want || !complete {
		t.Fatalf("collectUsage() = %+v, %v; want %+v, complete", got, complete, want)
	}
}

func TestCollectUsageUsesLatestCumulativeSnapshotAndSubtractsInheritedBaseline(t *testing.T) {
	home := t.TempDir()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"),
		usageSnapshot(12, 100, 20, 10), usageSnapshot(13, 250, 80, 30), usageEvent("task_complete", "root-turn"))
	writeUsageRollout(t, home, "child", "root", usageSnapshot(8, 100, 20, 10),
		usageEvent("task_started", "child-turn"), usageSnapshot(12, 130, 30, 12), usageSnapshot(13, 160, 40, 20), usageEvent("task_complete", "child-turn"))
	got, complete := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
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
			got, complete := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
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
			got, complete := collectUsage(context.Background(), home, test.rootID, time.Now(), &fallback, test.expected)
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
			got, complete := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
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
	got, complete := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
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
		got, complete := collectUsage(context.Background(), home, "root", time.Now(), nil, []string{"child"})
		if got != (usage.Tokens{Input: 30, Output: 3}) || complete {
			t.Fatalf("missing nested helper = %+v, %v; want known tokens, incomplete", got, complete)
		}
	}
}

func TestCollectUsageDetectsMissingResponseRecords(t *testing.T) {
	home := t.TempDir()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "turn"), usageRecord("root", "turn", "response", 10, 0, 1), usageSnapshot(18, 25, 10, 3), usageEvent("task_complete", "turn"))
	got, complete := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
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
		got, complete := collectUsage(context.Background(), home, "root", time.Now(), nil, nil)
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
		got, complete := collectUsage(ctx, home, "root", time.Now(), &fallback, nil)
		if got != fallback || complete {
			t.Fatalf("canceled scan = %+v, %t; want fallback, incomplete", got, complete)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		budget := int64(10)
		_, complete, _ := readRolloutUsage(context.Background(), rolloutMeta{ID: "root", path: root}, "root", &budget)
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
		got, complete := collectUsage(context.Background(), home, "root", time.Now(), &fallback, nil)
		if got != fallback || complete {
			t.Fatalf("too many rollout files = %+v, %t; want fallback, incomplete", got, complete)
		}
	})
}
