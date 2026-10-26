package codexcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
)

func rolloutTemplate(t *testing.T, id, parent string, tokens int64) []byte {
	t.Helper()
	path := writeUsageRollout(t, t.TempDir(), id, parent, completedUsageTurn(id, id+"-turn", id+"-response", tokens)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if id != "root" && parent == "" {
		data = []byte(strings.ReplaceAll(string(data), `"session_id":"root"`, `"session_id":"`+id+`"`))
	}
	return data
}

func putRolloutTemplate(t *testing.T, binary, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(binary+"."+name, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCleansFreshResumedAndNewConversationRollouts(t *testing.T) {
	binary := installFakeCodex(t, `#!/bin/sh
set -eu
cat >/dev/null
n=1
if [ -f "$0.count" ]; then n=$(( $(cat "$0.count") + 1 )); fi
printf '%s' "$n" > "$0.count"
dir="$CODEX_HOME/sessions/$(cat "$0.day")"
mkdir -p "$dir"
case "$n" in
  1) cp "$0.root" "$dir/rollout-root.jsonl"; cp "$0.child" "$dir/rollout-child.jsonl"; cp "$0.solo" "$dir/rollout-solo.jsonl"; id=root ;;
  2) cat "$0.resume" >> "$dir/rollout-root.jsonl"; id=root ;;
  3) cp "$0.fresh" "$dir/rollout-fresh.jsonl"; id=fresh ;;
esac
printf '{"type":"thread.started","thread_id":"%s"}\n' "$id"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	home := os.Getenv("CODEX_HOME")
	day := time.Now().Format("2006/01/02")
	putRolloutTemplate(t, binary, "day", []byte(day))
	putRolloutTemplate(t, binary, "root", rolloutTemplate(t, "root", "", 10))
	putRolloutTemplate(t, binary, "child", rolloutTemplate(t, "child", "root", 20))
	putRolloutTemplate(t, binary, "solo", []byte(`{"type":"session_meta","payload":{"id":"solo","session_id":"root","source":"exec"}}`+"\n"+strings.Join(completedUsageTurn("solo", "solo-turn", "solo-response", 30), "\n")+"\n"))
	putRolloutTemplate(t, binary, "fresh", rolloutTemplate(t, "fresh", "", 40))
	putRolloutTemplate(t, binary, "resume", []byte(strings.Join(completedUsageTurn("root", "resume-turn", "resume-response", 5), "\n")+"\n"))
	foreign := writeUsageRollout(t, home, "foreign", "", completedUsageTurn("foreign", "foreign-turn", "foreign-response", 99)...)
	preexisting := filepath.Join(home, "sessions", filepath.FromSlash(day), "older-root.jsonl")
	if err := os.WriteFile(preexisting, rolloutTemplate(t, "root", "", 100), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(preexisting, old, old); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(home, "auth.json")
	if err := os.WriteFile(auth, []byte("private login"), 0600); err != nil {
		t.Fatal(err)
	}
	var session Session
	source := t.TempDir()
	for call := 1; call <= 3; call++ {
		if call == 3 {
			session.NewConversation()
		}
		result, err := session.Run(t.Context(), source, "task", false, "", config.CodexConfig{})
		if err != nil || result.Answer != "done" {
			t.Fatalf("call %d: %+v, %v", call, result, err)
		}
	}
	paths := []string{}
	for _, id := range []string{"root", "child", "solo", "fresh"} {
		path := filepath.Join(home, "sessions", filepath.FromSlash(day), "rollout-"+id+".jsonl")
		paths = append(paths, path)
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned rollout remains at %s: %v", path, err)
		}
	}
	for path, want := range map[string]string{foreign: "", preexisting: "", auth: "private login"} {
		data, err := os.ReadFile(path)
		if err != nil || want != "" && string(data) != want {
			t.Fatalf("unrelated data changed at %s: %q, %v", path, data, err)
		}
	}
}

func TestSessionAdmitsPartialRunButRequiresThreadIdentity(t *testing.T) {
	for _, identified := range []bool{true, false} {
		t.Run(map[bool]string{true: "identified", false: "no thread ID"}[identified], func(t *testing.T) {
			binary := installFakeCodex(t, `#!/bin/sh
set -eu
cat >/dev/null
dir="$CODEX_HOME/sessions/$(cat "$0.day")"
mkdir -p "$dir"
cp "$0.root" "$dir/rollout-root.jsonl"
if [ -f "$0.identified" ]; then printf '%s\n' '{"type":"thread.started","thread_id":"root"}'; fi
exit 9
`)
			day := time.Now().Format("2006/01/02")
			putRolloutTemplate(t, binary, "day", []byte(day))
			putRolloutTemplate(t, binary, "root", rolloutTemplate(t, "root", "", 10))
			if identified {
				putRolloutTemplate(t, binary, "identified", nil)
			}
			path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", filepath.FromSlash(day), "rollout-root.jsonl")
			var session Session
			result, err := session.Run(t.Context(), t.TempDir(), "partial", false, "", config.CodexConfig{})
			wantInput := int64(0)
			if identified {
				wantInput = 10
			}
			if err == nil || result.Tokens.Input != wantInput || result.UsageComplete {
				t.Fatalf("partial run lost usage: %+v, %v", result, err)
			}
			closeErr := session.Close()
			if identified && closeErr != nil || !identified && (closeErr == nil || !strings.Contains(closeErr.Error(), path)) {
				t.Fatalf("partial cleanup recovery: identified=%t, err=%v", identified, closeErr)
			}
			_, err = os.Lstat(path)
			if identified && !errors.Is(err, os.ErrNotExist) || !identified && err != nil {
				t.Fatalf("partial rollout cleanup: identified=%t, err=%v", identified, err)
			}
		})
	}
}

func TestSessionCanceledRunPreservesUnverifiedRollout(t *testing.T) {
	binary := installFakeCodex(t, `#!/bin/sh
set -eu
cat >/dev/null
dir="$CODEX_HOME/sessions/$(cat "$0.day")"
mkdir -p "$dir"
cp "$0.root" "$dir/rollout-root.jsonl"
printf '%s\n' '{"type":"thread.started","thread_id":"root"}'
: > "$0.ready"
exec sleep 30
`)
	day := time.Now().Format("2006/01/02")
	putRolloutTemplate(t, binary, "day", []byte(day))
	putRolloutTemplate(t, binary, "root", rolloutTemplate(t, "root", "", 10))
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", filepath.FromSlash(day), "rollout-root.jsonl")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var session Session
	source := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := session.Run(ctx, source, "cancel", false, "", config.CodexConfig{})
		done <- err
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(binary + ".ready"); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("Codex exited before cancellation: %v", err)
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("Codex did not create the rollout")
		}
	}
	deadline.Stop()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled Run did not return")
	}
	if err := session.Close(); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), filepath.Join(os.Getenv("CODEX_HOME"), "sessions")) {
		t.Fatalf("canceled cleanup lacked recovery path: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("unverified rollout was removed: %v", err)
	}
}

func runOwnedRollout(t *testing.T) (*Session, string) {
	t.Helper()
	binary := installFakeCodex(t, `#!/bin/sh
set -eu
cat >/dev/null
dir="$CODEX_HOME/sessions/$(cat "$0.day")"
mkdir -p "$dir"
cp "$0.root" "$dir/rollout-root.jsonl"
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	day := time.Now().Format("2006/01/02")
	putRolloutTemplate(t, binary, "day", []byte(day))
	putRolloutTemplate(t, binary, "root", rolloutTemplate(t, "root", "", 10))
	session := new(Session)
	result, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
	if err != nil || result.Answer != "done" {
		t.Fatalf("start rollout: %+v, %v", result, err)
	}
	return session, filepath.Join(os.Getenv("CODEX_HOME"), "sessions", filepath.FromSlash(day), "rollout-root.jsonl")
}

func TestSessionClosePreservesReplacedAndSymlinkRollouts(t *testing.T) {
	for _, replacement := range []string{"same-header replacement", "changed header", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			session, path := runOwnedRollout(t)
			backup := filepath.Join(t.TempDir(), "original")
			if err := os.Link(path, backup); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if replacement != "changed header" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			switch replacement {
			case "same-header replacement":
				err = os.WriteFile(path, original, 0600)
			case "changed header":
				err = os.WriteFile(path, rolloutTemplate(t, "foreign", "", 10), 0600)
			case "symlink":
				symlinkOrSkip(t, backup, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Close(); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("unsafe rollout replacement was not reported: %v", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("unsafe rollout was removed: %v", err)
			}
			if replacement == "changed header" {
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(backup, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := session.Close(); err != nil {
				t.Fatalf("retry after restoring original: %v", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("original rollout remained after retry: %v", err)
			}
			if data, err := os.ReadFile(backup); err != nil || string(data) != string(original) {
				t.Fatalf("other hardlink changed: %q, %v", data, err)
			}
		})
	}
}

func TestSessionCloseRetriesFailedRolloutRemoval(t *testing.T) {
	session, path := runOwnedRollout(t)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	err := session.Close()
	if err == nil {
		t.Skip("directory permissions do not restrict deletion for this test user")
	}
	if !strings.Contains(err.Error(), path) || len(session.ownedRollouts) != 1 {
		t.Fatalf("deletion failure lost recovery path or retry state: %v, owned=%v", err, session.ownedRollouts)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry left rollout: %v", err)
	}
}

func TestSessionCloseAcceptsAlreadyMissingRollout(t *testing.T) {
	session, path := runOwnedRollout(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionPreservesAmbiguousRolloutIdentity(t *testing.T) {
	for _, duplicate := range []string{"root", "child"} {
		t.Run(duplicate, func(t *testing.T) {
			binary := installFakeCodex(t, `#!/bin/sh
set -eu
cat >/dev/null
dir="$CODEX_HOME/sessions/$(cat "$0.day")"
mkdir -p "$dir"
cp "$0.root" "$dir/rollout-root.jsonl"
if [ -f "$0.child" ]; then
  cp "$0.child" "$dir/rollout-child.jsonl"
  cp "$0.child" "$dir/rollout-duplicate.jsonl"
else
  cp "$0.root" "$dir/rollout-duplicate.jsonl"
fi
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
			day := time.Now().Format("2006/01/02")
			putRolloutTemplate(t, binary, "day", []byte(day))
			putRolloutTemplate(t, binary, "root", rolloutTemplate(t, "root", "", 10))
			if duplicate == "child" {
				putRolloutTemplate(t, binary, "child", rolloutTemplate(t, "child", "root", 20))
			}
			var session Session
			_, _ = session.Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
			err := session.Close()
			if err == nil || !strings.Contains(err.Error(), filepath.Join(os.Getenv("CODEX_HOME"), "sessions")) {
				t.Fatalf("ambiguous rollout cleanup was not reported: %v", err)
			}
			names := []string{"rollout-duplicate.jsonl"}
			if duplicate == "root" {
				names = append(names, "rollout-root.jsonl")
			} else if _, err := os.Lstat(filepath.Join(os.Getenv("CODEX_HOME"), "sessions", filepath.FromSlash(day), "rollout-root.jsonl")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unambiguous root was not cleaned: %v", err)
			}
			if duplicate == "child" {
				names = append(names, "rollout-child.jsonl")
			}
			for _, name := range names {
				if _, err := os.Lstat(filepath.Join(os.Getenv("CODEX_HOME"), "sessions", filepath.FromSlash(day), name)); err != nil {
					t.Fatalf("ambiguous rollout removed: %s, %v", name, err)
				}
			}
		})
	}
}

func TestSessionReportsInconsistentGrandchild(t *testing.T) {
	binary := installFakeCodex(t, `#!/bin/sh
set -eu
cat >/dev/null
dir="$CODEX_HOME/sessions/$(cat "$0.day")"
mkdir -p "$dir"
cp "$0.root" "$dir/rollout-root.jsonl"
cp "$0.child" "$dir/rollout-child.jsonl"
cp "$0.grandchild" "$dir/rollout-grandchild.jsonl"
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	day := time.Now().Format("2006/01/02")
	putRolloutTemplate(t, binary, "day", []byte(day))
	putRolloutTemplate(t, binary, "root", rolloutTemplate(t, "root", "", 10))
	putRolloutTemplate(t, binary, "child", rolloutTemplate(t, "child", "root", 20))
	grandchild := strings.ReplaceAll(string(rolloutTemplate(t, "grandchild", "child", 30)), `"session_id":"root"`, `"session_id":"foreign"`)
	putRolloutTemplate(t, binary, "grandchild", []byte(grandchild))
	var session Session
	_, _ = session.Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", filepath.FromSlash(day), "rollout-grandchild.jsonl")
	if err := session.Close(); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("inconsistent grandchild was not reported: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("inconsistent grandchild was removed: %v", err)
	}
}

func TestSessionReportsNewNonregularRolloutPath(t *testing.T) {
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			binary := installFakeCodex(t, `#!/bin/sh
set -eu
cat >/dev/null
dir="$CODEX_HOME/sessions/$(cat "$0.day")"
mkdir -p "$dir"
if [ -f "$0.symlink" ]; then
  ln -s "$0.root" "$dir/rollout-root.jsonl"
else
  mkdir "$dir/rollout-root.jsonl"
fi
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
			day := time.Now().Format("2006/01/02")
			putRolloutTemplate(t, binary, "day", []byte(day))
			putRolloutTemplate(t, binary, "root", rolloutTemplate(t, "root", "", 10))
			if kind == "symlink" {
				putRolloutTemplate(t, binary, "symlink", nil)
			}
			var session Session
			_, _ = session.Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
			path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", filepath.FromSlash(day), "rollout-root.jsonl")
			if err := session.Close(); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("new nonregular rollout path was not reported: %v", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("new nonregular rollout path was removed: %v", err)
			}
		})
	}
}

func TestSessionReportsHelperWithoutRootRollout(t *testing.T) {
	binary := installFakeCodex(t, `#!/bin/sh
set -eu
cat >/dev/null
dir="$CODEX_HOME/sessions/$(cat "$0.day")"
mkdir -p "$dir"
cp "$0.child" "$dir/rollout-child.jsonl"
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	day := time.Now().Format("2006/01/02")
	putRolloutTemplate(t, binary, "day", []byte(day))
	putRolloutTemplate(t, binary, "child", rolloutTemplate(t, "child", "root", 20))
	var session Session
	_, _ = session.Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{})
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", filepath.FromSlash(day), "rollout-child.jsonl")
	if err := session.Close(); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("helper without root rollout was not reported: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("helper without root rollout was removed: %v", err)
	}
}

func TestSessionPreservesConcurrentUnrelatedRollouts(t *testing.T) {
	binary := installFakeCodex(t, `#!/bin/sh
set -eu
cat >/dev/null
dir="$CODEX_HOME/sessions/$(cat "$0.day")"
mkdir -p "$dir"
cp "$0.root" "$dir/rollout-root.jsonl"
cp "$0.foreign" "$dir/rollout-foreign.jsonl"
cp "$0.foreign" "$dir/rollout-foreign-duplicate.jsonl"
cp "$0.foreign-child" "$dir/rollout-foreign-child.jsonl"
printf 'not a rollout header\n' > "$dir/rollout-broken.jsonl"
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' '{"type":"turn.completed"}'
`)
	day := time.Now().Format("2006/01/02")
	putRolloutTemplate(t, binary, "day", []byte(day))
	putRolloutTemplate(t, binary, "root", rolloutTemplate(t, "root", "", 10))
	putRolloutTemplate(t, binary, "foreign", rolloutTemplate(t, "foreign", "", 20))
	foreignChild := strings.ReplaceAll(string(rolloutTemplate(t, "foreign-child", "foreign", 30)), `"session_id":"root"`, `"session_id":"foreign"`)
	putRolloutTemplate(t, binary, "foreign-child", []byte(foreignChild))
	var session Session
	if result, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.CodexConfig{}); err != nil || result.Answer != "done" {
		t.Fatalf("concurrent rollout run: %+v, %v", result, err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("unrelated rollout caused cleanup failure: %v", err)
	}
	for _, tc := range []struct {
		name string
		keep bool
	}{{"rollout-root.jsonl", false}, {"rollout-foreign.jsonl", true}, {"rollout-foreign-duplicate.jsonl", true}, {"rollout-foreign-child.jsonl", true}, {"rollout-broken.jsonl", true}} {
		path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", filepath.FromSlash(day), tc.name)
		_, err := os.Lstat(path)
		if tc.keep && err != nil || !tc.keep && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("concurrent rollout %s: keep=%t, err=%v", tc.name, tc.keep, err)
		}
	}
}
