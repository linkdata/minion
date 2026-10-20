package codexcli

import (
	"context"
	"encoding/json"
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

func appendUsageEvents(t *testing.T, path string, events ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(strings.Join(events, "\n") + "\n")
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
}

func completedUsageTurn(thread, turn, response string, input int64) []string {
	return []string{usageEvent("task_started", turn), usageRecord(thread, turn, response, input, 0, 0), usageEvent("task_complete", turn)}
}

func TestUsageSessionDoesNotRecountPreviousTurnsOrUnreferencedHelpers(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
	writeUsageRollout(t, home, "old-child", "root", completedUsageTurn("old-child", "old-child-turn", "c1", 40)...)
	var session usageSession
	session.begin(ctx, home, "", time.Now())
	if got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 100}, []string{"old-child"}, nil); got.Input != 140 || !complete || err != nil {
		t.Fatalf("initial = %+v, %v, %v", got, complete, err)
	}
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
	if got != (usage.Tokens{Input: 20}) || !complete || err != nil {
		t.Fatalf("resumed = %+v, %v, %v", got, complete, err)
	}
	if session.before != nil || session.known["root"].path != root {
		t.Fatalf("retained baseline or lost root ownership: before=%v known=%v", session.before != nil, session.known["root"].path)
	}
}

func TestUsageSessionUnreferencedHelperWithoutByteProofFailsClosed(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
	writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", 40)...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	// A damaged child baseline cannot prove that an unreferenced helper stayed idle.
	session.before["child"].prefixKnown = false
	session.before["child"].valid = false
	appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
	if got != (usage.Tokens{Input: 20}) || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("unverified historical helper = %+v, complete=%t, err=%v", got, complete, err)
	}
}

func TestUsageSessionSubtractsSnapshotBaselineAndLateOldRecords(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", append(completedUsageTurn("root", "old", "old-response", 100), usageSnapshot(22, 100, 0, 0))...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root,
		usageRecord("root", "old", "late-old-response", 7, 0, 0),
		usageRecord("root", "old", "late-old-response", 7, 0, 0),
		usageEvent("task_complete", "old"),
		usageEvent("task_started", "new"), usageRecord("root", "new", "new-response", 20, 0, 0),
		usageSnapshot(25, 127, 0, 0), usageEvent("task_complete", "new"))
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 127}, nil, nil)
	if got != (usage.Tokens{Input: 20}) || complete || err != nil {
		t.Fatalf("resumed = %+v, %v, %v", got, complete, err)
	}
}

func TestUsageSessionPriorHelperNeedsANewCompletedTurn(t *testing.T) {
	for _, state := range []string{"unchanged", "started", "aborted", "complete"} {
		t.Run(state, func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
			child := writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", 40)...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
			if state != "unchanged" {
				appendUsageEvents(t, child, usageEvent("task_started", "new-child"), usageRecord("child", "new-child", "c2", 5, 0, 0))
				if state == "complete" {
					appendUsageEvents(t, child, usageEvent("task_complete", "new-child"))
				}
				if state == "aborted" {
					appendUsageEvents(t, child, usageEvent("turn_aborted", "new-child"))
				}
			}
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, []string{"child"}, nil)
			want := usage.Tokens{Input: 25}
			if state == "unchanged" {
				want = usage.Tokens{Input: 20}
			}
			if state == "complete" {
				if got != want || !complete || err != nil {
					t.Fatalf("completed = %+v, %v, %v", got, complete, err)
				}
			} else if got != want || !errors.Is(err, errUnverifiedChildren) || complete {
				t.Fatalf("unsafe helper accepted: %+v, %v, %v", got, complete, err)
			}
		})
	}
}

func TestUsageSessionOnlyCountsNewHelpersWithCurrentSpawnProof(t *testing.T) {
	for _, spawn := range []bool{false, true} {
		t.Run(map[bool]string{false: "ambiguous-reference", true: "current-spawn"}[spawn], func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", 100)...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			events := []string{usageEvent("task_started", "new"), usageRecord("root", "new", "r2", 20, 0, 0)}
			if spawn {
				events = append(events, `{"ordinal":25,"type":"event_msg","payload":{"type":"collab_agent_spawn_end","turn_id":"new","new_thread_id":"new-child"}}`)
			}
			events = append(events, usageEvent("task_complete", "new"))
			appendUsageEvents(t, root, events...)
			writeUsageRollout(t, home, "new-child", "root", completedUsageTurn("new-child", "child-turn", "c1", 5)...)
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, []string{"new-child"}, nil)
			if spawn {
				if got.Input != 25 || !complete || err != nil {
					t.Fatalf("spawn = %+v, %v, %v", got, complete, err)
				}
			} else if got.Input != 20 || complete || !errors.Is(err, errUnverifiedChildren) {
				t.Fatalf("unknown history accepted: %+v, %v, %v", got, complete, err)
			}
		})
	}
}

func TestUsageSessionNoTurnSpawnOnResumeCannotCertifyLateOldHelper(t *testing.T) {
	for _, test := range []struct{ name, event string }{
		{"legacy spawn", `{"ordinal":25,"type":"event_msg","payload":{"type":"collab_agent_spawn_end","new_thread_id":"child"}}`},
		{"native activity", `{"ordinal":25,"type":"event_msg","payload":{"type":"item_completed","thread_id":"root","item":{"type":"SubAgentActivity","kind":"started","agent_thread_id":"child"}}}`},
		{"spawn tool call", `{"ordinal":25,"type":"event_msg","payload":{"type":"item_completed","thread_id":"root","item":{"type":"CollabAgentToolCall","tool":"spawn_agent","status":"completed","receiver_thread_ids":["child"]}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			appendUsageEvents(t, root, usageEvent("task_started", "new-root"), usageRecord("root", "new-root", "r2", 20, 0, 0), test.event, usageEvent("task_complete", "new-root"))
			writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", 5)...)
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
			if got != (usage.Tokens{Input: 20}) || complete || !errors.Is(err, errUnverifiedChildren) {
				t.Fatalf("unattributed late helper was certified: tokens=%+v complete=%t err=%v", got, complete, err)
			}
		})
		t.Run(test.name+" fresh", func(t *testing.T) {
			path := writeUsageRollout(t, t.TempDir(), "root", "", usageEvent("task_started", "new-root"), test.event,
				usageRecord("root", "new-root", "r1", 20, 0, 0), usageEvent("task_complete", "new-root"))
			budget := int64(maxUsageScanBytes)
			spawned := map[string]bool{}
			got, complete, children, err := readRolloutUsageFrom(context.Background(), rolloutMeta{ID: "root", path: path}, "root", &budget, rolloutUsageMode{spawned: spawned})
			if got != (usage.Tokens{Input: 20}) || !complete || err != nil || !spawned["child"] || len(children) != 1 || children[0] != "child" {
				t.Fatalf("fresh no-turn spawn lost: tokens=%+v complete=%t children=%v spawned=%v err=%v", got, complete, children, spawned, err)
			}
		})
	}
}

func TestUsageSessionCurrentStdoutSpawnCertifiesNoTurnHelper(t *testing.T) {
	for _, activity := range []struct{ name, event string }{
		{"stdout only", ""},
		{"legacy spawn", `{"ordinal":25,"type":"event_msg","payload":{"type":"collab_agent_spawn_end","new_thread_id":"child"}}`},
		{"native activity", `{"ordinal":25,"type":"event_msg","payload":{"type":"item_completed","thread_id":"root","item":{"type":"SubAgentActivity","kind":"started","agent_thread_id":"child"}}}`},
		{"spawn tool call", `{"ordinal":25,"type":"event_msg","payload":{"type":"item_completed","thread_id":"root","item":{"type":"CollabAgentToolCall","tool":"spawn_agent","status":"completed","receiver_thread_ids":["child"]}}}`},
	} {
		for _, receipt := range []struct {
			name, kind, tool string
			proved           bool
		}{
			{"completed spawn", "item.completed", "spawn_agent", true},
			{"started spawn", "item.started", "spawn_agent", false},
			{"other tool", "item.completed", "send_input", false},
		} {
			t.Run(activity.name+"/"+receipt.name, func(t *testing.T) {
				ctx, home := context.Background(), t.TempDir()
				root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
				var session usageSession
				session.begin(ctx, home, "root", time.Now())
				events := []string{usageEvent("task_started", "new-root"), usageRecord("root", "new-root", "r2", 20, 0, 0)}
				if activity.event != "" {
					events = append(events, activity.event)
				}
				appendUsageEvents(t, root, append(events, usageEvent("task_complete", "new-root"))...)
				writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "child-turn", "c1", 5)...)
				var output jsonlOutput
				line := fmt.Sprintf(`{"type":%q,"item":{"type":"collab_tool_call","tool":%q,"receiver_thread_ids":["child"]}}`+"\n", receipt.kind, receipt.tool)
				_, _ = output.Write([]byte(line))
				got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, output.children, output.observationOnly, output.spawns)
				want := usage.Tokens{Input: 20}
				if receipt.proved {
					want.Input = 25
				}
				if got != want || complete != receipt.proved || receipt.proved && err != nil || !receipt.proved && !errors.Is(err, errUnverifiedChildren) {
					t.Fatalf("current spawn proof: tokens=%+v complete=%t err=%v want=%+v proved=%t", got, complete, err, want, receipt.proved)
				}
			})
		}
	}
}

func TestUsageSessionUnrelatedStdoutSpawnDoesNotCertifyLateOldNoTurn(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
	writeUsageRollout(t, home, "old-child", "root", completedUsageTurn("old-child", "old-turn", "c1", 5)...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, usageEvent("task_started", "new-root"), usageRecord("root", "new-root", "r2", 20, 0, 0),
		`{"ordinal":25,"type":"event_msg","payload":{"type":"item_completed","thread_id":"root","item":{"type":"SubAgentActivity","kind":"started","agent_thread_id":"old-child"}}}`,
		usageEvent("task_complete", "new-root"))
	writeUsageRollout(t, home, "new-child", "root", completedUsageTurn("new-child", "new-turn", "c2", 3)...)
	var output jsonlOutput
	_, _ = output.Write([]byte(`{"type":"item.completed","item":{"type":"collab_tool_call","tool":"spawn_agent","receiver_thread_ids":["new-child"]}}` + "\n"))
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, output.children, output.observationOnly, output.spawns)
	if got != (usage.Tokens{Input: 23}) || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("unrelated receipt certified late old helper: tokens=%+v complete=%t err=%v", got, complete, err)
	}
}

func TestUsageSessionKnownPathSurvivesOldDateAndMtime(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", 100)...)
	var session usageSession
	session.begin(ctx, home, "", time.Now())
	_, _, _ = session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 100}, nil, nil)
	future := time.Now().AddDate(0, 0, 5)
	session.begin(ctx, home, "root", future)
	appendUsageEvents(t, root, completedUsageTurn("root", "new", "r2", 20)...)
	old := time.Now().AddDate(0, 0, -10)
	if err := os.Chtimes(root, old, old); err != nil {
		t.Fatal(err)
	}
	got, complete, err := session.collectUsage(ctx, home, "root", future, nil, nil, nil)
	if got.Input != 20 || !complete || err != nil {
		t.Fatalf("aged = %+v, %v, %v", got, complete, err)
	}
}

func TestUsageSessionChangedOrIncompletePrefixDoesNotRecountHistory(t *testing.T) {
	for _, damage := range []string{"rewrite", "truncate", "partial-baseline"} {
		t.Run(damage, func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", 100)...)
			if damage == "partial-baseline" {
				f, err := os.OpenFile(root, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, e := f.WriteString(`{"type":`)
				if err := errors.Join(e, f.Close()); err != nil {
					t.Fatal(err)
				}
			}
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			if damage == "truncate" {
				if err := os.Truncate(root, 0); err != nil {
					t.Fatal(err)
				}
			}
			if damage == "rewrite" {
				data, err := os.ReadFile(root)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(root, []byte(strings.Replace(string(data), "r1", "xx", 1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			appendUsageEvents(t, root, completedUsageTurn("root", "new", "r2", 20)...)
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
			if got.Input != 0 || complete || err != nil {
				t.Fatalf("damaged prefix = %+v, %v, %v (%s)", got, complete, err, filepath.Base(root))
			}
		})
	}
}

func TestUsageSessionDamagedRootCannotVerifyFreshHelper(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", 100)...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	data, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	damaged := strings.Replace(string(data), "r1", "xx", 1)
	if damaged == string(data) {
		t.Fatal("old response missing from root fixture")
	}
	if err := os.WriteFile(root, []byte(damaged), 0600); err != nil {
		t.Fatal(err)
	}
	appendUsageEvents(t, root,
		usageEvent("task_started", "new"),
		usageRecord("root", "new", "r2", 20, 0, 0),
		`{"ordinal":25,"type":"event_msg","payload":{"type":"collab_agent_spawn_end","turn_id":"new","new_thread_id":"child"}}`,
		usageEvent("task_complete", "new"),
	)
	writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "child-turn", "c1", 5)...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, []string{"child"}, nil)
	if got != (usage.Tokens{}) || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("damaged root accepted fresh helper: tokens=%+v complete=%t err=%v", got, complete, err)
	}
}

func TestUsageSessionSnapshotOnlyDeltaAndCumulativeStdout(t *testing.T) {
	for _, fallback := range []int64{20, 120} {
		t.Run(map[int64]string{20: "scoped-stdout", 120: "cumulative-stdout"}[fallback], func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", usageEvent("task_started", "old"), usageSnapshot(21, 100, 0, 0), usageEvent("task_complete", "old"))
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			appendUsageEvents(t, root, usageEvent("task_started", "new"), usageSnapshot(24, 120, 0, 0), usageEvent("task_complete", "new"))
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: fallback}, nil, nil)
			if got.Input != 20 || complete != (fallback == 120) || err != nil {
				t.Fatalf("snapshot delta = %+v, %v, %v", got, complete, err)
			}
		})
	}
}

func TestUsageSessionNoCurrentSnapshotChecksCumulativeStdout(t *testing.T) {
	for _, test := range []struct {
		name              string
		baseline, current int64
		stdout            int64
		complete          bool
	}{
		{"matching", 100, 20, 120, true},
		{"missing record", 100, 20, 127, false},
		{"scoped stdout", 100, 20, 20, false},
		{"sum overflow", math.MaxInt64 - 10, 20, 20, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", append(completedUsageTurn("root", "old", "r1", test.baseline), usageSnapshot(22, test.baseline, 0, 0))...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			appendUsageEvents(t, root, completedUsageTurn("root", "new", "r2", test.current)...)
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: test.stdout}, nil, nil)
			if got != (usage.Tokens{Input: test.current}) || complete != test.complete || err != nil {
				t.Fatalf("resumed delta = %+v, complete=%t, err=%v", got, complete, err)
			}
		})
	}
}

func TestUsageSessionSnapshotResetKeepsOnlyAttributedNewResponses(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", append(completedUsageTurn("root", "old", "r1", 100), usageSnapshot(21, 100, 0, 0))...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, append(completedUsageTurn("root", "new", "r2", 20), usageSnapshot(25, 20, 0, 0))...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil, nil)
	if got.Input != 20 || complete || err != nil {
		t.Fatalf("reset = %+v, %v, %v", got, complete, err)
	}
}

func TestUsageSessionLateOldTurnNeverCompletesCurrentHelper(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
	child := writeUsageRollout(t, home, "child", "root", usageEvent("task_started", "old-child"), usageRecord("child", "old-child", "c1", 40, 0, 0))
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
	appendUsageEvents(t, child, usageRecord("child", "old-child", "late-c1", 5, 0, 0), usageEvent("task_complete", "old-child"))
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, []string{"child"}, nil)
	if got.Input != 20 || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("late old helper = %+v, %v, %v", got, complete, err)
	}
}

func TestUsageSessionBudgetAndCancellationKeepHelpersUnverified(t *testing.T) {
	for _, mode := range []string{"prefix bound", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
			child := writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", 40)...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
			appendUsageEvents(t, child, completedUsageTurn("child", "new-child", "c2", 5)...)
			if mode == "prefix bound" {
				session.before["child"].size = maxUsageRolloutBytes + 1
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, []string{"child"}, nil)
			want := usage.Tokens{}
			if mode == "prefix bound" {
				want.Input = 20
			}
			if got != want || complete || !errors.Is(err, errUnverifiedChildren) {
				t.Fatalf("limited = %+v, %v, %v; want %+v", got, complete, err, want)
			}
		})
	}
}

func TestUsageSessionUnfinishedBaselineCannotCertifySnapshotDelta(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", usageEvent("task_started", "old"), usageSnapshot(21, 100, 0, 0))
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, usageEvent("task_complete", "old"), usageEvent("task_started", "new"), usageSnapshot(24, 127, 0, 0), usageEvent("task_complete", "new"))
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 27}, nil, nil)
	if got.Input != 0 || complete || err != nil {
		t.Fatalf("unfinished baseline = %+v, %v, %v", got, complete, err)
	}
}

func TestUsageSessionUnreadableRootDoesNotAttributeResumedStdout(t *testing.T) {
	for _, test := range []struct {
		name   string
		events []string
		stdout int64
	}{
		// The 27-token stdout increase can include late old-turn work (7) and current work (20).
		{"trusted snapshot with ambiguous increase", append(completedUsageTurn("root", "old", "r1", 100), usageSnapshot(22, 100, 0, 0)), 127},
		{"no snapshot", completedUsageTurn("root", "old", "r1", 100), 127},
		{"unfinished baseline", []string{usageEvent("task_started", "old"), usageRecord("root", "old", "r1", 100, 0, 0), usageSnapshot(22, 100, 0, 0)}, 127},
		{"counter below baseline", append(completedUsageTurn("root", "old", "r1", 100), usageSnapshot(22, 100, 0, 0)), 90},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			writeUsageRollout(t, home, "root", "", test.events...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			if boundary := session.before["root"]; boundary == nil || !boundary.valid || !boundary.prefixKnown {
				t.Fatalf("invalid baseline fixture: %+v", boundary)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			got, complete, err := session.collectUsage(cancelled, home, "root", time.Now(), &usage.Tokens{Input: test.stdout}, nil, nil)
			if got != (usage.Tokens{}) || complete || err != nil {
				t.Fatalf("unreadable root = %+v, complete=%t, err=%v; want zero incomplete", got, complete, err)
			}
		})
	}
}

func TestUsageSessionBaselineFileCap(t *testing.T) {
	home := t.TempDir()
	path := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", 100)...)
	session := usageSession{known: map[string]rolloutMeta{"root": {ID: "root", path: path}}}
	for i := range maxUsageFiles {
		id := fmt.Sprintf("child-%04d", i)
		session.known[id] = rolloutMeta{ID: id, ParentID: "root", path: path}
	}
	session.begin(context.Background(), home, "root", time.Now())
	if len(session.before) != maxUsageFiles || session.before[fmt.Sprintf("child-%04d", maxUsageFiles-1)] != nil || session.baselineComplete {
		t.Fatalf("baseline cap = %d histories, complete=%t", len(session.before), session.baselineComplete)
	}
}

func TestUsageSessionDuplicateResponseAcrossOldAndNewTurnsIsIncomplete(t *testing.T) {
	oldRecord := usageRecord("root", "old", "shared", 5, 0, 0)
	newRecord := usageRecord("root", "new", "shared", 5, 0, 0)
	for _, test := range []struct {
		name    string
		records []string
		want    int64
	}{
		{"old first", []string{oldRecord, newRecord}, 0},
		{"new first", []string{newRecord, oldRecord}, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", 100)...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			appendUsageEvents(t, root, append(append([]string{usageEvent("task_started", "new")}, test.records...), usageEvent("task_complete", "new"))...)
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
			if got.Input != test.want || complete || err != nil {
				t.Fatalf("cross-turn duplicate = %+v, %v, %v; want input %d, incomplete", got, complete, err, test.want)
			}
		})
	}
}

func TestUsageSessionHistoricalHelpersDoNotConsumeCurrentAgentSlots(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
	var last string
	for i := range maxUsageAgents + 2 {
		id := fmt.Sprintf("old-child-%03d", i)
		last = writeUsageRollout(t, home, id, "root", completedUsageTurn(id, "old-child", id+"-r1", 1)...)
	}
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
	id := fmt.Sprintf("old-child-%03d", maxUsageAgents+1)
	appendUsageEvents(t, last, completedUsageTurn(id, "new-child", id+"-r2", 5)...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, []string{id}, nil)
	if got.Input != 25 || !complete || err != nil {
		t.Fatalf("historical slots = %+v, %v, %v", got, complete, err)
	}
}

func TestUsageSessionSpawnProvenanceIsBounded(t *testing.T) {
	home := t.TempDir()
	ids := make([]string, maxCodexChildren+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("child-%d", i)
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	path := writeUsageRollout(t, home, "root", "", usageEvent("task_started", "new"), fmt.Sprintf(`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"new","item":{"type":"CollabAgentToolCall","tool":"spawn_agent","status":"completed","receiver_thread_ids":%s}}}`, raw), usageEvent("task_complete", "new"))
	budget := int64(maxUsageScanBytes)
	spawned := map[string]bool{}
	_, complete, _, err := readRolloutUsageFrom(context.Background(), rolloutMeta{ID: "root", path: path}, "root", &budget, rolloutUsageMode{spawned: spawned})
	if len(spawned) != maxCodexChildren || complete || !errors.Is(err, aicli.ErrOutputLimit) {
		t.Fatalf("spawn bound = %d, %v, %v", len(spawned), complete, err)
	}
}

// These root and child counters match the owned native104 basic/helper callsets.
// Exec resume reports a root-lifetime stdout total; Result remains per invocation.
func TestUsageSessionNativeCumulativeStdoutPreservesCompleteDeltas(t *testing.T) {
	for _, helper := range []bool{false, true} {
		for _, mismatch := range []bool{false, true} {
			t.Run(fmt.Sprintf("helper=%t/mismatch=%t", helper, mismatch), func(t *testing.T) {
				ctx, home := context.Background(), t.TempDir()
				oldRoot := usage.Tokens{Input: 39386, CachedInput: 11264, Output: 372}
				newRoot := usage.Tokens{Input: 49917, CachedInput: 45824, Output: 367}
				if helper {
					oldRoot = usage.Tokens{Input: 66390, CachedInput: 38144, Output: 384}
					newRoot = usage.Tokens{Input: 82884, CachedInput: 78720, Output: 289}
				}
				turn := func(thread, id, response string, tokens usage.Tokens) []string {
					return []string{usageEvent("task_started", id), usageRecord(thread, id, response, tokens.Input, tokens.CachedInput, tokens.Output), usageEvent("task_complete", id)}
				}
				root := writeUsageRollout(t, home, "root", "", append(turn("root", "old-root", "r1", oldRoot), usageSnapshot(25, oldRoot.Input, oldRoot.CachedInput, oldRoot.Output))...)
				var child string
				oldChild := usage.Tokens{Input: 24164, CachedInput: 10752, Output: 159}
				newChild := usage.Tokens{Input: 13866, CachedInput: 13056, Output: 28}
				if helper {
					child = writeUsageRollout(t, home, "child", "root", append(turn("child", "old-child", "c1", oldChild), usageSnapshot(25, oldChild.Input, oldChild.CachedInput, oldChild.Output))...)
				}
				var session usageSession
				session.begin(ctx, home, "root", time.Now())
				cumulative, added := oldRoot.AddChecked(newRoot)
				if !added {
					t.Fatal("invalid root fixture")
				}
				appendUsageEvents(t, root, append(turn("root", "new-root", "r2", newRoot), usageSnapshot(30, cumulative.Input, cumulative.CachedInput, cumulative.Output))...)
				var expected []string
				want := newRoot
				if helper {
					childTotal, added := oldChild.AddChecked(newChild)
					if !added {
						t.Fatal("invalid child fixture")
					}
					appendUsageEvents(t, child, append(turn("child", "new-child", "c2", newChild), usageSnapshot(30, childTotal.Input, childTotal.CachedInput, childTotal.Output))...)
					expected = []string{"child"}
					want = usage.Tokens{Input: 96750, CachedInput: 91776, Output: 317}
				}
				if mismatch {
					cumulative.Input++
				}
				got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &cumulative, expected, nil)
				if got != want || complete == mismatch || err != nil {
					t.Fatalf("native-shaped delta = %+v complete=%t error=%v, want %+v", got, complete, err, want)
				}
			})
		}
	}
}

func TestUsageSessionLateInheritedUnknownOwnerDoesNotUseSnapshotAsCurrentUsage(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
	start := int64(9)
	child := writeUsageRolloutBoundary(t, home, "child", "root", &start,
		usageSnapshot(8, 100, 0, 0),
		usageEventAt(9, "task_started", "old-child"),
		usageRecordAt(10, "child", "old-child", "c1", 10, 0, 0),
		usageSnapshot(12, 110, 0, 0),
		usageEventAt(13, "task_complete", "old-child"))
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	if before := session.before["child"]; before == nil || !before.valid || !before.hasSnapshot || before.snapshot.Input != 110 {
		t.Fatalf("invalid child snapshot baseline: %+v", before)
	}
	appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
	appendUsageEvents(t, child,
		usageRecordAt(8, "", "inherited", "late-inherited", 20, 0, 0),
		usageEventAt(24, "task_started", "new-child"),
		usageSnapshot(25, 130, 0, 0),
		usageEventAt(26, "task_complete", "new-child"))
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, []string{"child"}, nil)
	if got != (usage.Tokens{Input: 20}) || complete || err != nil {
		t.Fatalf("late inherited ownerless record certified snapshot delta: %+v, complete=%t, err=%v", got, complete, err)
	}
}

func TestUsageSessionObservationOnlyHonorsToolAndHelperLifecycle(t *testing.T) {
	for _, test := range []struct {
		name, tools, before, after string
		accepted                   bool
	}{
		{"wait finished", "wait", "complete", "", true},
		{"close finished", "close_agent", "complete", "", true},
		{"input finished", "send_input", "complete", "", false},
		{"spawn reference", "spawn_agent", "complete", "", false},
		{"unknown reference", "unknown", "complete", "", false},
		{"input then wait", "send_input,wait", "complete", "", false},
		{"wait then input", "wait,send_input", "complete", "", false},
		{"wait unfinished", "wait", "started", "", false},
		{"wait unfinished then new completed", "wait", "started", "complete", false},
		{"close aborted", "close_agent", "aborted", "", false},
		{"wait aborted then new completed", "wait", "aborted", "complete", true},
		{"wait new unfinished", "wait", "complete", "started", false},
		{"wait new completed", "wait", "complete", "complete", true},
		{"wait invalid baseline", "wait", "invalid", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
			events := []string{usageEvent("task_started", "old-child"), usageRecord("child", "old-child", "c1", 40, 0, 0)}
			if test.before == "complete" || test.before == "invalid" {
				events = append(events, usageEvent("task_complete", "old-child"))
			}
			if test.before == "aborted" {
				events = append(events, usageEvent("turn_aborted", "old-child"))
			}
			if test.before == "invalid" {
				events = append(events, "{")
			}
			child := writeUsageRollout(t, home, "child", "root", events...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
			if test.after == "started" {
				appendUsageEvents(t, child, usageEvent("task_started", "new-child"))
			} else if test.after == "complete" {
				appendUsageEvents(t, child, completedUsageTurn("child", "new-child", "c2", 5)...)
			}
			var output jsonlOutput
			for _, tool := range strings.Split(test.tools, ",") {
				_, _ = output.Write([]byte(fmt.Sprintf(`{"type":"item.completed","item":{"type":"collab_tool_call","tool":%q,"receiver_thread_ids":["child"]}}`+"\n", tool)))
			}
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, output.children, output.observationOnly)
			want := usage.Tokens{Input: 20}
			if test.after == "complete" {
				want.Input = 25
			}
			if test.accepted {
				if got != want || !complete || err != nil {
					t.Fatalf("finished observation = %+v,%v,%v", got, complete, err)
				}
			} else if got != want || complete || !errors.Is(err, errUnverifiedChildren) {
				t.Fatalf("unverified helper accepted: %+v,%v,%v", got, complete, err)
			}
		})
	}
}

func TestUsageBoundaryEarlyReadFailuresAreInvalid(t *testing.T) {
	// Zero budget documents fail-closed output; the bounded reader also rejects it without the early guard.
	for _, cause := range []string{"budget", "cancel", "open"} {
		t.Run(cause, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			budget := int64(maxUsageScanBytes)
			if cause == "budget" {
				budget = 0
			}
			if cause == "cancel" {
				cancel()
			}
			path := writeUsageRollout(t, t.TempDir(), "root", "", completedUsageTurn("root", "turn", "response", 10)...)
			if cause == "open" {
				path = filepath.Join(t.TempDir(), "missing")
			}
			capture := &usageBoundary{valid: true, turns: map[string]bool{}}
			_, complete, _, err := readRolloutUsageFrom(ctx, rolloutMeta{ID: "root", path: path}, "root", &budget, rolloutUsageMode{capture: capture})
			if capture.valid || capture.prefixKnown || complete || err != nil || cause == "cancel" && budget != maxUsageScanBytes {
				t.Fatalf("failed read left valid boundary or debited canceled budget: %+v complete=%t budget=%d err=%v", capture, complete, budget, err)
			}
		})
	}
}

func TestCollectUsageMissingRootKeepsDirectParentOwnership(t *testing.T) {
	home := t.TempDir()
	path := writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "own", "response", 5)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Native source-parent provenance remains enough without session_id or a root header.
	data = []byte(strings.Replace(string(data), `"session_id":"root",`, "", 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, complete, err := (&usageSession{}).collectUsage(context.Background(), home, "root", time.Now(), &usage.Tokens{Input: 20}, []string{"child"}, nil)
	if got != (usage.Tokens{Input: 25}) || complete || err != nil {
		t.Fatalf("missing-root ownership changed: %+v,%v,%v", got, complete, err)
	}
}
