package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/usage"
)

func TestSessionContinuesAndResetsAtStableRolePaths(t *testing.T) {
	const doubled = `{"type":"result","subtype":"success","result":"continued","modelUsage":{"main":{"inputTokens":6,"cacheCreationInputTokens":10,"cacheReadInputTokens":14,"outputTokens":22}}}`
	binary := fakeExecutable(t, "claude", `n=0
[ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
printf '%s\n' "$@" > "$0.$n.args"
printf '%s\n' "$PWD" "$CLAUDE_CONFIG_DIR" > "$0.$n.paths"
if [ -e "$PWD/prior-notes" ] || [ -e "$GOCACHE/prior-notes" ]; then printf old; else printf clean; fi > "$0.$n.scratch"
cat >/dev/null
resume=false
for arg do [ "$arg" != --resume ] || resume=true; done
if $resume; then printf '%s\n' '`+doubled+`'; else printf '%s\n' '`+successfulResult+`'; fi
`)
	workspace, oldGrant, newGrant := t.TempDir(), t.TempDir(), t.TempDir()
	settings := config.ClaudeConfig{APIKey: "fixture-private-key", Model: "fixed-model", Effort: "high", Skills: []string{skills.BuiltinAntiPromptInjection}, ReadOnlyDirs: []string{oldGrant}}
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	known := usage.Tokens{Input: 15, CachedInput: 7, Output: 11}
	for call := 1; call <= 3; call++ {
		if call == 2 {
			settings.ReadOnlyDirs = []string{newGrant}
		}
		if call == 3 {
			for _, dir := range []string{session.runtimeDir, session.cacheDir} {
				if err := os.WriteFile(filepath.Join(dir, "prior-notes"), []byte("previous critic findings"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			session.NewConversation()
			settings.ReadOnlyDirs = nil
		}
		result, err := session.Run(t.Context(), workspace, "task", false, "", settings)
		if err != nil || !result.UsageComplete || result.Tokens != known {
			t.Fatalf("call%d: %+v, %v", call, result, err)
		}
	}
	oldGrant, err := filepath.EvalSymlinks(oldGrant)
	if err != nil {
		t.Fatal(err)
	}
	newGrant, err = filepath.EvalSymlinks(newGrant)
	if err != nil {
		t.Fatal(err)
	}
	firstArgs := strings.Split(readFile(t, binary+".1.args"), "\n")
	secondArgs := strings.Split(readFile(t, binary+".2.args"), "\n")
	thirdArgs := strings.Split(readFile(t, binary+".3.args"), "\n")
	option := func(args []string, flag string) string {
		t.Helper()
		at := slices.Index(args, flag)
		if at < 0 || at+1 == len(args) {
			t.Fatalf("missing %s: %q", flag, args)
		}
		return args[at+1]
	}
	firstID := option(firstArgs, "--session-id")
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(firstID) {
		t.Fatalf("session ID is not a version-4 UUID: %q", firstID)
	}
	if option(secondArgs, "--resume") != firstID || option(thirdArgs, "--session-id") == firstID {
		t.Fatal("conversation selection did not continue then start fresh")
	}
	paths := readFile(t, binary+".1.paths")
	if paths != readFile(t, binary+".2.paths") || paths != readFile(t, binary+".3.paths") {
		t.Fatal("runtime or authentication path changed")
	}
	if readFile(t, binary+".3.scratch") != "clean" {
		t.Fatal("fresh conversation retained model-writable critic notes")
	}
	for i, args := range [][]string{firstArgs, secondArgs, thirdArgs} {
		for _, forbidden := range []string{"--continue", "--fork-session", "--no-session-persistence"} {
			if slices.Contains(args, forbidden) {
				t.Fatalf("call%d used %s", i+1, forbidden)
			}
		}
		if option(args, "--permission-mode") != "dontAsk" || option(args, "--model") != settings.Model || option(args, "--effort") != settings.Effort || !slices.Contains(args, "--safe-mode") {
			t.Fatal("role settings were not reapplied")
		}
		var policy struct {
			AutoMemory *bool `json:"autoMemoryEnabled"`
			Sandbox    struct {
				Credentials struct {
					Files []struct{ Path, Mode string } `json:"files"`
				} `json:"credentials"`
			} `json:"sandbox"`
		}
		if err := json.Unmarshal([]byte(option(args, "--settings")), &policy); err != nil {
			t.Fatal(err)
		}
		if policy.AutoMemory == nil || *policy.AutoMemory || len(policy.Sandbox.Credentials.Files) != 1 || policy.Sandbox.Credentials.Files[0].Path != filepath.Join(session.authDir, "api-key") || policy.Sandbox.Credentials.Files[0].Mode != "deny" {
			t.Fatal("native memory or private authentication protection changed")
		}
		for _, grant := range []string{oldGrant, newGrant} {
			if slices.Contains(args, grant) {
				t.Fatalf("call%d passed read-only evidence as a writable grant %s", i+1, grant)
			}
		}
	}
	owned := []string{session.runtimeDir, session.authDir, session.embeddedDir}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range owned {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("owned resource remains %s: %v", path, err)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(t.Context(), workspace, "task", false, "", settings); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed session ran: %v", err)
	}
}

func TestSessionRejectsChangedIdentityAndSettingsBeforeNativeCall(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *string, *bool, *string, *config.ClaudeConfig)
	}{
		{"source", func(t *testing.T, dir *string, _ *bool, _ *string, _ *config.ClaudeConfig) { *dir = t.TempDir() }},
		{"binary", func(t *testing.T, _ *string, _ *bool, _ *string, _ *config.ClaudeConfig) {
			other := t.TempDir()
			if err := os.WriteFile(filepath.Join(other, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", other+string(os.PathListSeparator)+os.Getenv("PATH"))
		}},
		{"output", func(t *testing.T, _ *string, _ *bool, out *string, _ *config.ClaudeConfig) { *out = t.TempDir() }},
		{"writable", func(_ *testing.T, _ *string, writable *bool, _ *string, _ *config.ClaudeConfig) { *writable = true }},
		{"model", func(_ *testing.T, _ *string, _ *bool, _ *string, cfg *config.ClaudeConfig) { cfg.Model = "other" }},
		{"effort", func(_ *testing.T, _ *string, _ *bool, _ *string, cfg *config.ClaudeConfig) { cfg.Effort = "other" }},
		{"key", func(_ *testing.T, _ *string, _ *bool, _ *string, cfg *config.ClaudeConfig) { cfg.APIKey = "other" }},
		{"skill", func(_ *testing.T, _ *string, _ *bool, _ *string, cfg *config.ClaudeConfig) {
			cfg.Skills[0] = "builtin://other"
		}},
		{"schema", func(_ *testing.T, _ *string, _ *bool, _ *string, cfg *config.ClaudeConfig) { cfg.OutputSchema = `{}` }},
	} {
		t.Run(test.name, func(t *testing.T) {
			binary := fakeExecutable(t, "claude", "cat >/dev/null\nprintf called >>\"$0.calls\"\nprintf '%s\\n' '"+successfulResult+"'\n")
			dir, output, writable := t.TempDir(), "", false
			settings := config.ClaudeConfig{APIKey: "private-fixture-key", Skills: []string{skills.BuiltinAntiPromptInjection}}
			var session Session
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := session.Run(t.Context(), dir, "first", writable, output, settings); err != nil {
				t.Fatal(err)
			}
			test.change(t, &dir, &writable, &output, &settings)
			if _, err := session.Run(t.Context(), dir, "second", writable, output, settings); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("changed bound inputs accepted: %v", err)
			}
			if readFile(t, binary+".calls") != "called" {
				t.Fatal("changed session reached native execution")
			}
		})
	}
}

func TestSessionPreservesFailedTurnAndReportsMissingResume(t *testing.T) {
	binary := fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
printf '%s\n' "$@" > "$0.$n.args"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+successfulResult+`'; exit 7 ;;
2) printf '%s\n' '`+strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(successfulResult, `"inputTokens":3`, `"inputTokens":6`), `"cacheCreationInputTokens":5`, `"cacheCreationInputTokens":10`), `"cacheReadInputTokens":7`, `"cacheReadInputTokens":14`), `"outputTokens":11`, `"outputTokens":22`)+`' ;;
*) printf 'No conversation found\n' >&2; exit 9 ;;
esac
`)
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	dir := t.TempDir()
	first, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || first.Answer != "" || first.UsageComplete || first.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
		t.Fatalf("failed turn: %+v,%v", first, err)
	}
	id := session.id
	second, err := session.Run(t.Context(), dir, "continue", false, "", config.ClaudeConfig{})
	if err != nil || second.Tokens != first.Tokens || !second.UsageComplete {
		t.Fatalf("continued failed turn: %+v,%v", second, err)
	}
	third, err := session.Run(t.Context(), dir, "continue again", false, "", config.ClaudeConfig{})
	if err == nil || !strings.Contains(err.Error(), "No conversation found") || third.Answer != "" || session.id != id || !strings.Contains(readFile(t, binary+".3.args"), "--resume\n"+id) || readFile(t, binary+".count") != "3" {
		t.Fatalf("missing resume silently replaced: %+v,%v", third, err)
	}
}

func TestSessionNativeHomeCannotChange(t *testing.T) {
	for _, nativeEnv := range []string{"CLAUDE_CONFIG_DIR", "HOME"} {
		t.Run(nativeEnv, func(t *testing.T) {
			binary := fakeExecutable(t, "claude", "cat >/dev/null\nprintf called >>\"$0.calls\"\nprintf '%s\\n' '"+successfulResult+"'\n")
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv(nativeEnv, t.TempDir())
			var session Session
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			dir := t.TempDir()
			if _, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{}); err != nil {
				t.Fatal(err)
			}
			t.Setenv(nativeEnv, t.TempDir())
			if _, err := session.Run(t.Context(), dir, "second", false, "", config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("native configuration changed: %v", err)
			}
			if readFile(t, binary+".calls") != "called" {
				t.Fatal("changed native home executed")
			}
		})
	}
}

func TestSessionRejectsSwitchToExplicitDefaultConfig(t *testing.T) {
	binary := fakeExecutable(t, "claude", "cat >/dev/null\nprintf called >>\"$0.calls\"\nprintf '%s\\n' '"+successfulResult+"'\n")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	dir := t.TempDir()
	if _, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	if _, err := session.Run(t.Context(), dir, "second", false, "", config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "native configuration changed") {
		t.Fatalf("default configuration switched to explicit: %v", err)
	}
	if readFile(t, binary+".calls") != "called" {
		t.Fatal("changed native configuration reached CLI")
	}
}

func TestResumedUsageKeepsOnlyNewWholeTreeCounters(t *testing.T) {
	known := usage.Tokens{Input: 15, CachedInput: 7, Output: 11}
	const aggregateOnly = `{"type":"result","subtype":"success","result":"final answer","modelUsage":{"claude-main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":11}}}`
	doubled := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(successfulResult, `"inputTokens":3`, `"inputTokens":6`), `"cacheCreationInputTokens":5`, `"cacheCreationInputTokens":10`), `"cacheReadInputTokens":7`, `"cacheReadInputTokens":14`), `"outputTokens":11`, `"outputTokens":22`)
	withHelper := strings.TrimSuffix(successfulResult, "}}") + `,"helper":{"inputTokens":29,"cacheCreationInputTokens":31,"cacheReadInputTokens":37,"outputTokens":41}}}`
	for _, test := range []struct {
		name, stream     string
		baseline, want   usage.Tokens
		complete, failed bool
	}{
		{"new counters", doubled, known, known, true, false},
		{"prior totals repeated without current root usage", aggregateOnly, known, usage.Tokens{}, true, false},
		{"stale cumulative with current root usage", successfulResult, known, known, false, false},
		{"helper total", withHelper, known, usage.Tokens{Input: 97, CachedInput: 37, Output: 41}, true, false},
		{"repeated result snapshots", numberResultEvents(doubled + "\n" + doubled), known, known, true, false},
		{"root fallback is current turn", `{"type":"result","subtype":"success","result":"answer","usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}`, known, known, false, false},
		{"missing usage", `{"type":"result","subtype":"success","result":"answer"}`, known, usage.Tokens{}, false, false},
		{"decreasing aggregate without current root usage", aggregateOnly, usage.Tokens{Input: 30, CachedInput: 14, Output: 22}, usage.Tokens{}, false, false},
		{"decreasing aggregate retains current assistant", assistantUsageEvent("current", 3, 5, 7, 11) + "\n" + successfulResult, usage.Tokens{Input: 30, CachedInput: 14, Output: 22}, known, false, false},
		{"failed new counters", strings.Replace(doubled, `"is_error":false`, `"is_error":true`, 1), known, known, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := jsonlOutput{baseline: test.baseline}
			writeChunks(t, &output, test.stream+"\n")
			output.finish()
			if output.result.Tokens != test.want || output.result.UsageComplete != test.complete || (output.err != nil) != test.failed {
				t.Fatalf("resumed usage: %+v,%v; want %+v complete=%t failed=%t", output.result, output.err, test.want, test.complete, test.failed)
			}
		})
	}
}

func TestSessionRejectsForeignNativeConversation(t *testing.T) {
	for _, stream := range []string{
		`{"type":"system","subtype":"init","session_id":"other"}` + "\n" + successfulResult,
		strings.TrimSuffix(successfulResult, "}") + `,"session_id":"other"}`,
	} {
		output := jsonlOutput{sessionID: "expected"}
		writeChunks(t, &output, stream+"\n")
		if output.err == nil || !strings.Contains(output.err.Error(), "identity changed") || output.result.Answer != "" || output.result.UsageComplete {
			t.Fatalf("foreign conversation %s accepted: %+v,%v", stream, output.result, output.err)
		}
	}
}

func TestSessionScratchAndCloseFailuresAreVisible(t *testing.T) {
	binary := fakeExecutable(t, "claude", "cat >/dev/null\nprintf called >>\"$0.calls\"\nprintf '%s\\n' '"+successfulResult+"'\n")
	var session Session
	dir := t.TempDir()
	if _, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{}); err != nil {
		t.Fatal(err)
	}
	runtime := session.runtimeDir
	t.Cleanup(func() {
		session.runtimeDir = runtime
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	session.NewConversation()
	// A malformed owned path gives a portable filesystem failure without
	// modifying host permissions or deleting any unowned resource.
	session.runtimeDir = "invalid\x00path"
	if _, err := session.Run(t.Context(), dir, "new", false, "", config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "scratch cleanup") {
		t.Fatalf("scratch failure ignored: %v", err)
	}
	if readFile(t, binary+".calls") != "called" {
		t.Fatal("scratch failure reached native execution")
	}
	if err := session.Close(); err == nil || session.runtimeDir == "" {
		t.Fatalf("failed cleanup lost its path: %v", err)
	}
}

func TestSessionRequiresItsOwnRootIdentity(t *testing.T) {
	for _, test := range []struct {
		name, stream string
		failure      string
	}{
		{"missing", successfulResult, "missing Claude conversation identity"},
		{"tagged helper only", strings.TrimSuffix(successfulResult, "}") + `,"session_id":"helper","parent_tool_use_id":"parent"}`, "missing Claude conversation identity"},
		{"tagged helper then root", `{"type":"system","subtype":"init","session_id":"helper","parent_tool_use_id":"parent"}` + "\n" + strings.TrimSuffix(successfulResult, "}") + `,"session_id":"expected"}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := jsonlOutput{sessionID: "expected"}
			writeChunks(t, &output, test.stream+"\n")
			output.finish()
			fails := test.failure != ""
			if (output.err != nil) != fails || fails && !strings.Contains(output.err.Error(), test.failure) || output.result.UsageComplete == fails || fails && output.result.Answer != "" {
				t.Fatalf("root identity: %+v,%v", output.result, output.err)
			}
		})
	}
}

func TestSessionIncompleteUsageDoesNotBecomeNextCallUsage(t *testing.T) {
	for _, test := range []struct {
		name, first string
		fails       bool
	}{
		{"missing aggregate", `{"type":"result","subtype":"success","result":"done","usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}`, false},
		{"zero aggregate with current root usage", `{"type":"result","subtype":"success","result":"done","usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11},"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`, false},
		{"late delegate", successfulResult + "\n" + strings.Replace(assistantUsageEvent("late", 20, 0, 0, 5), `"parent_tool_use_id":null`, `"parent_tool_use_id":"helper"`, 1), false},
		{"stream error after snapshot", successfulResult + "\n{", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const cumulative = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":93,"cacheCreationInputTokens":0,"cacheReadInputTokens":7,"outputTokens":100}}}`
			const later = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":97,"cacheCreationInputTokens":0,"cacheReadInputTokens":7,"outputTokens":106}}}`
			fakeExecutable(t, "claude", `n=0
[ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+test.first+`' ;;
2) printf '%s\n' '`+assistantUsageEvent("current", 4, 0, 0, 6)+`' '`+cumulative+`' ;;
3) printf '%s\n' '`+later+`' ;;
esac
`)
			var session Session
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			source := t.TempDir()
			initial, err := session.Run(t.Context(), source, "first", false, "", config.ClaudeConfig{})
			if (err != nil) != test.fails || initial.UsageComplete {
				t.Fatalf("expected incomplete first call (error=%v): %+v,%v", test.fails, initial, err)
			}
			second, err := session.Run(t.Context(), source, "second", false, "", config.ClaudeConfig{})
			want := usage.Tokens{Input: 4, Output: 6}
			if err != nil || second.UsageComplete || second.Tokens != want {
				t.Fatalf("prior usage leaked into next call: %+v,%v", second, err)
			}
			third, err := session.Run(t.Context(), source, "third", false, "", config.ClaudeConfig{})
			if err != nil || !third.UsageComplete || third.Tokens != want {
				t.Fatalf("fresh cumulative baseline not recovered: %+v,%v", third, err)
			}
		})
	}
}

func TestSessionCleansNativeScratchBeforeResume(t *testing.T) {
	fakeExecutable(t, "claude", `cat >/dev/null
[ -d "$CLAUDE_CODE_TMPDIR" ] && [ ! -L "$CLAUDE_CODE_TMPDIR" ] || exit 4
[ ! -e "$PWD/.git" ] && [ ! -L "$PWD/.git" ] || exit 5
printf '%s\n' '`+successfulResult+`'
`)
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	source := t.TempDir()
	if _, err := session.Run(t.Context(), source, "first", false, "", config.ClaudeConfig{}); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(session.runtimeDir, "tmp")
	gitDir := filepath.Join(session.runtimeDir, ".git")
	if err := os.Mkdir(gitDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\n\tfsmonitor = /tmp/not-run\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	canary := filepath.Join(outside, "canary")
	if err := os.WriteFile(canary, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, tmp); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(t.Context(), source, "resume", false, "", config.ClaudeConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(gitDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planted Git metadata survived resume: %v", err)
	}
	if data, err := os.ReadFile(canary); err != nil || string(data) != "unchanged" {
		t.Fatalf("native temp followed planted target: %q,%v", data, err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "plant"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, gitDir); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(t.Context(), source, "again", false, "", config.ClaudeConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(gitDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planted Git symlink survived resume: %v", err)
	}
	if data, err := os.ReadFile(canary); err != nil || string(data) != "unchanged" {
		t.Fatalf("Git cleanup followed planted target: %q,%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "plant")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native temp plant survived: %v", err)
	}
}

func TestSessionContradictedCumulativeCannotBecomeBaseline(t *testing.T) {
	native := func(input, cached, output int64) string {
		return fmt.Sprintf(`{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":%d,"cacheCreationInputTokens":0,"cacheReadInputTokens":%d,"outputTokens":%d}}}`, input-cached, cached, output)
	}
	for _, test := range []struct {
		name          string
		input, output int64
	}{
		{"regressed below baseline", 14, 10},
		{"omitted current messages", 16, 12},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeExecutable(t, "claude", `n=0
[ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+successfulResult+`' ;;
2) printf '%s\n' '`+assistantUsageEvent("second", 4, 0, 0, 6)+`' '`+native(test.input, 7, test.output)+`' ;;
3) printf '%s\n' '`+assistantUsageEvent("third", 2, 0, 0, 3)+`' '`+native(25, 7, 25)+`' ;;
4) printf '%s\n' '`+native(27, 7, 28)+`' ;;
esac
`)
			var session Session
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			source := t.TempDir()
			for call, want := range []usage.Tokens{{Input: 15, CachedInput: 7, Output: 11}, {Input: 4, Output: 6}, {Input: 2, Output: 3}, {Input: 2, Output: 3}} {
				result, err := session.Run(t.Context(), source, "task", false, "", config.ClaudeConfig{})
				complete := call == 0 || call == 3
				if err != nil || result.Tokens != want || result.UsageComplete != complete {
					t.Fatalf("call%d = %+v,%v want %+v complete=%v", call+1, result, err, want, complete)
				}
			}
		})
	}
}

func TestLateCoveredBlockKeepsExactCumulativeBaseline(t *testing.T) {
	output := jsonlOutput{}
	writeChunks(t, &output, assistantUsageEvent("covered", 3, 5, 7, 2)+"\n"+successfulResult+"\n"+assistantUsageEvent("covered", 3, 5, 7, 11)+"\n")
	output.finish()
	if output.err != nil || !output.result.UsageComplete || !output.hasCumulative || output.cumulative != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
		t.Fatalf("covered duplicate invalidated exact baseline: %+v,%v", output, output.err)
	}
}

func TestSessionUnlaunchedCallKeepsConversation(t *testing.T) {
	const aggregateOnly = `{"type":"result","subtype":"success","result":"final answer","modelUsage":{"claude-main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":11}}}`
	for _, failure := range []string{"cancelled", "missing interpreter"} {
		t.Run(failure, func(t *testing.T) {
			binary := fakeExecutable(t, "claude", "printf '%s\\n' \"$@\" > \"$0.args\"\ncat >/dev/null\nprintf '%s\\n' '"+aggregateOnly+"'\n")
			original, err := os.ReadFile(binary)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wantError := context.Canceled
			if failure == "cancelled" {
				cancel()
			} else {
				if err := os.WriteFile(binary, []byte("#!/minion-no-such-interpreter\n"), 0700); err != nil {
					t.Fatal(err)
				}
				wantError = os.ErrNotExist
			}
			var session Session
			t.Cleanup(func() {
				if err := session.Close(); err != nil {
					t.Error(err)
				}
			})
			source := t.TempDir()
			result, err := session.Run(ctx, source, "not launched", false, "", config.ClaudeConfig{})
			if !errors.Is(err, wantError) || !result.UsageComplete || result.Tokens != (usage.Tokens{}) || result.Answer != "" || session.started {
				t.Fatalf("unlaunched call: %+v, %v, started=%t", result, err, session.started)
			}
			id := session.id
			if err := os.WriteFile(binary, original, 0700); err != nil {
				t.Fatal(err)
			}
			result, err = session.Run(t.Context(), source, "first native call", false, "", config.ClaudeConfig{})
			args := readFile(t, binary+".args")
			if err != nil || !result.UsageComplete || !strings.Contains(args, "--session-id\n"+id+"\n") || strings.Contains(args, "--resume\n") {
				t.Fatalf("first native call resumed an unlaunched turn: %+v, %v, args=%q", result, err, args)
			}
			baseline, gap := session.baseline, session.usageGap
			if failure == "missing interpreter" {
				if err := os.WriteFile(binary, []byte("#!/minion-no-such-interpreter\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			result, err = session.Run(ctx, source, "resume not launched", false, "", config.ClaudeConfig{})
			if !errors.Is(err, wantError) || !result.UsageComplete || result.Tokens != (usage.Tokens{}) || !session.started || session.baseline != baseline || session.usageGap != gap {
				t.Fatalf("unlaunched resume changed prior accounting: %+v, %v, baseline=%+v gap=%t", result, err, session.baseline, session.usageGap)
			}
			if err := os.WriteFile(binary, original, 0700); err != nil {
				t.Fatal(err)
			}
			result, err = session.Run(t.Context(), source, "resume", false, "", config.ClaudeConfig{})
			if err != nil || !result.UsageComplete || result.Tokens != (usage.Tokens{}) || !strings.Contains(readFile(t, binary+".args"), "--resume\n"+id+"\n") {
				t.Fatalf("unlaunched resume lost its prior conversation: %+v, %v", result, err)
			}
		})
	}
}

func TestNewConversationClearsIncompleteUsageBaseline(t *testing.T) {
	const partial = `{"type":"result","subtype":"success","result":"partial","usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}`
	fakeExecutable(t, "claude", `cat >/dev/null
if [ -f "$0.first" ]; then
  printf '%s\n' '`+successfulResult+`'
else
  touch "$0.first"
  printf '%s\n' '`+partial+`'
fi
`)
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	source := t.TempDir()
	first, err := session.Run(t.Context(), source, "first", false, "", config.ClaudeConfig{})
	if err != nil || first.UsageComplete {
		t.Fatalf("missing aggregate was not incomplete: %+v, %v", first, err)
	}
	session.NewConversation()
	fresh, err := session.Run(t.Context(), source, "independent", false, "", config.ClaudeConfig{})
	if err != nil || !fresh.UsageComplete || fresh.Tokens != (usage.Tokens{Input: 15, CachedInput: 7, Output: 11}) {
		t.Fatalf("old usage gap survived independent conversation: %+v, %v", fresh, err)
	}
}

func TestSessionInitializationFailureStaysBound(t *testing.T) {
	binary := fakeExecutable(t, "claude", "touch \"$0.called\"\nexit 99\n")
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	settings := config.ClaudeConfig{Skills: []string{skill}}
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	source := t.TempDir()
	_, first := session.Run(t.Context(), source, "first", false, "", settings)
	if first == nil || !strings.Contains(first.Error(), "claude skills") {
		t.Fatalf("missing skill did not fail initialization: %v", first)
	}
	if err := os.WriteFile(skill, []byte("now present"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(t.Context(), source, "retry", false, "", settings); !errors.Is(err, first) {
		t.Fatalf("partially initialized session forgot its error: %v; want %v", err, first)
	}
	if _, err := os.Stat(binary + ".called"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partially initialized session reached CLI: %v", err)
	}
}

func TestSessionUsageFloorOverflowRetainsPreviousBaseline(t *testing.T) {
	const maximum = `{"type":"result","subtype":"success","result":"first","modelUsage":{"main":{"inputTokens":9223372036854775807,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`
	const partial = `{"type":"result","subtype":"success","result":"second","usage":{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}`
	fakeExecutable(t, "claude", `cat >/dev/null
if [ -f "$0.first" ]; then
  printf '%s\n' '`+partial+`'
else
  touch "$0.first"
  printf '%s\n' '`+maximum+`'
fi
`)
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	source := t.TempDir()
	first, err := session.Run(t.Context(), source, "first", false, "", config.ClaudeConfig{})
	if err != nil || !first.UsageComplete || first.Tokens != (usage.Tokens{Input: math.MaxInt64}) {
		t.Fatalf("at-limit baseline refused: %+v, %v", first, err)
	}
	second, err := session.Run(t.Context(), source, "second", false, "", config.ClaudeConfig{})
	if err == nil || !strings.Contains(err.Error(), "session usage totals overflow") || second.Answer != "" || second.UsageComplete || second.Tokens != (usage.Tokens{Input: 1}) || session.baseline != first.Tokens {
		t.Fatalf("overflow corrupted prior baseline: %+v, %v, baseline=%+v", second, err, session.baseline)
	}
}

func TestSessionTreatsNilAndEmptySkillsAsTheSameSelection(t *testing.T) {
	fakeExecutable(t, "claude", "cat >/dev/null\nprintf '%s\\n' '"+successfulResult+"'\n")
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	dir := t.TempDir()
	for call, selected := range [][]string{{}, nil, {}} {
		result, err := session.Run(t.Context(), dir, "task", false, "", config.ClaudeConfig{Skills: selected})
		if err != nil || result.Answer != "final answer" {
			t.Fatalf("call %d changed the empty skill selection: %+v %v", call, result, err)
		}
		if len(session.skillPaths) != 0 {
			t.Fatalf("call %d retained empty skill paths: %v", call, session.skillPaths)
		}
	}
}
