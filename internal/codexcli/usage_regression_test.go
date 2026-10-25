package codexcli

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/usage"
)

func TestUsageSessionRejectsChangedBaselineResponseIdentity(t *testing.T) {
	for _, test := range []struct {
		name, turn string
		input      int64
		complete   bool
	}{
		{"unchanged old response", "old", 100, true},
		{"changed old counters", "old", 101, false},
		{"old response under new turn", "new", 100, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, home := t.Context(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", 100)...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			appendUsageEvents(t, root, usageEvent("task_started", "new"), usageRecord("root", test.turn, "r1", test.input, 0, 0), usageRecord("root", "new", "r2", 20, 0, 0), usageEvent("task_complete", "new"))
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
			if got != (usage.Tokens{Input: 20}) || complete != test.complete || err != nil {
				t.Fatalf("baseline replay: %+v complete=%t err=%v", got, complete, err)
			}
		})
	}
}

func TestUsageSessionOverflowRetainsCurrentInvocationLowerBound(t *testing.T) {
	for _, mode := range []string{"current helper sum", "late old responses", "old baseline plus late response"} {
		t.Run(mode, func(t *testing.T) {
			ctx, home := t.Context(), t.TempDir()
			old := int64(0)
			if mode == "old baseline plus late response" {
				old = 1
			}
			root := writeUsageRollout(t, home, "root", "", append(completedUsageTurn("root", "old", "r1", old), usageSnapshot(25, old, 0, 0))...)
			var child string
			if mode == "current helper sum" {
				child = writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", 0)...)
			}
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			var expected []string
			want := usage.Tokens{Input: 2}
			if mode == "current helper sum" {
				appendUsageEvents(t, root, completedUsageTurn("root", "new", "r2", 2)...)
				appendUsageEvents(t, child, completedUsageTurn("child", "new-child", "c2", math.MaxInt64)...)
				expected = []string{"child"}
			} else {
				appendUsageEvents(t, root, usageRecord("root", "old", "late-1", math.MaxInt64, 0, 0))
				if mode == "late old responses" {
					appendUsageEvents(t, root, usageRecord("root", "old", "late-2", 1, 0, 0))
				}
				appendUsageEvents(t, root, append(completedUsageTurn("root", "new", "r2", 2), usageSnapshot(30, 2, 0, 0))...)
			}
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, expected, nil)
			if got != want || !got.Valid() || complete || err != nil {
				t.Fatalf("overflow changed attributable lower bound: %+v complete=%t err=%v", got, complete, err)
			}
		})
	}
}

func TestUsageRecordLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("scans the actual 100000-record identity boundary")
	}
	var events strings.Builder
	events.WriteString(usageEvent("task_started", "turn") + "\n")
	for i := range maxUsageRecords {
		events.WriteString(usageRecord("root", "turn", fmt.Sprint(i), 1, 0, 0) + "\n")
	}
	events.WriteString(usageEvent("task_complete", "turn"))
	path := writeUsageRollout(t, t.TempDir(), "root", "", events.String())
	appendUsageEvents(t, path, usageRecord("root", "turn", "0", 1, 0, 0), usageEvent("task_complete", "turn"))
	for _, extra := range []bool{false, true} {
		if extra {
			appendUsageEvents(t, path, usageRecord("root", "turn", "one-more", 1, 0, 0), usageEvent("task_complete", "turn"))
		}
		budget := int64(maxUsageScanBytes)
		capture := &usageBoundary{valid: true, turns: map[string]bool{}}
		got, complete, _, err := readRolloutUsageFrom(t.Context(), rolloutMeta{ID: "root", path: path}, "root", &budget, rolloutUsageMode{capture: capture})
		if got != (usage.Tokens{Input: maxUsageRecords}) || complete == extra || err != nil {
			t.Fatalf("record cap: %+v complete=%t extra=%t err=%v", got, complete, extra, err)
		}
		if len(capture.responses) != maxUsageRecords || capture.valid == extra || !capture.prefixKnown || !capture.completed {
			t.Fatalf("response cap lost bounded history or physical/lifecycle proof: responses=%d valid=%t prefixKnown=%t completed=%t", len(capture.responses), capture.valid, capture.prefixKnown, capture.completed)
		}
	}
	// Logical retention limits must not stop a later trustworthy root floor.
	appendUsageEvents(t, path, usageSnapshot(30, maxUsageRecords+17, 0, 0))
	budget := int64(maxUsageScanBytes)
	got, complete, _, err := readRolloutUsage(t.Context(), rolloutMeta{ID: "root", path: path}, "root", &budget)
	if got != (usage.Tokens{Input: maxUsageRecords + 17}) || complete || err != nil {
		t.Fatalf("record cap lost later root lower bound: %+v complete=%t err=%v", got, complete, err)
	}
	// Losing counters must not by itself revoke known helper completion.
	childEvents := strings.ReplaceAll(events.String(), `"thread_id":"root"`, `"thread_id":"child"`)
	child := writeUsageRollout(t, t.TempDir(), "child", "root", childEvents, usageRecord("child", "turn", "one-more", 1, 0, 0), usageEvent("task_complete", "turn"))
	boundary := int64(9)
	budget = maxUsageScanBytes
	got, complete, _, err = readRolloutUsage(t.Context(), rolloutMeta{ID: "child", path: child, HistoryStart: &boundary}, "root", &budget)
	if got != (usage.Tokens{Input: maxUsageRecords}) || complete || err != nil {
		t.Fatalf("counter-only cap revoked helper lifecycle proof: %+v complete=%t err=%v", got, complete, err)
	}
}

func TestUsageLifecycleTurnLimitDoesNotCertifyCappedHelper(t *testing.T) {
	const recordLimit = 1000
	var events strings.Builder
	events.WriteString(usageRecord("child", "0", "response", 7, 0, 0) + "\n")
	for i := range recordLimit {
		events.WriteString(usageEvent("task_complete", fmt.Sprint(i)) + "\n")
	}
	path := writeUsageRollout(t, t.TempDir(), "child", "root", strings.TrimSuffix(events.String(), "\n"))
	appendUsageEvents(t, path, usageRecord("child", "0", "response", 7, 0, 0), usageEvent("task_complete", "0"))
	boundary := int64(9)
	meta := rolloutMeta{ID: "child", path: path, HistoryStart: &boundary}
	for _, extra := range []bool{false, true} {
		if extra {
			appendUsageEvents(t, path, usageEvent("task_complete", "one-more"))
		}
		budget := int64(maxUsageScanBytes)
		capture := &usageBoundary{valid: true, turns: map[string]bool{}}
		got, complete, _, err := readRolloutUsageFrom(t.Context(), meta, "root", &budget, rolloutUsageMode{capture: capture, recordLimit: recordLimit})
		if got != (usage.Tokens{Input: 7}) || complete == extra || errors.Is(err, errUnverifiedChildren) != extra || (err != nil && !extra) {
			t.Fatalf("helper lifecycle cap: %+v complete=%t extra=%t err=%v", got, complete, extra, err)
		}
		if len(capture.turns) != recordLimit || capture.valid == extra || capture.completed == extra || !capture.prefixKnown {
			t.Fatalf("turn cap proof: turns=%d valid=%t completed=%t prefixKnown=%t extra=%t", len(capture.turns), capture.valid, capture.completed, capture.prefixKnown, extra)
		}
	}
}

func TestUsageTurnLimitCountsLifecycleAndResponseIdentitiesTogether(t *testing.T) {
	const recordLimit = 1000
	var events strings.Builder
	for i := range recordLimit / 2 {
		events.WriteString(usageEvent("task_complete", fmt.Sprintf("lifecycle-%d", i)) + "\n")
	}
	for i := range recordLimit / 2 {
		id := fmt.Sprintf("response-%d", i)
		events.WriteString(usageRecord("root", id, id, 1, 0, 0) + "\n" + usageEvent("task_complete", id) + "\n")
	}
	path := writeUsageRollout(t, t.TempDir(), "root", "", strings.TrimSuffix(events.String(), "\n"))
	for _, extra := range []bool{false, true} {
		if extra {
			appendUsageEvents(t, path, usageRecord("root", "one-more", "one-more", 1, 0, 0), usageEvent("task_complete", "one-more"))
		}
		budget := int64(maxUsageScanBytes)
		got, complete, _, err := readRolloutUsageFrom(t.Context(), rolloutMeta{ID: "root", path: path}, "root", &budget, rolloutUsageMode{recordLimit: recordLimit})
		if got != (usage.Tokens{Input: recordLimit / 2}) || complete == extra || err != nil {
			t.Fatalf("disjoint turn trackers exceeded their shared cap: %+v complete=%t extra=%t err=%v", got, complete, extra, err)
		}
	}
}

func TestUsageLateOldResponseLimitPreservesSubtractionCertainty(t *testing.T) {
	const recordLimit = 1000
	ctx := t.Context()
	path := writeUsageRollout(t, t.TempDir(), "root", "", append(completedUsageTurn("root", "old", "baseline", 0), usageSnapshot(25, 0, 0, 0))...)
	before := &usageBoundary{valid: true, turns: map[string]bool{}}
	budget := int64(maxUsageScanBytes)
	_, complete, _, err := readRolloutUsageFrom(ctx, rolloutMeta{ID: "root", path: path}, "root", &budget, rolloutUsageMode{capture: before})
	if !complete || !before.valid || err != nil {
		t.Fatalf("invalid baseline fixture: complete=%t valid=%t err=%v", complete, before.valid, err)
	}
	var events strings.Builder
	for i := range recordLimit {
		events.WriteString(usageRecord("root", "old", fmt.Sprint(i), 1, 0, 0) + "\n")
	}
	events.WriteString(usageEvent("task_started", "new") + "\n" + usageSnapshot(30, recordLimit+20, 0, 0) + "\n" + usageEvent("task_complete", "new"))
	appendUsageEvents(t, path, events.String())
	for _, extra := range []bool{false, true} {
		if extra {
			appendUsageEvents(t, path, usageRecord("root", "old", "one-more", 1, 0, 0), usageSnapshot(31, recordLimit+21, 0, 0), usageEvent("task_complete", "new"))
		}
		budget := int64(maxUsageScanBytes)
		got, complete, _, err := readRolloutUsageFrom(ctx, rolloutMeta{ID: "root", path: path}, "root", &budget, rolloutUsageMode{before: before, recordLimit: recordLimit})
		want := usage.Tokens{Input: 20}
		if extra {
			want = usage.Tokens{}
		}
		if got != want || complete || err != nil {
			t.Fatalf("late-old cap must not certify incomplete usage: %+v want=%+v complete=%t extra=%t err=%v", got, want, complete, extra, err)
		}
	}
}

func TestUsageCappedBaselineCannotAttributeDroppedOldIdentity(t *testing.T) {
	const recordLimit = 1000
	ctx := t.Context()
	var events strings.Builder
	for i := range recordLimit + 1 {
		id := fmt.Sprint(i)
		events.WriteString(usageRecord("root", id, id, 1, 0, 0) + "\n" + usageEvent("task_complete", id) + "\n")
	}
	events.WriteString(usageSnapshot(25, recordLimit+1, 0, 0))
	path := writeUsageRollout(t, t.TempDir(), "root", "", events.String())
	meta := rolloutMeta{ID: "root", path: path}
	before := &usageBoundary{valid: true, turns: map[string]bool{}}
	budget := int64(maxUsageScanBytes)
	_, complete, _, err := readRolloutUsageFrom(ctx, meta, "root", &budget, rolloutUsageMode{capture: before, recordLimit: recordLimit})
	if complete || before.valid || !before.prefixKnown || len(before.turns) != recordLimit || len(before.responses) != recordLimit || err != nil {
		t.Fatalf("capped baseline fixture: complete=%t valid=%t prefixKnown=%t turns=%d responses=%d err=%v", complete, before.valid, before.prefixKnown, len(before.turns), len(before.responses), err)
	}
	budget = maxUsageScanBytes
	if !unchangedUsageRollout(ctx, path, before, &budget) {
		t.Fatal("logical identity cap invalidated unchanged physical history")
	}
	// The dropped last ID is historical, even though the bounded maps lack it.
	appendUsageEvents(t, path, usageRecord("root", fmt.Sprint(recordLimit), fmt.Sprint(recordLimit), 1, 0, 0), usageEvent("task_started", "new"), usageRecord("root", "new", "new", 20, 0, 0), usageSnapshot(30, recordLimit+21, 0, 0), usageEvent("task_complete", "new"))
	budget = maxUsageScanBytes
	got, complete, _, err := readRolloutUsageFrom(ctx, meta, "root", &budget, rolloutUsageMode{before: before})
	if got != (usage.Tokens{}) || complete || err != nil {
		t.Fatalf("capped baseline treated an unknown old ID as current: %+v complete=%t err=%v", got, complete, err)
	}
}

func TestUnchangedUsageRolloutRespectsExactRemainingBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout")
	data := []byte("known historical bytes\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	before := usageBoundary{prefixKnown: true, size: int64(len(data)), hash: sha256.Sum256(data)}
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprintf("one-byte-short=%t", short), func(t *testing.T) {
			budget := before.size
			if short {
				budget--
			}
			if got := unchangedUsageRollout(t.Context(), path, &before, &budget); got == short {
				t.Fatalf("byte proof=%t short=%t remaining=%d", got, short, budget)
			}
		})
	}
}

func TestUsageDiscoveryExactWalkLimitAndNextEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("creates the actual 100000-entry discovery boundary")
	}
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	if err := os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessions, "root.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"root","session_id":"root"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Only directory children are visited; root.jsonl consumes one entry.
	for i := range maxUsageWalkEntries - 1 {
		if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("unrelated-%06d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	known := map[string]rolloutMeta{"root": {ID: "root", SessionID: "root", path: path}}
	for _, extra := range []bool{false, true} {
		if extra {
			if err := os.WriteFile(filepath.Join(sessions, "one-more"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		files, ambiguous, complete := usageFiles(t.Context(), home, "root", time.Now(), known)
		if complete == extra || len(files) != 1 || ambiguous["root"] || files["root"].path != path {
			t.Fatalf("walk limit: complete=%t extra=%t files=%+v ambiguous=%v", complete, extra, files, ambiguous)
		}
	}
}

func TestUsageErrorJoinDeduplicatesSharedSentinels(t *testing.T) {
	got := joinUsageErrors(errUnverifiedChildren, errUsageAgentLimit, errors.Join(errUnverifiedChildren, errUsageAgentLimit))
	if strings.Count(got.Error(), errUnverifiedChildren.Error()) != 1 || strings.Count(got.Error(), errUsageAgentLimit.Error()) != 1 || !errors.Is(got, errUnverifiedChildren) || !errors.Is(got, errUsageAgentLimit) || !errors.Is(got, aicli.ErrOutputLimit) {
		t.Fatalf("joined completion causes lost or duplicated: %v", got)
	}
}

func TestCollectUsageManyMissingHelpersReportOneCompletionCause(t *testing.T) {
	ctx, home := t.Context(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", 10)...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, completedUsageTurn("root", "new", "r2", 20)...)
	for _, collector := range []*usageSession{{}, &session} {
		got, complete, err := collector.collectUsage(ctx, home, "root", time.Now(), nil, []string{"missing-1", "missing-2", "missing-3"}, nil)
		want := int64(30)
		if collector == &session {
			want = 20
		}
		if got != (usage.Tokens{Input: want}) || complete || err == nil || strings.Count(err.Error(), errUnverifiedChildren.Error()) != 1 {
			t.Fatalf("duplicate completion cause: %+v complete=%t err=%v", got, complete, err)
		}
	}
}
