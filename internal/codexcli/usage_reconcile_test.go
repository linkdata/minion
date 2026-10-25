package codexcli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/usage"
)

func TestUsageSessionUnvisitedRolloutsRequireUnchangedOwnedBaseline(t *testing.T) {
	for _, test := range []struct {
		name, change string
		owned        bool
	}{
		{"new owned", "new", true},
		{"grown owned", "grown", true},
		{"rewritten owned at same size", "rewritten", true},
		{"rewritten malformed owned at same size", "rewritten malformed", true},
		{"unchanged owned", "unchanged", true},
		{"new unrelated", "new", false},
		{"grown unrelated", "grown", false},
		{"rewritten unrelated at same size", "rewritten", false},
		{"unchanged unrelated", "unchanged", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, home := t.Context(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
			writeChild := func() string {
				parent := "root"
				if !test.owned {
					parent = "other-root"
				}
				input := int64(40)
				if test.change == "rewritten malformed" {
					input = -1
				}
				child := writeUsageRollout(t, home, "child", parent, completedUsageTurn("child", "old-child", "c1", input)...)
				if !test.owned {
					data, err := os.ReadFile(child)
					if err != nil {
						t.Fatal(err)
					}
					const selected = `"session_id":"root"`
					if !strings.Contains(string(data), selected) {
						t.Fatal("fixture has no root session identity to replace")
					}
					data = []byte(strings.Replace(string(data), selected, `"session_id":"other-root"`, 1))
					if err := os.WriteFile(child, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				return child
			}
			var child string
			if test.change != "new" {
				child = writeChild()
			}
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
			switch test.change {
			case "new":
				writeChild()
			case "grown":
				appendUsageEvents(t, child, completedUsageTurn("child", "new-child", "c2", 5)...)
			case "rewritten", "rewritten malformed":
				before, err := os.ReadFile(child)
				if err != nil {
					t.Fatal(err)
				}
				after := strings.ReplaceAll(string(before), "old-child", "new-child")
				if after == string(before) || len(after) != len(before) {
					t.Fatal("fixture rewrite must change content without changing size")
				}
				if err := os.WriteFile(child, []byte(after), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// There is no current root reference to this lifetime-owned
			// history. Do not invent current usage from its metadata alone.
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
			unverified := test.owned && test.change != "unchanged"
			if got != (usage.Tokens{Input: 20}) || complete == unverified || errors.Is(err, errUnverifiedChildren) != unverified || (!unverified && err != nil) {
				t.Fatalf("unvisited rollout: %+v complete=%t error=%v; want root-only usage, complete=%t unverified=%t", got, complete, err, !unverified, unverified)
			}
		})
	}
}

func TestUsageSessionObservationCannotCertifyUnchangedRoot(t *testing.T) {
	for _, tool := range []string{"wait", "close_agent"} {
		t.Run(tool, func(t *testing.T) {
			ctx, home := t.Context(), t.TempDir()
			writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			var output jsonlOutput
			_, _ = output.Write([]byte(fmt.Sprintf(`{"type":"item.completed","item":{"type":"collab_tool_call","tool":%q,"receiver_thread_ids":["root"]}}`+"\n", tool)))
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, output.children, output.observationOnly)
			if got != (usage.Tokens{}) || complete || err != nil {
				t.Fatalf("root observation bypassed current-turn proof: %+v complete=%t error=%v", got, complete, err)
			}
		})
	}
}

func TestUsageSessionMalformedHistoricalCountersDoNotPoisonResume(t *testing.T) {
	ctx, home := t.Context(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
	child := writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", -1)...)
	before, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	var session usageSession
	session.begin(ctx, home, "", time.Now())
	initial, initialComplete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 100}, []string{"child"}, nil)
	if initial != (usage.Tokens{Input: 100}) || initialComplete || err != nil {
		t.Fatalf("completed helper with unavailable counters must leave usable incomplete usage: %+v complete=%t error=%v", initial, initialComplete, err)
	}
	for round := 1; round <= 2; round++ {
		session.begin(ctx, home, "root", time.Now())
		boundary := session.before["child"]
		if boundary == nil || !boundary.valid || !boundary.counterGap || !boundary.counterResponses["c1"] || !boundary.completed || boundary.size != int64(len(before)) || boundary.hash != sha256.Sum256(before) {
			t.Fatalf("fixture must capture old attribution and exact bytes despite invalid counters: %+v", boundary)
		}
		appendUsageEvents(t, root, completedUsageTurn("root", fmt.Sprintf("new-root-%d", round), fmt.Sprintf("r%d", round+1), 20)...)
		got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
		if got != (usage.Tokens{Input: 20}) || !complete || err != nil {
			t.Fatalf("unchanged old counter gap poisoned resume%d: %+v complete=%t error=%v", round, got, complete, err)
		}
	}
	if after, err := os.ReadFile(child); err != nil || string(after) != string(before) {
		t.Fatalf("historical helper changed during fixture: %q error=%v", after, err)
	}
}

func TestUsageSessionCounterGapKeepsNewRootDelta(t *testing.T) {
	ctx, home := t.Context(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", append(completedUsageTurn("root", "old", "r1", -1), usageSnapshot(22, 100, 0, 0))...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	if before := session.before["root"]; before == nil || !before.valid || !before.counterGap || !before.counterResponses["r1"] || !before.prefixKnown {
		t.Fatalf("old root attribution lost: %+v", before)
	}
	appendUsageEvents(t, root, usageRecord("root", "old", "r1", 100, 0, 0), usageRecord("root", "old", "late-old", 7, 0, 0))
	appendUsageEvents(t, root, append(completedUsageTurn("root", "new", "r2", 20), usageSnapshot(30, 127, 0, 0))...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 127}, nil, nil)
	if got != (usage.Tokens{Input: 20}) || complete || err != nil {
		t.Fatalf("old counter correction or cumulative snapshot hid new root delta: %+v complete=%t err=%v", got, complete, err)
	}
}

func TestUsageSessionCounterGapSnapshotDetectsMissingNewResponse(t *testing.T) {
	ctx, home := t.Context(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", append(completedUsageTurn("root", "old", "r1", -1), usageSnapshot(22, 100, 0, 0))...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, append(completedUsageTurn("root", "new", "r2", 20), usageSnapshot(30, 140, 0, 0))...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 140}, nil, nil)
	if got != (usage.Tokens{Input: 20}) || complete || err != nil {
		t.Fatalf("old counter gap hid missing current response: %+v complete=%t err=%v", got, complete, err)
	}
}

func TestUsageSessionCounterGapNeedsTrustworthySnapshotBaseline(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  []string
	}{
		{"unfinished old turn", []string{usageEvent("task_started", "old"), usageRecord("root", "old", "r1", -1, 0, 0), usageSnapshot(22, 100, 0, 0)}},
		{"no old snapshot", completedUsageTurn("root", "old", "r1", -1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, home := t.Context(), t.TempDir()
			root := writeUsageRollout(t, home, "root", "", tc.old...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			if before := session.before["root"]; before == nil || !before.counterGap {
				t.Fatalf("fixture has no counter gap: %+v", before)
			}
			appendUsageEvents(t, root, append(completedUsageTurn("root", "new", "r2", 20), usageSnapshot(30, 120, 0, 0))...)
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 120}, nil, nil)
			if got != (usage.Tokens{Input: 20}) || complete || err != nil {
				t.Fatalf("untrusted snapshot baseline certified new delta: %+v complete=%t err=%v", got, complete, err)
			}
		})
	}
}

func TestUsageSessionCounterGapAllowsWaitOnCompletedHelper(t *testing.T) {
	ctx, home := t.Context(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
	writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", -1)...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
	var output jsonlOutput
	_, _ = output.Write([]byte(`{"type":"item.completed","item":{"type":"collab_tool_call","tool":"wait","receiver_thread_ids":["child"]}}` + "\n"))
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, output.children, output.observationOnly)
	if got != (usage.Tokens{Input: 20}) || !complete || err != nil {
		t.Fatalf("wait on unchanged completed helper refused: %+v complete=%t err=%v", got, complete, err)
	}
}

func TestUsageBoundaryCounterGapStillRequiresExactPrefix(t *testing.T) {
	ctx, home := t.Context(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", -1)...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	data, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), `"input_tokens":-1`, `"input_tokens":-2`, 1)
	if changed == string(data) || len(changed) != len(data) {
		t.Fatal("fixture must rewrite old counter without changing byte count")
	}
	if err := os.WriteFile(root, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	appendUsageEvents(t, root, completedUsageTurn("root", "new", "r2", 20)...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
	if got != (usage.Tokens{}) || complete || err != nil {
		t.Fatalf("rewritten old counter bypassed exact prefix: %+v complete=%t err=%v", got, complete, err)
	}
}

func TestUsageBoundaryCounterGapDoesNotExcuseMissingIdentity(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement string }{
		{"missing turn", `"turn_id":"turn"`, `"turn_id":""`},
		{"missing response", `"response_id":"response"`, `"response_id":""`},
		{"missing session", `"session_id":"root"`, `"session_id":""`},
		{"malformed payload", `"response_id":"response"`, `"response_id":42`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, home := t.Context(), t.TempDir()
			record := usageRecord("root", "turn", "response", -1, 0, 0)
			changed := strings.Replace(record, tc.old, tc.replacement, 1)
			if changed == record {
				t.Fatal("fixture did not change record")
			}
			writeUsageRollout(t, home, "root", "", usageEvent("task_started", "turn"), changed, usageEvent("task_complete", "turn"))
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			if before := session.before["root"]; before == nil || before.valid || !before.prefixKnown {
				t.Fatalf("invalid record identity or structure accepted: %+v", before)
			}
		})
	}
}

func TestUsageBoundaryIdentityRequiresAnUncappedPhysicalRead(t *testing.T) {
	ctx, home := t.Context(), t.TempDir()
	name := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "turn", "response", -1)...)
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, capped := range []bool{false, true} {
		label := "complete log"
		if capped {
			label = "capped log"
		}
		t.Run(label, func(t *testing.T) {
			budget := int64(len(data))
			if capped {
				budget--
			}
			capture := &usageBoundary{valid: true, turns: map[string]bool{}}
			_, complete, _, err := readRolloutUsageFrom(ctx, rolloutMeta{ID: "root", path: name}, "root", &budget, rolloutUsageMode{capture: capture})
			if capture.valid == capped || !capture.counterGap || complete || capture.prefixKnown == capped || err != nil {
				t.Fatalf("counter/byte validity conflated: %+v complete=%t capped=%t err=%v", capture, complete, capped, err)
			}
		})
	}
}

func TestUsageSessionAmbiguousHistoricalIdentityCannotCertifyAnUnchangedHelper(t *testing.T) {
	ctx, home := t.Context(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
	child := writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", 40)...)
	data, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := filepath.Join(filepath.Dir(child), "rollout-duplicate.jsonl")
	if err := os.WriteFile(duplicate, data, 0600); err != nil {
		t.Fatal(err)
	}
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	boundary := session.before["child"]
	if boundary == nil || boundary.valid || boundary.prefixKnown {
		t.Fatalf("ambiguous baseline gained usable identity: %+v", boundary)
	}
	// Current discovery is unique, but it cannot retroactively prove the old identity.
	if err := os.Remove(duplicate); err != nil {
		t.Fatal(err)
	}
	appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
	if got != (usage.Tokens{Input: 20}) || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("ambiguous old identity was accepted: %+v complete=%t error=%v", got, complete, err)
	}
}

func TestUsageSessionKnownHelperIdentityLossCannotDisappearFromReconcile(t *testing.T) {
	for _, timing := range []string{"before begin", "between begin and collect"} {
		for _, change := range []string{"unchanged", "removed", "unreadable", "malformed header", "reparented header", "changed parent only", "changed session only", "changed history boundary", "added history boundary", "removed history boundary", "changed header ID", "equivalent parent header"} {
			t.Run(timing+"/"+change, func(t *testing.T) {
				ctx, home := t.Context(), t.TempDir()
				root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
				child := writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", 40)...)
				if change == "added history boundary" {
					child = writeUsageRolloutBoundary(t, home, "child", "root", nil, completedUsageTurn("child", "old-child", "c1", 40)...)
				}
				t.Cleanup(func() {
					if err := os.Chmod(child, 0600); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Error(err)
					}
				})
				var session usageSession
				session.begin(ctx, home, "", time.Now())
				if got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 100}, []string{"child"}, nil); got != (usage.Tokens{Input: 140}) || !complete || err != nil {
					t.Fatalf("initial complete ownership was not established: %+v complete=%t error=%v", got, complete, err)
				}
				mutate := func() {
					t.Helper()
					switch change {
					case "removed":
						if err := os.Remove(child); err != nil {
							t.Fatal(err)
						}
					case "unreadable":
						if err := os.Chmod(child, 0000); err != nil {
							t.Fatal(err)
						}
						if file, err := os.Open(child); err == nil {
							_ = file.Close()
							t.Skip("file permissions do not restrict this test user")
						} else if !errors.Is(err, os.ErrPermission) {
							t.Fatal(err)
						}
					case "malformed header", "reparented header", "changed parent only", "changed session only", "changed history boundary", "added history boundary", "removed history boundary", "changed header ID", "equivalent parent header":
						data, err := os.ReadFile(child)
						if err != nil {
							t.Fatal(err)
						}
						_, body, found := strings.Cut(string(data), "\n")
						if !found {
							t.Fatal("fixture has no header boundary")
						}
						header := "{"
						switch change {
						case "reparented header":
							header = `{"type":"session_meta","payload":{"id":"child","session_id":"other-root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"other-root"}}},"subagent_history_start_ordinal":9}}`
						case "changed parent only":
							header = `{"type":"session_meta","payload":{"id":"child","session_id":"root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"other-root"}}},"subagent_history_start_ordinal":9}}`
						case "changed session only":
							header = `{"type":"session_meta","payload":{"id":"child","session_id":"other-root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"root"}}},"subagent_history_start_ordinal":9}}`
						case "changed history boundary":
							header = `{"type":"session_meta","payload":{"id":"child","session_id":"root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"root"}}},"subagent_history_start_ordinal":10}}`
						case "added history boundary":
							header = `{"type":"session_meta","payload":{"id":"child","session_id":"root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"root"}}},"subagent_history_start_ordinal":9}}`
						case "removed history boundary":
							header = `{"type":"session_meta","payload":{"id":"child","session_id":"root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"root"}}}}}`
						case "changed header ID":
							header = `{"type":"session_meta","payload":{"id":"replacement-child","session_id":"root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"root"}}},"subagent_history_start_ordinal":9}}`
						case "equivalent parent header":
							header = `{"type":"session_meta","payload":{"id":"child","session_id":"root","parent_thread_id":"root","source":"exec","subagent_history_start_ordinal":9}}`
						}
						if err := os.WriteFile(child, []byte(header+"\n"+body), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				if timing == "before begin" {
					mutate()
				}
				session.begin(ctx, home, "root", time.Now())
				if timing == "between begin and collect" {
					mutate()
				}
				appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
				got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
				unverified := change != "unchanged" && !(timing == "before begin" && change == "equivalent parent header")
				if got != (usage.Tokens{Input: 20}) || complete == unverified || errors.Is(err, errUnverifiedChildren) != unverified || (!unverified && err != nil) {
					t.Fatalf("known helper identity loss: %+v complete=%t error=%v; want complete=%t unverified=%t", got, complete, err, !unverified, unverified)
				}
			})
		}
	}
}

func TestUsageSessionHeaderAliasesDoNotGrowKnownOwnership(t *testing.T) {
	ctx, home := t.Context(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
	child := writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", 40)...)
	original, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	_, body, found := strings.Cut(string(original), "\n")
	if !found {
		t.Fatal("fixture has no header boundary")
	}
	var session usageSession
	session.begin(ctx, home, "", time.Now())
	if got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 100}, []string{"child"}, nil); got != (usage.Tokens{Input: 140}) || !complete || err != nil {
		t.Fatalf("initial complete ownership was not established: %+v complete=%t error=%v", got, complete, err)
	}
	for round := range 3 {
		alias := fmt.Sprintf("replacement-%d", round)
		header := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"session_id":"root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"root"}}},"subagent_history_start_ordinal":9}}`, alias)
		if err := os.WriteFile(child, []byte(header+"\n"+body), 0600); err != nil {
			t.Fatal(err)
		}
		session.begin(ctx, home, "root", time.Now())
		appendUsageEvents(t, root, completedUsageTurn("root", fmt.Sprintf("bad-turn-%d", round), fmt.Sprintf("bad-response-%d", round), 20)...)
		if got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil); got != (usage.Tokens{Input: 20}) || complete || !errors.Is(err, errUnverifiedChildren) {
			t.Fatalf("alias round %d: %+v complete=%t error=%v", round, got, complete, err)
		}
		if len(session.known) != 2 || session.known["child"].path != child || session.known[alias].ID != "" {
			t.Fatalf("alias round %d changed the bounded ownership set: %+v", round, session.known)
		}
		if err := os.WriteFile(child, original, 0600); err != nil {
			t.Fatal(err)
		}
		session.begin(ctx, home, "root", time.Now())
		appendUsageEvents(t, root, completedUsageTurn("root", fmt.Sprintf("good-turn-%d", round), fmt.Sprintf("good-response-%d", round), 20)...)
		if got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil); got != (usage.Tokens{Input: 20}) || !complete || err != nil {
			t.Fatalf("recovered round %d: %+v complete=%t error=%v", round, got, complete, err)
		}
		if len(session.known) != 2 {
			t.Fatalf("recovered round %d grew the ownership set to %d", round, len(session.known))
		}
	}
}

func TestUsageSessionCancellationDoesNotForgetKnownHelperIdentity(t *testing.T) {
	for _, timing := range []string{"before begin", "before collect"} {
		for _, change := range []string{"unchanged", "removed", "reparented header"} {
			t.Run(timing+"/"+change, func(t *testing.T) {
				ctx, home := t.Context(), t.TempDir()
				root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
				child := writeUsageRollout(t, home, "child", "root", completedUsageTurn("child", "old-child", "c1", 40)...)
				var session usageSession
				session.begin(ctx, home, "", time.Now())
				if got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 100}, []string{"child"}, nil); got != (usage.Tokens{Input: 140}) || !complete || err != nil {
					t.Fatalf("initial complete ownership was not established: %+v complete=%t error=%v", got, complete, err)
				}
				cancelled, cancel := context.WithCancel(ctx)
				defer cancel()
				if timing == "before begin" {
					cancel()
					session.begin(cancelled, home, "root", time.Now())
				} else {
					session.begin(ctx, home, "root", time.Now())
					cancel()
				}
				if _, complete, _ := session.collectUsage(cancelled, home, "root", time.Now(), nil, nil, nil); complete {
					t.Fatal("cancelled accounting was reported complete")
				}
				switch change {
				case "removed":
					if err := os.Remove(child); err != nil {
						t.Fatal(err)
					}
				case "reparented header":
					data, err := os.ReadFile(child)
					if err != nil {
						t.Fatal(err)
					}
					_, body, found := strings.Cut(string(data), "\n")
					if !found {
						t.Fatal("fixture has no header boundary")
					}
					header := `{"type":"session_meta","payload":{"id":"child","session_id":"other-root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"other-root"}}},"subagent_history_start_ordinal":9}}`
					if err := os.WriteFile(child, []byte(header+"\n"+body), 0600); err != nil {
						t.Fatal(err)
					}
				}
				session.begin(ctx, home, "root", time.Now())
				appendUsageEvents(t, root, completedUsageTurn("root", "new-root", "r2", 20)...)
				got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
				unverified := change != "unchanged"
				if got != (usage.Tokens{Input: 20}) || complete == unverified || errors.Is(err, errUnverifiedChildren) != unverified || (!unverified && err != nil) {
					t.Fatalf("cancelled call forgot a known identity: %+v complete=%t error=%v; want complete=%t unverified=%t", got, complete, err, !unverified, unverified)
				}
				if len(session.known) != 2 || session.known["child"].path != child {
					t.Fatalf("cancelled call lost bounded ownership: %+v", session.known)
				}
			})
		}
	}
}
