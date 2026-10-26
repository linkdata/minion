package codexcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/usage"
)

func TestUsageWalkContinuesAfterLocalFailureButHonorsStops(t *testing.T) {
	local := errors.New("local discovery failure")
	for _, cause := range []error{local, fs.SkipAll, context.Canceled} {
		t.Run(cause.Error(), func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"one", "two"} {
				if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, name, "event.jsonl"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			visited := 0
			err := walkUsageFiles(ctx, root, time.Now(), func(_ string, entry fs.DirEntry) error {
				if !entry.IsDir() {
					visited++
					if visited == 1 {
						if errors.Is(cause, context.Canceled) {
							cancel()
						}
						return cause
					}
				}
				return nil
			})
			want := 1
			if cause == local {
				want = 2
			}
			if !errors.Is(err, cause) || visited != want {
				t.Fatalf("walk lost failure or skipped/stopped wrong branch: error=%v visited=%d want=%d", err, visited, want)
			}
		})
	}
}

func TestUsageWalkKeepsDiscoveredChildrenAfterLocalFailure(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one", "two"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "event.jsonl"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	local := errors.New("local discovery failure")
	first, seen := "", ""
	err := walkUsageFiles(t.Context(), root, time.Now(), func(path string, entry fs.DirEntry) error {
		if entry.IsDir() && filepath.Dir(path) == root {
			if first == "" {
				first = path
				return nil
			}
			return local
		}
		if !entry.IsDir() {
			seen = path
		}
		return nil
	})
	if !errors.Is(err, local) || first == "" || seen != filepath.Join(first, "event.jsonl") {
		t.Fatalf("partial directory scan lost a child: first=%q seen=%q err=%v", first, seen, err)
	}
}

func TestUsageDiscoverySkipsSymlinksAndNonRegularEntries(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "root-turn", "response", 10)...)
	child := writeUsageRollout(t, outside, "child", "root", completedUsageTurn("child", "child-turn", "response", 20)...)
	sessions := filepath.Join(home, "sessions")
	for link, target := range map[string]string{
		filepath.Join(filepath.Dir(root), "linked.jsonl"): child,
		filepath.Join(sessions, "linked-directory"):       filepath.Dir(child),
	} {
		symlinkOrSkip(t, target, link)
	}
	if err := os.Mkdir(filepath.Join(filepath.Dir(root), "directory.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	files, ambiguous, complete := usageFiles(t.Context(), home, "root", time.Now(), nil)
	if !complete || len(files) != 1 || files["root"].path != root || len(ambiguous) != 0 {
		t.Fatalf("discovered non-regular rollout: files=%v ambiguous=%v complete=%t", files, ambiguous, complete)
	}
}

func TestCollectUsageKeepsCurrentHelpersAcrossUnreadableUnrelatedDirectory(t *testing.T) {
	home := t.TempDir()
	blocked := filepath.Join(home, "sessions", "unrelated")
	if err := os.MkdirAll(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(blocked, 0700); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(blocked, 0000); err != nil {
		t.Fatal(err)
	}
	if file, err := os.Open(blocked); err == nil {
		_ = file.Close()
		t.Skip("directory permissions do not restrict this test user")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	started := time.Now()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "r", 10, 0, 1), usageEvent("task_complete", "root-turn"))
	writeUsageRollout(t, home, "child", "root", usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "c", 20, 0, 2), usageEvent("task_complete", "child-turn"))
	tokens, complete, childError := (&usageSession{}).collectUsage(t.Context(), home, "root", started, nil, []string{"child"}, nil)
	if tokens != (usage.Tokens{Input: 30, Output: 3}) || complete || childError != nil {
		t.Fatalf("local scan failure lost known helper evidence: tokens=%+v complete=%t childError=%v", tokens, complete, childError)
	}
}

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
			err := walkUsageFiles(t.Context(), filepath.Join(home, "sessions"), started, func(path string, entry fs.DirEntry) error {
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
			tokens, complete, childError := (&usageSession{}).collectUsage(t.Context(), home, "root", started, nil, []string{"child"}, nil)
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
			if err := walkUsageFiles(t.Context(), root, started, func(path string, entry fs.DirEntry) error {
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

func TestRolloutPathInventorySkipsOldAndUnrelatedDirectories(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "sessions")
	started := time.Date(2027, time.January, 1, 12, 0, 0, 0, time.UTC)
	current := filepath.Join(root, "2027", "01", "01", "current.jsonl")
	manual := filepath.Join(root, "manual", "manual.jsonl")
	for _, path := range []string{current, manual, filepath.Join(root, "2024", "old.jsonl"), filepath.Join(root, "2026", "01", "old.jsonl")} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := rolloutPaths(t.Context(), home, started)
	if err != nil || len(paths) != 1 || !paths[current] {
		t.Fatalf("rollout inventory followed stale or unrelated directories: paths=%v err=%v", paths, err)
	}
	seenManual := false
	if err := walkUsageFiles(t.Context(), root, started, func(path string, entry fs.DirEntry) error {
		seenManual = seenManual || path == manual
		return nil
	}); err != nil || !seenManual {
		t.Fatalf("usage discovery lost unknown layouts: seen=%t err=%v", seenManual, err)
	}
}

type failingCloseReader struct {
	io.Reader
	err error
}

func (r failingCloseReader) Close() error { return r.err }

func TestRolloutHeaderKeepsParsedMetaOnCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	path := filepath.Join(t.TempDir(), "child.jsonl")
	header := `{"type":"session_meta","payload":{"id":"child","session_id":"root","source":{"subagent":{"thread_spawn":{"parent_thread_id":"root"}}}}}` + "\n"
	meta, err := readRolloutHeaderFrom(failingCloseReader{strings.NewReader(header), closeErr}, path)
	if !errors.Is(err, closeErr) || meta.ID != "child" || meta.SessionID != "root" || meta.ParentID != "root" || meta.path != path {
		t.Fatalf("valid header lost after close failure: meta=%+v err=%v", meta, err)
	}
	meta, err = readRolloutHeaderFrom(failingCloseReader{strings.NewReader(`{"type":"other"}`), closeErr}, path)
	if !errors.Is(err, closeErr) || meta.ID != "" {
		t.Fatalf("invalid header admitted after close failure: meta=%+v err=%v", meta, err)
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
				tokens, complete, childError := (&usageSession{}).collectUsage(t.Context(), home, "root", started, nil, nil, nil)
				if tokens != (usage.Tokens{Input: 10, Output: 1}) || complete != (size == maxUsageHeaderBytes) || childError != nil {
					t.Fatalf("header boundary changed: tokens=%+v complete=%v childError=%v", tokens, complete, childError)
				}
			})
		}
	}
}
