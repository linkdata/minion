package codexcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/usage"
)

// This shape was observed in the owned Codex 0.160 root rollout. The namespace
// tool result names a canonical agent path; the activity event supplies its UUID.
func nativeActivityEvent(ordinal int, thread, turn, kind, child string) string {
	return fmt.Sprintf(`{"ordinal":%d,"type":"event_msg","payload":{"type":"item_completed","thread_id":%q,"turn_id":%q,"item":{"type":"SubAgentActivity","id":"call_fixture","kind":%q,"agent_thread_id":%q,"agent_path":"/root/history_helper"}}}`, ordinal, thread, turn, kind, child)
}

func TestUsageSessionNativeSubAgentActivityNewSpawnOnResume(t *testing.T) {
	for _, test := range []struct {
		name, kind, thread, turn       string
		missing, unrelated, unfinished bool
		want                           int64
		complete, unverified           bool
	}{
		{name: "started owned new helper", kind: "started", want: 25, complete: true},
		{name: "completed alone is not spawn", kind: "completed", want: 20, unverified: true},
		{name: "unknown kind is not spawn", kind: "unknown", want: 20, unverified: true},
		{name: "old turn cannot grant current spawn", kind: "started", turn: "old-root", want: 20, unverified: true},
		{name: "other thread cannot grant current spawn", kind: "started", thread: "unrelated-root", want: 20, unverified: true},
		{name: "started missing child remains unverified", kind: "started", missing: true, want: 20, unverified: true},
		{name: "started unrelated metadata remains unverified", kind: "started", unrelated: true, want: 20, unverified: true},
		{name: "started unfinished child remains unverified", kind: "started", unfinished: true, want: 25, unverified: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			thread, turn := test.thread, test.turn
			if thread == "" {
				thread = "root"
			}
			if turn == "" {
				turn = "new-root"
			}
			appendUsageEvents(t, root,
				usageEvent("task_started", "new-root"),
				nativeActivityEvent(25, thread, turn, test.kind, "child"),
				usageRecord("root", "new-root", "r2", 20, 0, 0),
				nativeActivityEvent(35, thread, turn, "completed", "child"),
				usageEvent("task_complete", "new-root"))
			if !test.missing {
				parent := "root"
				if test.unrelated {
					parent = "unrelated-root"
				}
				childEvents := []string{usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "c1", 5, 0, 0)}
				if !test.unfinished {
					childEvents = append(childEvents, usageEvent("task_complete", "child-turn"))
				}
				child := writeUsageRolloutBoundary(t, home, "child", parent, nil, childEvents...)
				if test.unrelated {
					data, err := os.ReadFile(child)
					if err != nil {
						t.Fatal(err)
					}
					data = []byte(strings.Replace(string(data), `"session_id":"root"`, `"session_id":"unrelated-root"`, 1))
					if err := os.WriteFile(child, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			// No legacy stdout receiver UUID is invented. Discovery must come
			// from the current owned rollout's observed native activity shape.
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil, nil)
			if got != (usage.Tokens{Input: test.want}) || complete != test.complete || errors.Is(err, errUnverifiedChildren) != test.unverified || (!test.unverified && err != nil) {
				t.Fatalf("native activity accounting: tokens=%+v complete=%t error=%v; want input=%d complete=%t unverified=%t", got, complete, err, test.want, test.complete, test.unverified)
			}
		})
	}
}

func TestUsageSessionNativeInheritedActivityDoesNotGrantGrandchild(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root,
		usageEvent("task_started", "new-root"),
		nativeActivityEvent(25, "root", "new-root", "started", "child"),
		usageRecord("root", "new-root", "r2", 20, 0, 0),
		usageEvent("task_complete", "new-root"))
	boundary := int64(9)
	childEvents := []string{nativeActivityEvent(1, "child", "inherited-turn", "started", "grandchild")}
	childEvents = append(childEvents, completedUsageTurn("child", "child-turn", "c1", 5)...)
	writeUsageRolloutBoundary(t, home, "child", "root", &boundary, childEvents...)
	writeUsageRolloutBoundary(t, home, "grandchild", "child", nil, completedUsageTurn("grandchild", "grandchild-turn", "g1", 7)...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil, nil)
	if got != (usage.Tokens{Input: 25}) || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("inherited native activity certified unverified grandchild work: %+v,%t,%v", got, complete, err)
	}
}

func TestUsageSessionNativeActivityInitialCallStillCountsOwnedHelper(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), nativeActivityEvent(25, "root", "root-turn", "started", "child"), usageRecord("root", "root-turn", "r1", 20, 0, 0), nativeActivityEvent(35, "root", "root-turn", "completed", "child"), usageEvent("task_complete", "root-turn"))
	writeUsageRolloutBoundary(t, home, "child", "root", nil, completedUsageTurn("child", "child-turn", "c1", 5)...)
	var session usageSession
	session.begin(ctx, home, "", time.Now())
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil, nil)
	if got != (usage.Tokens{Input: 25}) || !complete || err != nil {
		t.Fatalf("initial native activity: %+v,%t,%v", got, complete, err)
	}
}
