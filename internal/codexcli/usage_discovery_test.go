package codexcli

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/usage"
)

func TestCollectUsagePrunesHistoricalDaysBeforeCurrentRootAndHelper(t *testing.T) {
	for _, done := range []bool{false, true} {
		t.Run(fmt.Sprint(done), func(t *testing.T) {
			home := t.TempDir()
			started := time.Now()
			writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "r", 10, 0, 1), usageEvent("task_complete", "root-turn"))
			child := []string{usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "c", 20, 0, 2)}
			if done {
				child = append(child, usageEvent("task_complete", "child-turn"))
			}
			writeUsageRollout(t, home, "child", "root", child...)
			old := filepath.Join(home, "sessions", started.UTC().AddDate(0, 0, -2).Format("2006/01/02"))
			if err := os.MkdirAll(old, 0700); err != nil {
				t.Fatal(err)
			}
			// Fresh mtimes deliberately defeat the file-mtime gate. These dated
			// histories must be pruned before consuming the traversal/header caps.
			for i := 0; i < 250; i++ {
				if err := os.WriteFile(filepath.Join(old, fmt.Sprintf("old-%03d.jsonl", i)), []byte("unrelated old content\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			visits := 0
			err := walkUsageFiles(context.Background(), filepath.Join(home, "sessions"), started, func(path string, entry fs.DirEntry) error {
				visits++
				if strings.HasPrefix(path, old+string(os.PathSeparator)) {
					t.Fatal("old day consumed a traversal entry")
				}
				if visits > 16 {
					t.Fatal("dated history exhausted a scaled current-entry budget")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			tokens, complete, childError := collectUsage(context.Background(), home, "root", started, nil, []string{"child"})
			if tokens != (usage.Tokens{Input: 30, Output: 3}) || complete != done || (childError != nil) == done {
				t.Fatalf("current helper lost behind dated history: tokens=%+v complete=%v childError=%v", tokens, complete, childError)
			}
		})
	}
}

func TestUsageDatePruningPreservesMidnightMarginAndUnknownLayouts(t *testing.T) {
	for _, tc := range []struct{ start, old, margin, recent string }{
		{"2027-01-01T00:15:00Z", "2026/12/30", "2026/12/31", "2027/01/02"},
		{"2024-03-01T00:15:00Z", "2024/02/28", "2024/02/29", "2024/03/01"},
		{"2023-03-01T00:15:00Z", "2023/02/27", "2023/02/28", "2023/03/01"},
		{"2027-01-01T00:15:00+14:00", "2026/12/29", "2026/12/30", "2027/01/01"},
		{"2026-12-31T23:45:00-12:00", "2026/12/30", "2026/12/31", "2027/01/02"},
	} {
		t.Run(tc.start, func(t *testing.T) {
			root := t.TempDir()
			paths := []string{tc.old + "/pruned.jsonl", tc.margin + "/margin.jsonl", tc.recent + "/recent.jsonl", "flat.jsonl", "unknown/2020/01/01/file.jsonl", "2020/13/01/file.jsonl", "2020/02/31/file.jsonl"}
			for _, name := range paths {
				path := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			started, err := time.Parse(time.RFC3339, tc.start)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			if err := walkUsageFiles(context.Background(), root, started, func(path string, entry fs.DirEntry) error {
				if !entry.IsDir() {
					rel, _ := filepath.Rel(root, path)
					seen[filepath.ToSlash(rel)] = true
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for i, name := range paths {
				if seen[name] != (i != 0) {
					t.Fatalf("date/layout path=%s retained=%v", name, seen[name])
				}
			}
		})
	}
}

func TestCollectUsageHeaderAcceptsExactLimitAndRejectsNextByte(t *testing.T) {
	const prefix = `{"type":"session_meta","payload":{"id":"unrelated"},"padding":"`
	const suffix = `"}`
	for _, size := range []int{maxUsageHeaderBytes, maxUsageHeaderBytes + 1} {
		for _, ending := range []string{"\n", "\r\n", ""} {
			t.Run(fmt.Sprintf("bytes%d/ending%q", size, ending), func(t *testing.T) {
				home := t.TempDir()
				started := time.Now()
				root := writeUsageRollout(t, home, "root", "", usageEvent("task_started", "turn"), usageRecord("root", "turn", "response", 10, 0, 1), usageEvent("task_complete", "turn"))
				path := filepath.Join(filepath.Dir(root), "unrelated.jsonl")
				header := prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix + ending
				if ending != "" {
					header += "unrelated body is not inspected\n"
				}
				if err := os.WriteFile(path, []byte(header), 0600); err != nil {
					t.Fatal(err)
				}
				tokens, complete, childError := collectUsage(context.Background(), home, "root", started, nil, nil)
				if tokens != (usage.Tokens{Input: 10, Output: 1}) || complete != (size == maxUsageHeaderBytes) || childError != nil {
					t.Fatalf("header boundary changed: tokens=%+v complete=%v childError=%v", tokens, complete, childError)
				}
			})
		}
	}
}
