package codexcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 100}, []string{"old-child"}); got.Input != 140 || !complete || err != nil {
		t.Fatalf("initial = %+v, %v, %v", got, complete, err)
	}
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil)
	if got != (usage.Tokens{Input: 20}) || !complete || err != nil {
		t.Fatalf("resumed = %+v, %v, %v", got, complete, err)
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
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 127}, nil)
	if got != (usage.Tokens{Input: 20}) || !complete || err != nil {
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
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, []string{"child"})
			if state == "complete" {
				if got.Input != 25 || !complete || err != nil {
					t.Fatalf("completed = %+v, %v, %v", got, complete, err)
				}
			} else if !errors.Is(err, errUnverifiedChildren) || complete {
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
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, []string{"new-child"})
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

func TestUsageSessionKnownPathSurvivesOldDateAndMtime(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", 100)...)
	var session usageSession
	session.begin(ctx, home, "", time.Now())
	_, _, _ = session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 100}, nil)
	future := time.Now().AddDate(0, 0, 5)
	session.begin(ctx, home, "root", future)
	appendUsageEvents(t, root, completedUsageTurn("root", "new", "r2", 20)...)
	old := time.Now().AddDate(0, 0, -10)
	if err := os.Chtimes(root, old, old); err != nil {
		t.Fatal(err)
	}
	got, complete, err := session.collectUsage(ctx, home, "root", future, &usage.Tokens{Input: 20}, nil)
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
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil)
			if got.Input != 0 || complete || err != nil {
				t.Fatalf("damaged prefix = %+v, %v, %v (%s)", got, complete, err, filepath.Base(root))
			}
		})
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
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: fallback}, nil)
			if got.Input != 20 || complete != (fallback == 120) || err != nil {
				t.Fatalf("snapshot delta = %+v, %v, %v", got, complete, err)
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
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 120}, nil)
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
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, []string{"child"})
	if got.Input != 20 || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("late old helper = %+v, %v, %v", got, complete, err)
	}
}

func TestUsageSessionBudgetAndCancellationKeepHelpersUnverified(t *testing.T) {
	for _, mode := range []string{"budget", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, home := context.Background(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
			child := writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", 40)...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
			appendUsageEvents(t, child, completedUsageTurn("child", "new-child", "c2", 5)...)
			if mode == "budget" {
				session.budget = 1
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, []string{"child"})
			if got.Input != 0 || complete || !errors.Is(err, errUnverifiedChildren) {
				t.Fatalf("limited = %+v, %v, %v", got, complete, err)
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
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 27}, nil)
	if got.Input != 0 || complete || err != nil {
		t.Fatalf("unfinished baseline = %+v, %v, %v", got, complete, err)
	}
}

func TestUsageSessionDuplicateResponseAcrossOldAndNewTurnsIsIncomplete(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", 100)...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, usageRecord("root", "old", "shared", 5, 0, 0), usageEvent("task_started", "new"), usageRecord("root", "new", "shared", 5, 0, 0), usageEvent("task_complete", "new"))
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil)
	if got.Input != 0 || complete || err != nil {
		t.Fatalf("cross-turn duplicate = %+v, %v, %v", got, complete, err)
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
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, []string{id})
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
	_, complete, _, err := readRolloutUsageFrom(context.Background(), rolloutMeta{ID: "root", path: path}, "root", &budget, nil, nil, spawned)
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
				got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &cumulative, expected)
				if got != want || complete == mismatch || err != nil {
					t.Fatalf("native-shaped delta = %+v complete=%t error=%v, want %+v", got, complete, err, want)
				}
			})
		}
	}
}
