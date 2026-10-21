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
			tokens, complete, err := (&usageSession{}).collectUsage(ctx, home, rootID, time.Now(), &fallback, []string{"child"}, nil)
			if tokens != fallback || complete || !errors.Is(err, errUnverifiedChildren) {
				t.Fatalf("unknown lifecycle accepted: tokens=%+v complete=%t err=%v", tokens, complete, err)
			}
		})
	}
	// Zero budget documents fail-closed output; the bounded reader also rejects it without the early guard.
	for _, scenario := range []string{"missing file", "directory", "exhausted budget"} {
		t.Run(scenario, func(t *testing.T) {
			path := t.TempDir()
			budget := int64(maxUsageScanBytes)
			switch scenario {
			case "missing file":
				path += "/missing.jsonl"
			case "exhausted budget":
				path = writeUsageRollout(t, t.TempDir(), "child", "root", completedUsageTurn("child", "turn", "response", 5)...)
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
	duplicate := strings.Replace(string(data), `"input_tokens":20`, `"input_tokens":900`, 1)
	if duplicate == string(data) {
		t.Fatal("fixture must change duplicate usage")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "duplicate.jsonl"), []byte(duplicate), 0600); err != nil {
		t.Fatal(err)
	}
	tokens, complete, err := (&usageSession{}).collectUsage(context.Background(), home, "root", time.Now(), nil, []string{"child"}, nil)
	if tokens != (usage.Tokens{Input: 10, Output: 1}) || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("ambiguous helper accepted or double-counted: tokens=%+v complete=%t err=%v", tokens, complete, err)
	}
}

func TestDuplicateRootRolloutsCannotInflateFreshUsage(t *testing.T) {
	home := t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "turn", "response", 100)...)
	data, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := strings.Replace(string(data), `"input_tokens":100`, `"input_tokens":900`, 1)
	if duplicate == string(data) {
		t.Fatal("fixture must change duplicate usage")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "duplicate-root.jsonl"), []byte(duplicate), 0600); err != nil {
		t.Fatal(err)
	}
	fallback := usage.Tokens{Input: 7}
	for _, tc := range []struct {
		name     string
		fallback *usage.Tokens
		want     usage.Tokens
	}{
		{"stdout", &fallback, fallback},
		{"no stdout", nil, usage.Tokens{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, complete, err := (&usageSession{}).collectUsage(context.Background(), home, "root", time.Now(), tc.fallback, nil, nil)
			if got != tc.want || complete || !errors.Is(err, errUnverifiedChildren) {
				t.Fatalf("ambiguous root usage = %+v, complete=%t, err=%v; want %+v, incomplete and unverified", got, complete, err, tc.want)
			}
		})
	}
}

func TestResumedDuplicateRootCannotAttestCompletion(t *testing.T) {
	ctx, home := context.Background(), t.TempDir()
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "old", "r1", 100)...)
	var session usageSession
	session.begin(ctx, home, "root", time.Now())
	data, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := filepath.Join(filepath.Dir(root), "duplicate-root.jsonl")
	if err := os.WriteFile(duplicate, data, 0600); err != nil {
		t.Fatal(err)
	}
	appendUsageEvents(t, root, completedUsageTurn("root", "new", "r2", 20)...)
	got, complete, err := session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
	if got != (usage.Tokens{}) || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("resumed ambiguous root accepted: tokens=%+v complete=%t err=%v", got, complete, err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(duplicate, old, old); err != nil {
		t.Fatal(err)
	}
	session.begin(ctx, home, "root", time.Now())
	appendUsageEvents(t, root, completedUsageTurn("root", "later", "r3", 5)...)
	got, complete, err = session.collectUsage(ctx, home, "root", time.Now(), nil, nil, nil)
	if got != (usage.Tokens{}) || complete || !errors.Is(err, errUnverifiedChildren) {
		t.Fatalf("aged duplicate root accepted: tokens=%+v complete=%t err=%v", got, complete, err)
	}
}

func TestRunRejectsDuplicateRootWithoutStreamedHelper(t *testing.T) {
	installFakeCodex(t, `#!/bin/sh
cat <<'EVENTS'
{"type":"thread.started","thread_id":"root"}
{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}
{"type":"turn.completed"}
EVENTS
`)
	home := os.Getenv("CODEX_HOME")
	root := writeUsageRollout(t, home, "root", "", completedUsageTurn("root", "turn", "r1", 10)...)
	data, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "duplicate-root.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.CodexConfig{})
	if !errors.Is(err, errUnverifiedChildren) || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{}) {
		t.Fatalf("ambiguous root published answer: result=%+v err=%v", result, err)
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
			if !errors.Is(err, errUnverifiedChildren) || !strings.Contains(err.Error(), scenario.failure) || result.Answer != "" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 30, Output: 3}) {
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
