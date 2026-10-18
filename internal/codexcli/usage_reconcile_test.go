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
