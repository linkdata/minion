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

func TestHomePathUsesNativeVariableAndDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	for _, override := range []string{"", filepath.Join(root, "explicit")} {
		t.Setenv("CODEX_HOME", override)
		want := override
		if want == "" {
			want = filepath.Join(root, ".codex")
		}
		if got, err := HomePath(); err != nil || got != want {
			t.Fatalf("native home=%q, %v; want %q", got, err, want)
		}
	}
}

func TestRunUsesNativeHomeForLaunchAndUsage(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit", true: "explicit relative"}[explicit], func(t *testing.T) {
			binary := installFakeCodex(t, `#!/bin/sh
printf '%s\n' "${CODEX_HOME-absent}" > "$0.home"
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"root"}' '{"type":"item.completed","item":{"type":"collab_tool_call","receiver_thread_ids":["child"]}}' '{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":1}}'
`)
			home := t.TempDir()
			wantEnv := home
			if !explicit {
				t.Setenv("HOME", home)
				home = filepath.Join(home, ".codex")
				wantEnv = "absent"
			}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			relative, err := filepath.Rel(cwd, home)
			if err != nil {
				t.Fatal(err)
			}
			if explicit {
				t.Setenv("CODEX_HOME", relative)
			} else {
				t.Setenv("CODEX_HOME", "")
				if err := os.Unsetenv("CODEX_HOME"); err != nil {
					t.Fatal(err)
				}
			}
			writeUsageRollout(t, home, "root", "", usageEvent("task_started", "root-turn"), usageRecord("root", "root-turn", "root-response", 10, 0, 1), usageEvent("task_complete", "root-turn"))
			writeUsageRollout(t, home, "child", "root", usageEvent("task_started", "child-turn"), usageRecord("child", "child-turn", "child-response", 20, 5, 2), usageEvent("task_complete", "child-turn"))
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.CodexConfig{})
			if err != nil || result.Answer != "answer" || !result.UsageComplete || result.Tokens != (usage.Tokens{Input: 30, CachedInput: 5, Output: 3}) {
				t.Fatalf("relative home lost native usage: result=%+v error=%v", result, err)
			}
			data, err := os.ReadFile(binary + ".home")
			if err != nil || strings.TrimSpace(string(data)) != wantEnv {
				t.Fatalf("native home=%q, %v; want %q", data, err, wantEnv)
			}
		})
	}
}

func TestRunConfiguredKeyRequiresKnownNativeHome(t *testing.T) {
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
			if noHome {
				if err == nil || !strings.Contains(err.Error(), "CODEX_HOME") || result.Answer != "" || !result.UsageComplete || result.Tokens != (usage.Tokens{}) {
					t.Fatalf("unavailable home was not refused before inference: %+v, %v", result, err)
				}
				if _, err := os.Stat(binary + ".home"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unknown-home refusal launched Codex: %v", err)
				}
				return
			}
			if err != nil || result.Answer != "answer" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 10, Output: 1}) {
				t.Fatalf("missing home rejected configured key or overstated usage: %+v, %v", result, err)
			}
			data, err := os.ReadFile(binary + ".home")
			if err != nil || strings.TrimSpace(string(data)) != home {
				t.Fatalf("native home=%q, %v; want %q", data, err, home)
			}
			if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("home admission created a directory: %v", err)
			}
		})
	}
}

func TestRunKeepsMissingImplicitHomeWithConfiguredOrStoredAuthentication(t *testing.T) {
	for _, key := range []string{"", "fixture-key"} {
		t.Run(map[bool]string{false: "stored", true: "configured"}[key != ""], func(t *testing.T) {
			installFakeCodex(t, `#!/bin/sh
[ "${CODEX_HOME+x}" != x ] || exit 3
[ ! -e "$HOME/.codex" ] || exit 4
cat >/dev/null
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":1}}'
`)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CODEX_HOME", "")
			result, err := Run(context.Background(), t.TempDir(), "task", false, "", config.CodexConfig{APIKey: key})
			if err != nil || result.Answer != "answer" || result.UsageComplete || result.Tokens != (usage.Tokens{Input: 10, Output: 1}) {
				t.Fatalf("implicit home rejected or usage overstated: %+v, %v", result, err)
			}
		})
	}
}

func TestChildEnvironmentUsesOnlyItsExplicitNativeOverride(t *testing.T) {
	t.Setenv("CODEX_HOME", "/ambient/not-selected")
	for _, override := range []string{"", "CODEX_HOME=", "CODEX_HOME=relative"} {
		env := []string{"HOME=/native/home"}
		if override != "" {
			env = append(env, override)
		}
		got := strings.Join(childEnvironment(env, "/run", "/cache", "/canonical/home", false), "\n")
		if strings.Contains(got, "CODEX_HOME=") != (override == "CODEX_HOME=relative") || strings.Contains(got, "ambient") || strings.Contains(got, "relative") {
			t.Fatalf("native override selection depends on ambient environment: %s", got)
		}
	}
}
