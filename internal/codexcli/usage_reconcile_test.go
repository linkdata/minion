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
			ctx, home := context.Background(), t.TempDir()
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
			// There is no current root/stdout reference to this lifetime-owned
			// history. Do not invent current usage from its metadata alone.
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil, nil)
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
			ctx, home := context.Background(), t.TempDir()
			writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old-root", "r1", 100)...)
			var session usageSession
			session.begin(ctx, home, "root", time.Now())
			var output jsonlOutput
			_, _ = output.Write([]byte(fmt.Sprintf(`{"type":"item.completed","item":{"type":"collab_tool_call","tool":%q,"receiver_thread_ids":["root"]}}`+"\n", tool)))
			got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, output.children, output.observationOnly)
			if got != (usage.Tokens{}) || complete || err != nil {
				t.Fatalf("root observation bypassed current-turn proof: %+v complete=%t error=%v", got, complete, err)
			}
		})
	}
}

func TestUsageSessionMalformedHistoricalCountersDoNotPoisonResume(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
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
		if boundary == nil || boundary.valid || !boundary.completed || boundary.size != int64(len(before)) || boundary.hash != sha256.Sum256(before) {
			t.Fatalf("fixture must capture exact bytes despite invalid old counters: %+v", boundary)
		}
		appendUsageEvents(t, root, completedUsageTurn("root", fmt.Sprintf("new-root-%d", round), fmt.Sprintf("r%d", round+1), 20)...)
		got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil, nil)
		if got != (usage.Tokens{Input: 20}) || !complete || err != nil {
			t.Fatalf("unchanged old counter gap poisoned resume%d: %+v complete=%t error=%v", round, got, complete, err)
		}
	}
	if after, err := os.ReadFile(child); err != nil || string(after) != string(before) {
		t.Fatalf("historical helper changed during fixture: %q error=%v", after, err)
	}
}

func TestUsageBoundaryIdentityRequiresAnUncappedPhysicalRead(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
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
			_, complete, _, _ := readRolloutUsageFrom(ctx, rolloutMeta{ID: "root", path: name}, "root", &budget, nil, capture, nil, false)
			if capture.valid || complete || capture.prefixKnown == capped {
				t.Fatalf("counter/byte validity conflated: %+v complete=%t capped=%t", capture, complete, capped)
			}
		})
	}
}

func TestUsageSessionAmbiguousHistoricalIdentityCannotCertifyAnUnchangedHelper(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
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
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil, nil)
	if got != (usage.Tokens{Input: 20}) || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("ambiguous old identity was accepted: %+v complete=%t error=%v", got, complete, err)
	}
}

func TestUsageSessionKnownHelperIdentityLossCannotDisappearFromReconcile(t *testing.T) {
	for _, timing := range []string{"before begin", "between begin and collect"} {
		for _, change := range []string{"unchanged", "removed", "unreadable", "malformed header", "reparented header", "changed parent only", "changed session only", "changed history boundary", "added history boundary", "removed history boundary", "changed header ID", "equivalent parent header"} {
			t.Run(timing+"/"+change, func(t *testing.T) {
				ctx, home := context.Background(), t.TempDir()
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
				got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil, nil)
				unverified := change != "unchanged" && !(timing == "before begin" && change == "equivalent parent header")
				if got != (usage.Tokens{Input: 20}) || complete == unverified || errors.Is(err, errUnverifiedChildren) != unverified || (!unverified && err != nil) {
					t.Fatalf("known helper identity loss: %+v complete=%t error=%v; want complete=%t unverified=%t", got, complete, err, !unverified, unverified)
				}
			})
		}
	}
}

func TestUsageSessionHeaderAliasesDoNotGrowKnownOwnership(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
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
		if got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil, nil); got != (usage.Tokens{Input: 20}) || complete || !errors.Is(err, errUnverifiedChildren) {
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
		if got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil, nil); got != (usage.Tokens{Input: 20}) || !complete || err != nil {
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
				ctx, home := context.Background(), t.TempDir()
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
				got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), &usage.Tokens{Input: 20}, nil, nil)
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
