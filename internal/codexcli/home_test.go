package codexcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestHomePathKeepsNativeSelectionAndMissingDirectories(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", "")
	if got, err := HomePath(); err != nil || got != filepath.Join(root, ".codex") {
		t.Fatalf("default native home=%q, %v", got, err)
	}
	t.Setenv("HOME", "")
	if got, err := HomePath(); err != nil || got != "" {
		t.Fatalf("guessed an unavailable native home=%q, %v", got, err)
	}
	target := filepath.Join(root, "real", "child")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", alias+"/../not-created")
	want := filepath.Join(root, "real", "not-created")
	if got, err := HomePath(); err != nil || got != want {
		t.Fatalf("native symlink/.. home=%q, %v; want %q", got, err, want)
	}
	if _, err := os.Stat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("home resolution created a directory: %v", err)
	}
}

func TestRunPinsRelativeNativeHomeForLaunchAndUsage(t *testing.T) {
	binary := installFakeCodex(t, `#!/bin/sh
printf '%s\n' "$CODEX_HOME" > "$0.home"
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"collab_tool_call","receiver_thread_ids":["child"]}}' '{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":1}}'
`)
	home := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", relative)
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
	writeUsageRollout(t, home, "child", "root", usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "child-response", 20, 5, 2), usageEvent("task_complete", "child-turn"))
	result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.CodexConfig{})
	if err != nil || result.Answer != "answer" || !result.UsageComplete || result.Tokens != (usage.Tokens{Input: 30, CachedInput: 5, Output: 3}) {
		t.Fatalf("relative home lost native usage: result=%+v error=%v", result, err)
	}
	data, err := os.ReadFile(binary + ".home")
	if err != nil || strings.TrimSpace(string(data)) != home {
		t.Fatalf("native home=%q, %v; want %q", data, err, home)
	}
}

func TestRunConfiguredKeyAllowsMissingNativeHome(t *testing.T) {
	for _, noHome := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit missing", true: "unavailable"}[noHome], func(t *testing.T) {
			binary := installFakeCodex(t, "#!/bin/sh\nprintf '%s\\n' \"${CODEX_HOME-}\" > \"$0.home\"\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"answer\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"cached_input_tokens\":0,\"output_tokens\":1}}'\n")
			home := filepath.Join(t.TempDir(), "not-created")
			t.Setenv("HOME", "")
			if noHome {
				home = ""
			}
			t.Setenv("CODEX_HOME", home)
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.CodexConfig{APIKey: "fixture-key"})
			if err != nil || result.Answer != "answer" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 10, Output: 1}) {
				t.Fatalf("missing home rejected configured key or overstated usage: %+v, %v", result, err)
			}
			data, err := os.ReadFile(binary + ".home")
			if err != nil || strings.TrimSpace(string(data)) != home {
				t.Fatalf("native home=%q, %v; want %q", data, err, home)
			}
			if !noHome {
				if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("home admission created a directory: %v", err)
				}
			}
		})
	}
}
