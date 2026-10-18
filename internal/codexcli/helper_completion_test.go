package codexcli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestKnownHelpersRequireLifecycleAccess(t *testing.T) {
	fallback := usage.Tokens{Input: 100, CachedInput: 10, Output: 7}
	for _, scenario := range []string{"missing root ID", "missing log", "canceled scan"} {
		t.Run(scenario, func(t *testing.T) {
			home, rootID := t.TempDir(), "root"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "missing root ID":
				rootID = ""
			case "canceled scan":
				cancel()
			}
			tokens, complete, err := collectUsage(ctx, home, rootID, time.Now(), &fallback, []string{"child"})
			if tokens != fallback || complete || !errors.Is(err, errUnverifiedChildren) {
				t.Fatalf("unknown lifecycle accepted: tokens=%+v complete=%t err=%v", tokens, complete, err)
			}
		})
	}
	for _, scenario := range []string{"missing file", "directory", "exhausted budget"} {
		t.Run(scenario, func(t *testing.T) {
			path := t.TempDir()
			budget := int64(maxUsageScanBytes)
			switch scenario {
			case "missing file":
				path += "/missing.jsonl"
			case "exhausted budget":
				budget = 0
			}
			_, complete, _, err := readRolloutUsage(context.Background(), rolloutMeta{ID: "child", path: path}, "root", &budget)
			if complete || !errors.Is(err, errUnverifiedChildren) {
				t.Fatalf("unreadable lifecycle accepted: complete=%t err=%v", complete, err)
			}
		})
	}
}

func TestDuplicateHelperRolloutsCannotAttestCompletion(t *testing.T) {
	home := t.TempDir()
	writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
	path := writeUsageRollout(t, home, "child", "root", usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "child-response", 20, 0, 2), usageEvent("task_complete", "child-turn"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "duplicate.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	tokens, complete, err := collectUsage(context.Background(), home, "root", time.Now(), nil, []string{"child"})
	if tokens != (usage.Tokens{Input: 30, Output: 3}) || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("ambiguous helper accepted or double-counted: tokens=%+v complete=%t err=%v", tokens, complete, err)
	}
}

func TestRunRetainsHelperFailureAlongsideProviderFailure(t *testing.T) {
	for _, scenario := range []struct{ name, events, failure string }{
		{"exit", `{"type":"turn.completed"}`, "exit status 7"},
		{"provider", `{"type":"turn.failed","error":{"message":"provider failed"}}`, "provider failed"},
		{"parse", `{damaged`, "invalid JSONL event"},
		{"missing completion", `{"type":"item.completed","item":{"type":"agent_message","text":"progress"}}`, "missing turn.completed"},
		{"missing answer", `{"type":"turn.completed"}`, "without a final agent message"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			script := "#!/bin/sh\ncat <<'EVENTS'\n" + `{"type":"thread.started","thread_id":"root"}
{"type":"item.completed","item":{"type":"collab_tool_call","receiver_thread_ids":["child"]}}` + "\n" + scenario.events + "\nEVENTS\n"
			if scenario.name == "exit" {
				script += "exit 7\n"
			}
			installFakeCodex(t, script)
			home := os.Getenv("CODEX_HOME")
			writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
			writeUsageRollout(t, home, "child", "root", usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "child-response", 20, 0, 2))
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.CodexConfig{})
			if !errors.Is(err, errUnverifiedChildren) || !strings.Contains(err.Error(), scenario.failure) || result.Answer != "" || result.UsageComplete || result.Tokens.Input != 30 {
				t.Fatalf("provider/helper cause lost: result=%+v err=%v", result, err)
			}
			if scenario.name == "exit" {
				var native *exec.ExitError
				if !errors.As(err, &native) || native.ExitCode() != 7 {
					t.Fatalf("native exit identity lost: %v", err)
				}
			}
		})
	}
}
