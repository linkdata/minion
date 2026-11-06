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

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/skills"
	"github.com/linkdata/minion/internal/testsupport"
	"github.com/linkdata/minion/internal/usage"
)

func TestSessionContinuesAndResetsAtStableRolePaths(t *testing.T) {
	const doubled = `{"type":"result","subtype":"success","result":"continued","total_cost_usd":1.25,"modelUsage":{"main":{"inputTokens":6,"cacheCreationInputTokens":10,"cacheReadInputTokens":14,"outputTokens":22}}}`
	firstWithCost := strings.Replace(successfulResult, `"modelUsage":`, `"total_cost_usd":0.5,"modelUsage":`, 1)
	binary := fakeExecutable(t, "claude", `n=0
[ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
printf '%s\n' "$@" > "$0.$n.args"
printf '%s\n' "$PWD" "$CLAUDE_CONFIG_DIR" > "$0.$n.paths"
if [ -e "$PWD/prior-notes" ] || [ -e "$GOCACHE/prior-notes" ]; then printf old; else printf clean; fi > "$0.$n.scratch"
cat >/dev/null
resume=false
for arg do [ "$arg" != --resume ] || resume=true; done
if $resume; then printf '%s\n' '`+doubled+`'; else printf '%s\n' '`+firstWithCost+`'; fi
`)
	workspace, oldGrant, newGrant := t.TempDir(), t.TempDir(), t.TempDir()
	settings := config.ClaudeConfig{APIKey: "fixture-private-key", Model: "fixed-model", Effort: "high", Skills: []string{skills.BuiltinAntiPromptInjection}, ReadOnlyDirs: []string{oldGrant}}
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	known := usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}
	var parentBefore os.FileInfo
	for call := 1; call <= 3; call++ {
		if call == 2 {
			settings.ReadOnlyDirs = []string{newGrant}
		}
		if call == 3 {
			parent, err := os.Stat(session.runtimeRoot)
			if err != nil {
				t.Fatal(err)
			}
			parentBefore = parent
			if err := os.WriteFile(filepath.Join(session.runtimeRoot, "parent-marker"), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{session.runtimeDir, session.cacheDir} {
				if err := os.WriteFile(filepath.Join(dir, "prior-notes"), []byte("previous critic findings"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			session.NewConversation()
			settings.ReadOnlyDirs = nil
		}
		result, err := session.Run(t.Context(), workspace, "task", false, "", settings)
		if err != nil || !result.UsageComplete || result.Tokens != known || result.UsageCoverage != (usage.Coverage{Scope: "invocation"}) {
			t.Fatalf("call%d: %+v, %v", call, result, err)
		}
		if call == 3 {
			parentAfter, err := os.Stat(session.runtimeRoot)
			if err != nil || !os.SameFile(parentBefore, parentAfter) {
				t.Fatalf("private runtime parent changed on reset: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(session.runtimeRoot, "parent-marker")); err != nil || string(got) != "keep" {
				t.Fatalf("private runtime parent lost marker on reset: %q, %v", got, err)
			}
		}
		wantCost := 0.5
		if call == 2 {
			wantCost = 0.75 // A resumed result is cumulative, so only its delta belongs to this call.
		}
		if result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != wantCost {
			t.Fatalf("call%d cost estimate=%v, want %.2f", call, result.EstimatedCostUSD, wantCost)
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
	rootInfo, err := os.Stat(session.runtimeRoot)
	if err != nil || rootInfo.Mode().Perm() != 0700 {
		t.Fatalf("private runtime root mode = %v, %v", rootInfo, err)
	}
	owned := []string{session.runtimeRoot, session.runtimeDir, session.authDir, session.embeddedDir}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if session.runtimeRoot != "" {
		t.Fatal("closed session retained runtime root")
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

func TestDocumentedTempRootLimit(t *testing.T) {
	if maxLinuxTempRootBytes != 56 || maxDarwinTempRootBytes != 56 {
		t.Fatalf("temp-root limits: Linux=%d macOS=%d", maxLinuxTempRootBytes, maxDarwinTempRootBytes)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), fmt.Sprintf("canonical path of at most %d bytes", maxTempRootBytes)) {
		t.Fatalf("README does not state the %d-byte temp-root limit", maxTempRootBytes)
	}
}

func TestSessionResumedAssistantFramesKeepUsageAndContent(t *testing.T) {
	start := func(id string) string {
		return fmt.Sprintf(`{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":%q}}}`, id)
	}
	frame := func(id, text string, input, output int) string {
		return fmt.Sprintf(`{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":%q,"content":[{"type":"text","text":%q}],"usage":{"input_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":%d}}}`, id, text, input, output)
	}
	final := func(text string, cost float64, input, output, totalInput, totalOutput int) string {
		return fmt.Sprintf(`{"type":"result","session_id":"native-id","subtype":"success","result":%q,"total_cost_usd":%g,"usage":{"input_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":%d},"modelUsage":{"main":{"inputTokens":%d,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":%d}}}`, text, cost, input, output, totalInput, totalOutput)
	}
	const helperStart = `{"type":"stream_event","agent_id":"helper","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"h3"}}}`
	const helperFrame = `{"type":"assistant","session_id":"native-id","agent_id":"helper","parent_tool_use_id":"delegate","message":{"id":"h3","content":[{"type":"text","text":"helper"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}}}`
	const thirdResult = `{"type":"result","session_id":"native-id","subtype":"success","result":"three","total_cost_usd":1,"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1},"modelUsage":{"main":{"inputTokens":35,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":13},"helper":{"inputTokens":2,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":3}}}`
	var script strings.Builder
	script.WriteString("n=0; [ ! -f \"$0.count\" ] || n=$(cat \"$0.count\")\nn=$((n+1)); printf '%s' \"$n\" > \"$0.count\"\ncat >/dev/null\ncase \"$n\" in\n")
	for i, call := range [][]string{
		{start("m1"), frame("m1", "one", 10, 5), final("one", 0.5, 10, 5, 10, 5)},
		{start("m2"), frame("m2", "two", 20, 7), final("two", 0.75, 20, 7, 30, 12)},
		{start("m3"), frame("m3", "three", 5, 1), helperStart, helperFrame, thirdResult},
	} {
		fmt.Fprintf(&script, "%d) printf '%%s\\n'", i+1)
		for _, event := range call {
			fmt.Fprintf(&script, " %s", aicli.ShellQuote(event))
		}
		script.WriteString(" ;;\n")
	}
	script.WriteString("esac\n")
	fakeExecutable(t, "claude", script.String())
	var session Session
	session.id = "native-id"
	t.Cleanup(func() { _ = session.Close() })
	workspace := t.TempDir()
	var events []aicli.StreamEvent
	session.OnEvent = func(event aicli.StreamEvent) { events = append(events, event) }
	want := []usage.Tokens{{Input: 10, Output: 5}, {Input: 20, Output: 7}, {Input: 7, Output: 4}}
	for i, tokens := range want {
		start := len(events)
		result, err := session.Run(t.Context(), workspace, "task", false, "", config.ClaudeConfig{})
		if err != nil || result.Tokens != tokens || !result.UsageComplete ||
			result.UsageCoverage != (usage.Coverage{Scope: "invocation"}) ||
			result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != []float64{0.5, 0.25, 0.25}[i] || !result.EstimatedCostComplete {
			t.Fatalf("call %d: result=%+v, error=%v", i+1, result, err)
		}
		found := false
		foundHelper := false
		for _, event := range events[start:] {
			if event.Kind == "assistant" && event.Text == []string{"one", "two", "three"}[i] {
				found = true
			}
			if event.Kind == "assistant" && event.Text == "helper" {
				foundHelper = true
			}
			if event.Kind == "content_missing" {
				t.Fatalf("call %d lost assistant content: %+v", i+1, events[start:])
			}
		}
		if !found || i == 2 && !foundHelper {
			t.Fatalf("call %d omitted assistant content: %+v", i+1, events[start:])
		}
	}
}

func TestSessionResumedAssistantFrameNeedsCoveringResult(t *testing.T) {
	const first = `{"type":"result","session_id":"native-id","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":100}}}`
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"unproved"}}}`
	const frame = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"unproved","content":[{"type":"text","text":"prior work"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":100}}}`
	const second = `{"type":"result","session_id":"native-id","subtype":"success","result":"second","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":105}}}`
	fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
if [ "$n" -eq 1 ]; then printf '%s\n' '`+first+`'; else printf '%s\n' '`+start+`' '`+frame+`' '`+second+`'; fi
`)
	var session Session
	session.id = "native-id"
	t.Cleanup(func() { _ = session.Close() })
	var events []aicli.StreamEvent
	session.OnEvent = func(event aicli.StreamEvent) { events = append(events, event) }
	workspace := t.TempDir()
	if _, err := session.Run(t.Context(), workspace, "first", false, "", config.ClaudeConfig{}); err != nil {
		t.Fatal(err)
	}
	result, err := session.Run(t.Context(), workspace, "second", false, "", config.ClaudeConfig{})
	if err != nil || result.Tokens != (usage.Tokens{Output: 5}) || result.UsageComplete ||
		result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != 0.25 || result.EstimatedCostComplete {
		t.Fatalf("underreported resumed frame: %+v, %v", result, err)
	}
	missing := 0
	for _, event := range events {
		if event.Text == "prior work" {
			t.Fatalf("unproved resumed content escaped: %+v", events)
		}
		if event.Kind == "content_missing" {
			missing++
		}
	}
	if missing != 1 {
		t.Fatalf("unproved resumed frame lacked one content marker: %+v", events)
	}
}

func TestSessionCostEstimateNeedsContiguousSnapshots(t *testing.T) {
	const resumed = `{"type":"result","subtype":"success","result":"continued","total_cost_usd":1.25,"modelUsage":{"main":{"inputTokens":6,"cacheCreationInputTokens":10,"cacheReadInputTokens":14,"outputTokens":22}}}`
	firstWithCost := strings.Replace(successfulResult, `"modelUsage":`, `"total_cost_usd":0.25,"modelUsage":`, 1)
	fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+successfulResult+`' ;;
2) printf '%s\n' '`+resumed+`' ;;
*) printf '%s\n' '`+firstWithCost+`' ;;
esac
`)
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	dir := t.TempDir()
	for call := 1; call <= 2; call++ {
		result, err := session.Run(t.Context(), dir, "task", false, "", config.ClaudeConfig{})
		if err != nil || result.EstimatedCostUSD != nil {
			t.Fatalf("call%d attributed cost across a missing baseline: result=%+v error=%v", call, result, err)
		}
	}
	session.NewConversation()
	result, err := session.Run(t.Context(), dir, "task", false, "", config.ClaudeConfig{})
	if err != nil || result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != 0.25 {
		t.Fatalf("fresh conversation did not restore cost attribution: result=%+v error=%v", result, err)
	}
}

func TestSessionResumedUsageUsesInstalledCLIScope(t *testing.T) {
	const first = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":11},"helper":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	const secondOld = `{"type":"result","subtype":"success","result":"continued","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":11},"helper":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	const secondCumulative = `{"type":"result","subtype":"success","result":"continued","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":6,"cacheCreationInputTokens":10,"cacheReadInputTokens":14,"outputTokens":22},"helper":{"inputTokens":2,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":4}}}`
	cost := func(value float64) *float64 { return &value }
	for _, tc := range []struct {
		version, second string
		want            *float64
	}{
		{version: "2.1.276", second: secondOld, want: cost(0.75)},
		{version: "2.1.277", second: secondCumulative, want: cost(0.25)},
		{version: "2.2.0", second: secondCumulative},
		{version: "unrecognized", second: secondCumulative},
	} {
		t.Run(tc.version, func(t *testing.T) {
			binary := fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
if [ "$n" -eq 1 ]; then printf '%s\n' '`+first+`'; else printf '%s\n' '`+tc.second+`'; fi
`)
			if err := os.WriteFile(binary+".version", []byte(tc.version+" (Claude Code)\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var session Session
			t.Cleanup(func() { _ = session.Close() })
			dir := t.TempDir()
			initial, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{})
			if err != nil || initial.EstimatedCostUSD == nil || *initial.EstimatedCostUSD != 0.5 {
				t.Fatalf("initial cost: %+v, %v", initial, err)
			}
			resumed, err := session.Run(t.Context(), dir, "second", false, "", config.ClaudeConfig{})
			if err != nil || (resumed.EstimatedCostUSD == nil) != (tc.want == nil) ||
				tc.want != nil && *resumed.EstimatedCostUSD != *tc.want {
				t.Fatalf("resumed cost for %s: %+v, %v", tc.version, resumed, err)
			}
			if tc.want != nil && (!resumed.UsageComplete || resumed.Tokens != initial.Tokens) ||
				tc.want == nil && resumed.UsageComplete {
				t.Fatalf("resumed token scope for %s: initial=%+v resumed=%+v", tc.version, initial, resumed)
			}
		})
	}
}

func TestSessionRecoversAfterInitialVersionProbeFailure(t *testing.T) {
	first := strings.Replace(successfulResult, `"modelUsage":`, `"total_cost_usd":0.5,"modelUsage":`, 1)
	const second = `{"type":"result","subtype":"success","result":"continued","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":6,"cacheCreationInputTokens":10,"cacheReadInputTokens":14,"outputTokens":22}}}`
	binary := fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
if [ "$n" -eq 1 ]; then printf '%s\n' '`+first+`'; else printf '%s\n' '`+second+`'; fi
`)
	if err := os.WriteFile(binary+".version", []byte("unavailable\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	workspace := t.TempDir()
	initial, err := session.Run(t.Context(), workspace, "first", false, "", config.ClaudeConfig{})
	if err != nil || !initial.UsageComplete || initial.EstimatedCostUSD == nil || *initial.EstimatedCostUSD != 0.5 {
		t.Fatalf("initial call: %+v, %v", initial, err)
	}
	if err := os.WriteFile(binary+".version", []byte("2.1.318 (Claude Code)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resumed, err := session.Run(t.Context(), workspace, "second", false, "", config.ClaudeConfig{})
	if err != nil || !resumed.UsageComplete || resumed.Tokens != initial.Tokens ||
		resumed.EstimatedCostUSD == nil || *resumed.EstimatedCostUSD != 0.25 || !resumed.EstimatedCostComplete {
		t.Fatalf("resumed call after recovered version probe: %+v, %v", resumed, err)
	}
}

func TestSessionOlderPerCallCostRecoversAfterMissingFirstSnapshot(t *testing.T) {
	second := strings.Replace(successfulResult, `"modelUsage":`, `"total_cost_usd":0.75,"modelUsage":`, 1)
	binary := fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
if [ "$n" -eq 1 ]; then printf '%s\n' '`+successfulResult+`'; else printf '%s\n' '`+second+`'; fi
`)
	if err := os.WriteFile(binary+".version", []byte("2.1.276 (Claude Code)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	dir := t.TempDir()
	first, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{})
	if err != nil || first.EstimatedCostUSD != nil {
		t.Fatalf("first missing cost: %+v, %v", first, err)
	}
	resumed, err := session.Run(t.Context(), dir, "second", false, "", config.ClaudeConfig{})
	if err != nil || resumed.EstimatedCostUSD == nil || *resumed.EstimatedCostUSD != 0.75 ||
		!resumed.UsageComplete || resumed.Tokens != first.Tokens {
		t.Fatalf("older per-call cost failed to recover: %+v, %v", resumed, err)
	}
}

func TestSessionOlderPerCallReplayDoesNotDuplicateTokens(t *testing.T) {
	prior := assistantUsageEvent("prior", 100, 0, 0, 50)
	fresh := assistantUsageEvent("fresh", 1, 0, 0, 1)
	const freshStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"fresh"}}}`
	const first = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":100,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":50}}}`
	const second = `{"type":"result","subtype":"success","result":"second","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	binary := fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
if [ "$n" -eq 1 ]; then printf '%s\n' '`+prior+`' '`+first+`'
else printf '%s\n' '`+prior+`' '`+freshStart+`' '`+fresh+`' '`+second+`' '`+prior+`'; fi
`)
	if err := os.WriteFile(binary+".version", []byte("2.1.276 (Claude Code)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	dir := t.TempDir()
	if firstResult, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{}); err != nil ||
		!firstResult.UsageComplete || firstResult.Tokens != (usage.Tokens{Input: 100, Output: 50}) {
		t.Fatalf("first per-call result: %+v, %v", firstResult, err)
	}
	resumed, err := session.Run(t.Context(), dir, "second", false, "", config.ClaudeConfig{})
	if err != nil || !resumed.UsageComplete || resumed.Tokens != (usage.Tokens{Input: 1, Output: 1}) ||
		resumed.EstimatedCostUSD == nil || *resumed.EstimatedCostUSD != 0.75 || !resumed.EstimatedCostComplete {
		t.Fatalf("old replay changed current call: %+v, %v", resumed, err)
	}
}

func TestSessionUnseenResumedFullFrameDoesNotRebillPriorUsage(t *testing.T) {
	const first = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":100}}}`
	const old = `{"type":"assistant","session_id":"native-id","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"historical root text"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":100}}}`
	const current = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"current","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}}}`
	const second = `{"type":"result","subtype":"success","result":"second","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":105}}}`
	binary := fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
if [ "$n" -eq 1 ]; then cat <<'FIRST'
`+first+`
FIRST
else cat <<'SECOND'
`+old+`
`+current+`
`+second+`
SECOND
fi
`)
	if err := os.WriteFile(binary+".version", []byte("2.1.277 (Claude Code)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var session Session
	session.id = "native-id"
	var events []aicli.StreamEvent
	session.OnEvent = func(e aicli.StreamEvent) { events = append(events, e) }
	t.Cleanup(func() { _ = session.Close() })
	dir := t.TempDir()
	initial, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{})
	if err != nil || initial.Tokens != (usage.Tokens{Output: 100}) || !initial.UsageComplete ||
		initial.EstimatedCostUSD == nil || *initial.EstimatedCostUSD != 0.5 || !initial.EstimatedCostComplete {
		t.Fatalf("first result lacked a complete baseline: result=%+v error=%v", initial, err)
	}
	resumed, err := session.Run(t.Context(), dir, "second", false, "", config.ClaudeConfig{})
	if err != nil || resumed.Answer != "second" || resumed.Tokens != (usage.Tokens{Output: 5}) ||
		resumed.UsageComplete || resumed.UsageCoverage.Scope != "known_subset" || !resumed.UsageCoverage.PartialReport ||
		resumed.EstimatedCostUSD == nil || *resumed.EstimatedCostUSD != 0.25 || resumed.EstimatedCostComplete ||
		session.baseline != (usage.Tokens{Output: 105}) {
		t.Fatalf("unseen old frame changed resumed subtotal: result=%+v baseline=%+v error=%v", resumed, session.baseline, err)
	}
	missing := 0
	for _, event := range events {
		if strings.Contains(event.Text, "historical root text") {
			t.Fatalf("old root content entered resumed receipt: %+v", events)
		}
		if event == (aicli.StreamEvent{Kind: "content_missing"}) {
			missing++
		}
	}
	if missing != 1 {
		t.Fatalf("unproved root content lacked one receipt gap marker: %+v", events)
	}
}

func TestSessionMissingAssistantIDUsesFreshFloorButNotResumedCounter(t *testing.T) {
	const missing = `{"type":"assistant","parent_tool_use_id":null,"message":{"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":10}}}`
	const first = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5},"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}}`
	const second = `{"type":"result","subtype":"success","result":"second","total_cost_usd":0.75,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5},"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":15}}}`
	binary := fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
if [ "$n" -eq 1 ]; then cat <<'FIRST'
`+missing+`
`+first+`
FIRST
else cat <<'SECOND'
`+strings.Replace(missing, `"output_tokens":10`, `"output_tokens":100`, 1)+`
`+second+`
SECOND
fi
`)
	if err := os.WriteFile(binary+".version", []byte("2.1.277 (Claude Code)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	dir := t.TempDir()
	initial, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{})
	if err != nil || initial.Answer != "first" || initial.Tokens != (usage.Tokens{Output: 10}) ||
		initial.UsageComplete || initial.EstimatedCostUSD == nil || *initial.EstimatedCostUSD != 0.5 ||
		initial.EstimatedCostComplete || session.baseline != (usage.Tokens{Output: 10}) {
		t.Fatalf("fresh missing ID lost known floor: result=%+v baseline=%+v error=%v", initial, session.baseline, err)
	}
	resumed, err := session.Run(t.Context(), dir, "second", false, "", config.ClaudeConfig{})
	if err != nil || resumed.Answer != "second" || resumed.Tokens != (usage.Tokens{Output: 5}) ||
		resumed.UsageComplete || !resumed.UsageCoverage.CumulativeUnattributed ||
		resumed.EstimatedCostUSD != nil || resumed.EstimatedCostComplete ||
		session.baseline != (usage.Tokens{Output: 15}) {
		t.Fatalf("resumed missing ID allocated old counters: result=%+v baseline=%+v error=%v", resumed, session.baseline, err)
	}
}

func TestSessionOlderPerCallRejectsConflictingAssistantCounters(t *testing.T) {
	first := assistantUsageEvent("same", 0, 10, 0, 1)
	conflict := assistantUsageEvent("same", 0, 0, 10, 1)
	const resultLine = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":10,"cacheReadInputTokens":0,"outputTokens":1}}}`
	binary := fakeExecutable(t, "claude", "cat >/dev/null\nprintf '%s\\n' '"+first+"' '"+conflict+"' '"+resultLine+"'\n")
	if err := os.WriteFile(binary+".version", []byte("2.1.276 (Claude Code)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if err != nil || result.UsageComplete {
		t.Fatalf("conflicting assistant cache counters were certified complete: %+v, %v", result, err)
	}
}

func TestSessionOlderPerCallLateHistoricalRootDoesNotDoubleCount(t *testing.T) {
	prior := assistantUsageEvent("prior", 0, 0, 0, 1)
	priorIncreased := assistantUsageEvent("prior", 0, 0, 0, 2)
	priorIncreasedAgain := assistantUsageEvent("prior", 0, 0, 0, 3)
	const first = `{"type":"result","subtype":"success","result":"first","modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const second = `{"type":"result","subtype":"success","result":"second","modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	for _, afterResult := range []bool{false, true} {
		t.Run(fmt.Sprintf("after result=%t", afterResult), func(t *testing.T) {
			secondOutput := "'" + priorIncreased + "' '" + priorIncreasedAgain + "' '" + second + "'"
			if afterResult {
				secondOutput = "'" + second + "' '" + priorIncreased + "' '" + priorIncreasedAgain + "'"
			}
			binary := fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
if [ "$n" -eq 1 ]; then printf '%s\n' '`+prior+`' '`+first+`'
else printf '%s\n' `+secondOutput+`; fi
`)
			if err := os.WriteFile(binary+".version", []byte("2.1.276 (Claude Code)\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var session Session
			t.Cleanup(func() { _ = session.Close() })
			dir := t.TempDir()
			if _, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{}); err != nil {
				t.Fatal(err)
			}
			resumed, err := session.Run(t.Context(), dir, "second", false, "", config.ClaudeConfig{})
			if (err != nil) != afterResult || resumed.Tokens.Output != 1 || resumed.UsageComplete {
				t.Fatalf("historical root revisions were lost or double counted: %+v, %v", resumed, err)
			}
		})
	}
}

func TestSessionUpgradeDoesNotReattributeOldPerCallUsage(t *testing.T) {
	const first = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":10,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const second = `{"type":"result","subtype":"success","result":"second","total_cost_usd":1.75,"modelUsage":{"main":{"inputTokens":30,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":3}}}`
	const third = `{"type":"result","subtype":"success","result":"third","total_cost_usd":2.0,"modelUsage":{"main":{"inputTokens":40,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":4}}}`
	binary := fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+first+`' ;;
2) printf '%s\n' '`+second+`' ;;
*) printf '%s\n' '`+third+`' ;;
esac
`)
	versionPath := binary + ".version"
	if err := os.WriteFile(versionPath, []byte("2.1.276 (Claude Code)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	dir := t.TempDir()
	old, err := session.Run(t.Context(), dir, "old", false, "", config.ClaudeConfig{})
	if err != nil || !old.UsageComplete || old.Tokens != (usage.Tokens{Input: 10, Output: 1}) ||
		old.EstimatedCostUSD == nil || *old.EstimatedCostUSD != 0.5 {
		t.Fatalf("first old per-call result: %+v, %v", old, err)
	}
	if err := os.WriteFile(versionPath, []byte("2.1.277 (Claude Code)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	transition, err := session.Run(t.Context(), dir, "upgraded", false, "", config.ClaudeConfig{})
	if err != nil || transition.UsageComplete || transition.EstimatedCostUSD != nil ||
		!transition.UsageCoverage.CumulativeUnattributed {
		t.Fatalf("upgrade reattributed older usage: %+v, %v", transition, err)
	}
	reanchored, err := session.Run(t.Context(), dir, "after upgrade", false, "", config.ClaudeConfig{})
	if err != nil || !reanchored.UsageComplete || reanchored.Tokens != (usage.Tokens{Input: 10, Output: 1}) ||
		reanchored.EstimatedCostUSD == nil || *reanchored.EstimatedCostUSD != 0.25 {
		t.Fatalf("upgraded session did not reanchor: %+v, %v", reanchored, err)
	}
}

func TestSessionCumulativeCostReanchorsAfterMissingSnapshot(t *testing.T) {
	const first = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const second = `{"type":"result","subtype":"success","result":"second","modelUsage":{"main":{"inputTokens":2,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	const third = `{"type":"result","subtype":"success","result":"third","total_cost_usd":1.5,"modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":3}}}`
	const fourth = `{"type":"result","subtype":"success","result":"fourth","total_cost_usd":2.0,"modelUsage":{"main":{"inputTokens":4,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":4}}}`
	fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+first+`' ;;
2) printf '%s\n' '`+second+`' ;;
3) printf '%s\n' '`+third+`' ;;
*) printf '%s\n' '`+fourth+`' ;;
esac
`)
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	dir := t.TempDir()
	for call := 1; call <= 4; call++ {
		result, err := session.Run(t.Context(), dir, "turn", false, "", config.ClaudeConfig{})
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		if call == 1 || call == 4 {
			want := 0.5
			if result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != want {
				t.Fatalf("call %d cost after reanchor: %+v", call, result)
			}
		} else if result.EstimatedCostUSD != nil {
			t.Fatalf("call %d attributed cost across gap: %+v", call, result)
		}
	}
}

func TestSessionBackwardCostCannotReanchor(t *testing.T) {
	t.Run("after gap", func(t *testing.T) {
		const first = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
		const missing = `{"type":"result","subtype":"success","result":"missing","modelUsage":{"main":{"inputTokens":2,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
		const backward = `{"type":"result","subtype":"success","result":"backward","total_cost_usd":0.1,"modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":3}}}`
		const later = `{"type":"result","subtype":"success","result":"later","total_cost_usd":0.2,"modelUsage":{"main":{"inputTokens":4,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":4}}}`
		fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+first+`' ;;
2) printf '%s\n' '`+missing+`' ;;
3) printf '%s\n' '`+backward+`' ;;
*) printf '%s\n' '`+later+`' ;;
esac
`)
		var session Session
		t.Cleanup(func() { _ = session.Close() })
		dir := t.TempDir()
		for call := 1; call <= 4; call++ {
			result, err := session.Run(t.Context(), dir, "turn", false, "", config.ClaudeConfig{})
			if err != nil || call > 1 && (result.EstimatedCostUSD != nil || result.EstimatedCostComplete) {
				t.Fatalf("call %d attributed cost from a backward snapshot: %+v, %v", call, result, err)
			}
		}
	})
	t.Run("provisional decrease", func(t *testing.T) {
		const provisional = `{"type":"result","subtype":"success","result_index":0,"result":"early","total_cost_usd":0.8,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
		const final = `{"type":"result","subtype":"success","result_index":1,"result":"final","total_cost_usd":0.6,"modelUsage":{"main":{"inputTokens":2,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
		fakeExecutable(t, "claude", "cat >/dev/null\nprintf '%s\\n' '"+provisional+"' '"+final+"'\n")
		var session Session
		t.Cleanup(func() { _ = session.Close() })
		result, err := session.Run(t.Context(), t.TempDir(), "turn", false, "", config.ClaudeConfig{})
		if err != nil || result.EstimatedCostUSD != nil || result.EstimatedCostComplete {
			t.Fatalf("contradictory final cost retained an assured estimate: %+v, %v", result, err)
		}
	})
	t.Run("missing intermediate snapshot", func(t *testing.T) {
		const first = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
		const gap = `{"type":"result","subtype":"success","result":"gap","modelUsage":{"main":{"inputTokens":2,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
		const provisional = `{"type":"result","subtype":"success","result_index":0,"result":"early","total_cost_usd":0.8,"modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":3}}}`
		const missing = `{"type":"result","subtype":"success","result_index":1,"result":"middle","modelUsage":{"main":{"inputTokens":4,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":4}}}`
		const backward = `{"type":"result","subtype":"success","result_index":2,"result":"final","total_cost_usd":0.6,"modelUsage":{"main":{"inputTokens":5,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}}`
		const later = `{"type":"result","subtype":"success","result":"later","total_cost_usd":0.7,"modelUsage":{"main":{"inputTokens":6,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":6}}}`
		const floor = `{"type":"result","subtype":"success","result":"floor","total_cost_usd":0.8,"modelUsage":{"main":{"inputTokens":7,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":7}}}`
		const recovered = `{"type":"result","subtype":"success","result":"recovered","total_cost_usd":0.9,"modelUsage":{"main":{"inputTokens":8,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":8}}}`
		fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+first+`' ;;
2) printf '%s\n' '`+gap+`' ;;
3) printf '%s\n' '`+provisional+`' '`+missing+`' '`+backward+`' ;;
4) printf '%s\n' '`+later+`' ;;
5) printf '%s\n' '`+floor+`' ;;
*) printf '%s\n' '`+recovered+`' ;;
esac
`)
		var session Session
		t.Cleanup(func() { _ = session.Close() })
		dir := t.TempDir()
		for call := 1; call <= 6; call++ {
			result, err := session.Run(t.Context(), dir, "turn", false, "", config.ClaudeConfig{})
			if err != nil {
				t.Fatalf("call %d: %v", call, err)
			}
			if call >= 2 && call <= 5 && (result.EstimatedCostUSD != nil || result.EstimatedCostComplete) {
				t.Fatalf("call %d reanchored below an earlier snapshot: %+v, %v", call, result, err)
			}
			if call == 6 && (result.EstimatedCostUSD == nil || math.Abs(*result.EstimatedCostUSD-0.1) > 1e-9 || !result.EstimatedCostComplete) {
				t.Fatalf("call %d did not recover after a safe reanchor: %+v", call, result)
			}
		}
	})
	t.Run("version probe gap", func(t *testing.T) {
		const first = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
		const provisional = `{"type":"result","subtype":"success","result_index":0,"result":"early","total_cost_usd":0.8,"modelUsage":{"main":{"inputTokens":2,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
		const missing = `{"type":"result","subtype":"success","result_index":1,"result":"final","modelUsage":{"main":{"inputTokens":3,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":3}}}`
		const lower = `{"type":"result","subtype":"success","result":"lower","total_cost_usd":0.7,"modelUsage":{"main":{"inputTokens":4,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":4}}}`
		const floor = `{"type":"result","subtype":"success","result":"floor","total_cost_usd":0.8,"modelUsage":{"main":{"inputTokens":5,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":5}}}`
		const recovered = `{"type":"result","subtype":"success","result":"recovered","total_cost_usd":0.9,"modelUsage":{"main":{"inputTokens":6,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":6}}}`
		binary := fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+first+`' ;;
2) printf '%s\n' '`+provisional+`' '`+missing+`' ;;
3) printf '%s\n' '`+lower+`' ;;
4) printf '%s\n' '`+floor+`' ;;
*) printf '%s\n' '`+recovered+`' ;;
esac
`)
		versionPath := binary + ".version"
		var session Session
		t.Cleanup(func() { _ = session.Close() })
		dir := t.TempDir()
		for call := 1; call <= 5; call++ {
			if call == 2 {
				if err := os.WriteFile(versionPath, []byte("probe unavailable\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if call == 3 {
				if err := os.WriteFile(versionPath, []byte("2.1.316 (Claude Code)\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := session.Run(t.Context(), dir, "turn", false, "", config.ClaudeConfig{})
			if err != nil {
				t.Fatalf("call %d: %v", call, err)
			}
			if call >= 2 && call <= 4 && (result.EstimatedCostUSD != nil || result.EstimatedCostComplete) {
				t.Fatalf("call %d attributed cost below an unknown-scope floor: %+v", call, result)
			}
			if call == 5 && (result.EstimatedCostUSD == nil || math.Abs(*result.EstimatedCostUSD-0.1) > 1e-9 || !result.EstimatedCostComplete) {
				t.Fatalf("call %d did not recover after the probe and cost reanchored: %+v", call, result)
			}
		}
	})
}

func TestSessionLateHelperUsageLeavesCostIncomplete(t *testing.T) {
	const final = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	helper := strings.Replace(assistantUsageEvent("helper", 0, 0, 0, 2), `"parent_tool_use_id":null`, `"parent_tool_use_id":"delegate"`, 1)
	fakeExecutable(t, "claude", "cat >/dev/null\nprintf '%s\\n' '"+final+"' '"+helper+"'\n")
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.Run(t.Context(), t.TempDir(), "turn", false, "", config.ClaudeConfig{})
	if err != nil || result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != 0.5 || result.EstimatedCostComplete {
		t.Fatalf("late helper left native cost falsely complete: %+v, %v", result, err)
	}
}

func TestSessionLateHelperStreamLeavesCostIncomplete(t *testing.T) {
	const final = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const start = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"helper"}}}`
	const missingStart = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{}}}`
	const delta = `{"type":"stream_event","uuid":"chunk","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"later work"}}}`
	unindexedDelta := strings.Replace(delta, `"index":0,`, "", 1)
	const changedDelta = `{"type":"stream_event","uuid":"chunk","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"new work"}}}`
	untypedDelta := strings.Replace(changedDelta, `"type":"text_delta",`, "", 1)
	mixedDelta := strings.Replace(delta, `"text":"later work"`, `"text":"later work","thinking":"new work"`, 1)
	unknownDelta := strings.Replace(delta, `"text":"later work"`, `"text":"later work","new_content":"unproved"`, 1)
	const unknownNested = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"delta":{"text":"new work"}}}`
	const messageDeltaUsage = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_delta","delta":{"usage":{"output_tokens":2}}}}`
	const eventUsage = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_delta","usage":{"output_tokens":2}}}`
	messageStartUsage := strings.Replace(start, `"id":"helper"`, `"id":"helper","usage":{"output_tokens":2}`, 1)
	const stop = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_stop"}}`
	const toolStart = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool","name":"Task","input":{}}}}`
	changedToolStart := strings.Replace(toolStart, `"id":"tool"`, `"id":"other-tool"`, 1)
	changedToolName := strings.Replace(toolStart, `"name":"Task"`, `"name":"Bash"`, 1)
	changedToolInput := strings.Replace(toolStart, `"input":{}`, `"input":{"command":"true"}`, 1)
	const full = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[{"type":"tool_use","id":"tool","name":"Task","input":{}}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const fullWithSecondTool = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[{"type":"text","text":"later work"},{"type":"tool_use","id":"tool","name":"Task","input":{}}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const textFull = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[{"type":"text","text":"later work"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const emptyFullWithMoreUsage = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2}}}`
	const emptyFullWithMalformedUsage = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[],"usage":{}}}`
	const emptyFullWithBadOutput = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[],"usage":{"output_tokens":"bad"}}}`
	const whitespaceEmptyFull = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[ ],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	changedTextFull := strings.Replace(textFull, `"text":"later work"`, `"text":"new work"`, 1)
	malformedToolFull := strings.Replace(full, `"id":"tool",`, "", 1)
	const mixedToolFull = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[7,{"type":"tool_use","id":"tool","name":"Task","input":{}}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const toolStop = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"content_block_stop","index":0}}`
	const nullDelta = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","index":0,"delta":null}}`
	const emptyDelta = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","index":0,"delta":{}}}`
	missingTextDelta := strings.Replace(delta, `,"text":"later work"`, "", 1)
	const contentStop = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_stop","delta":{"type":"text_delta","text":"new work"}}}`
	contentStart := strings.Replace(start, `"id":"helper"`, `"id":"helper","content":[{"type":"text","text":"new work"}]`, 1)
	unindexedToolStop := strings.Replace(toolStop, `,"index":0`, "", 1)
	toolAtSecondIndex := strings.Replace(toolStart, `"index":0`, `"index":1`, 1)
	const malformedDelta = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","index":"bad","delta":{"type":"text_delta","text":"later work"}}}`
	unindexedToolStart := strings.Replace(toolStart, `"index":0,`, "", 1)
	nullToolStart := strings.Replace(toolStart, `"content_block":{"type":"tool_use","id":"tool","name":"Task","input":{}}`, `"content_block":null`, 1)
	oldHelper := strings.Replace(assistantUsageEvent("helper", 0, 0, 0, 1), `"parent_tool_use_id":null`, `"parent_tool_use_id":"delegate"`, 1)
	otherStart := strings.Replace(start, `"id":"helper"`, `"id":"other-helper"`, 1)
	otherDelta := strings.Replace(delta, `"uuid":"chunk"`, `"uuid":"other-chunk"`, 1)
	crossKeyStart := strings.Replace(start, `"agent_id":"worker"`, `"agent_id":"other-worker"`, 1)
	crossKeyDelta := strings.Replace(delta, `"agent_id":"worker"`, `"agent_id":"other-worker"`, 1)
	for _, tc := range []struct {
		name, before, after string
		complete            bool
	}{
		{"new helper", "", start + "\n" + delta, false},
		{"repeated start then new content", start, start + "\n" + delta, false},
		{"missing ID then new content", missingStart, delta, false},
		{"replayed content", start + "\n" + delta, delta, true},
		{"replayed content after stop", start + "\n" + delta + "\n" + stop, start + "\n" + delta, true},
		{"new content after stop", start + "\n" + delta + "\n" + stop, start + "\n" + changedDelta, false},
		{"old ID with new content", oldHelper, start + "\n" + delta, false},
		{"sealed tool stop", start + "\n" + toolStart + "\n" + full, toolStop, true},
		{"sealed exact tool start", start + "\n" + toolStart + "\n" + full, toolStart, true},
		{"sealed changed tool start", start + "\n" + toolStart + "\n" + full, changedToolStart, false},
		{"sealed changed tool name", start + "\n" + toolStart + "\n" + full, changedToolName, false},
		{"sealed changed tool input", start + "\n" + toolStart + "\n" + full, changedToolInput, false},
		{"first late full frame changes proved tool", start + "\n" + toolStart, strings.Replace(full, `"id":"tool"`, `"id":"other-tool"`, 1), false},
		{"unindexed late tool start", start + "\n" + toolStart + "\n" + full, unindexedToolStart, false},
		{"null late tool block", start + "\n" + toolStart + "\n" + full, nullToolStart, false},
		{"full frame alone cannot prove late tool start", start + "\n" + delta + "\n" + fullWithSecondTool, toolAtSecondIndex, false},
		{"late full frame adds unseen tool", start + "\n" + delta, fullWithSecondTool, false},
		{"first matching full frame after result remains unproven", start + "\n" + delta, textFull, false},
		{"first changed full text after result remains unproven", start + "\n" + delta, changedTextFull, false},
		{"first malformed full tool after result remains unproven", start + "\n" + delta, malformedToolFull, false},
		{"first mixed full tool after result remains unproven", start + "\n" + delta, mixedToolFull, false},
		{"empty full frame has usage beyond result", start + "\n" + delta, emptyFullWithMoreUsage, false},
		{"empty full frame has malformed usage", start + "\n" + delta, emptyFullWithMalformedUsage, false},
		{"empty full frame has bad output counter", start + "\n" + delta, emptyFullWithBadOutput, false},
		{"whitespace empty full frame", start + "\n" + delta, whitespaceEmptyFull, true},
		{"sealed exact delta", start + "\n" + delta + "\n" + textFull, delta, true},
		{"sealed changed delta", start + "\n" + delta + "\n" + textFull, changedDelta, false},
		{"malformed nested delta", start + "\n" + delta, malformedDelta, false},
		{"untyped late delta", start + "\n" + delta, untypedDelta, false},
		{"early unindexed delta is not replay proof", start + "\n" + unindexedDelta, delta, false},
		{"early mixed delta is not replay proof", start + "\n" + mixedDelta, delta, false},
		{"early unknown delta field is not replay proof", start + "\n" + unknownDelta, delta, false},
		{"mixed late delta payload", start + "\n" + delta, mixedDelta, false},
		{"unknown late delta field", start + "\n" + delta, unknownDelta, false},
		{"content-bearing unknown nested event", start + "\n" + delta, unknownNested, false},
		{"late unindexed tool stop", start + "\n" + unindexedToolStart, unindexedToolStop, false},
		{"indexed stop without proved start", start + "\n" + unindexedToolStart, toolStop, false},
		{"null late delta", start + "\n" + delta, nullDelta, false},
		{"empty late delta", start + "\n" + delta, emptyDelta, false},
		{"empty late delta without stream", "", emptyDelta, false},
		{"missing selected late delta field", start + "\n" + delta, missingTextDelta, false},
		{"content on message stop", start + "\n" + delta, contentStop, false},
		{"content on replayed message start", start + "\n" + delta, contentStart, false},
		{"mixed tool start replay", start + "\n" + toolStart + "\n" + full, strings.Replace(toolStart, `"input":{}`, `"input":{},"text":"new work"`, 1), false},
		{"tool start replay adds content field", start + "\n" + toolStart + "\n" + full, strings.Replace(toolStart, `"input":{}`, `"input":{},"content":"new work"`, 1), false},
		{"late nested message usage", start + "\n" + delta, messageDeltaUsage, false},
		{"late event usage", start + "\n" + delta, eventUsage, false},
		{"replayed start with unanchored usage", start + "\n" + delta, messageStartUsage, false},
		{"old ID after key replacement", start + "\n" + delta + "\n" + stop + "\n" + otherStart + "\n" + otherDelta + "\n" + stop, start + "\n" + delta, false},
		{"old ID on another key", start + "\n" + delta + "\n" + stop, crossKeyStart + "\n" + crossKeyDelta, false},
	} {
		for _, progress := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/progress=%t", tc.name, progress), func(t *testing.T) {
				lines := []string{final, tc.after}
				if tc.before != "" {
					lines = append([]string{tc.before}, lines...)
				}
				fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'JSONL'\n"+strings.Join(lines, "\n")+"\nJSONL\n")
				var session Session
				if progress {
					session.OnProgress = func() {}
				}
				t.Cleanup(func() { _ = session.Close() })
				result, err := session.Run(t.Context(), t.TempDir(), "turn", false, "", config.ClaudeConfig{})
				if err != nil || result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != 0.5 ||
					result.EstimatedCostComplete != tc.complete || result.UsageComplete != tc.complete ||
					tc.after == emptyFullWithMoreUsage && result.Tokens.Output != 2 {
					t.Fatalf("late stream-only helper cost coverage: %+v, %v", result, err)
				}
			})
		}
	}
}

func TestSessionLateUnprovedDataKeepsCurrentCallSubtotal(t *testing.T) {
	const oldHelper = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"old","content":[{"type":"text","text":"A"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const malformedReplay = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"old","content":[{"type":"text","text":"A"},{"type":"tool_use","name":"Bash","input":{}}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const mixedReplay = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"old","content":[{"type":"text","text":"A","thinking":"new work"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const contentReplay = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"old","content":[{"type":"text","text":"A","content":"new work"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	const firstReplay = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":100,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":100}}}`
	const firstHelper = `{"type":"result","subtype":"success","result":"first","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const secondHelper = `{"type":"result","subtype":"success","result":"second","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	oldUsage := strings.Replace(assistantUsageEvent("old", 100, 0, 0, 100), `"parent_tool_use_id":null`, `"parent_tool_use_id":"delegate"`, 1)
	for _, tc := range []struct {
		name, first, second string
		want, baseline      usage.Tokens
	}{
		{"old nested usage", oldUsage + "\n" + firstReplay,
			`{"type":"result","subtype":"success","result":"second","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":101,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":110}}}` + "\n" +
				`{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"old","usage":{"output_tokens":100}}}}`,
			usage.Tokens{Input: 1, Output: 10}, usage.Tokens{Input: 101, Output: 110}},
		{"overflowed old nested usage", oldUsage + "\n" + firstReplay,
			`{"type":"result","subtype":"success","result":"second","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":101,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":101}}}` + "\n" +
				`{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_delta","usage":{"output_tokens":9223372036854775807}}}`,
			usage.Tokens{Input: 1, Output: 1}, usage.Tokens{Input: 101, Output: 101}},
		{"historical malformed block", oldHelper + "\n" + firstHelper, secondHelper + "\n" + malformedReplay,
			usage.Tokens{Output: 1}, usage.Tokens{Output: 2}},
		{"historical mixed block", oldHelper + "\n" + firstHelper, secondHelper + "\n" + mixedReplay,
			usage.Tokens{Output: 1}, usage.Tokens{Output: 2}},
		{"historical extra content field", oldHelper + "\n" + firstHelper, secondHelper + "\n" + contentReplay,
			usage.Tokens{Output: 1}, usage.Tokens{Output: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
if [ "$n" -eq 1 ]; then cat <<'FIRST'
`+tc.first+`
FIRST
else cat <<'SECOND'
`+tc.second+`
SECOND
fi
`)
			if err := os.WriteFile(binary+".version", []byte("2.1.277 (Claude Code)\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var session Session
			t.Cleanup(func() { _ = session.Close() })
			dir := t.TempDir()
			if _, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{}); err != nil {
				t.Fatal(err)
			}
			resumed, err := session.Run(t.Context(), dir, "second", false, "", config.ClaudeConfig{})
			if err != nil || resumed.Tokens != tc.want || resumed.UsageComplete || resumed.Answer != "second" ||
				resumed.UsageCoverage.Scope != "known_subset" || !resumed.UsageCoverage.PartialReport ||
				resumed.EstimatedCostUSD == nil || *resumed.EstimatedCostUSD != 0.25 || resumed.EstimatedCostComplete ||
				session.baseline != tc.baseline {
				t.Fatalf("late data changed the current call subtotal: result=%+v baseline=%+v error=%v", resumed, session.baseline, err)
			}
		})
	}
}

func TestSessionOldPerCallLateEmptyFrameKeepsKnownCounterFloor(t *testing.T) {
	const start = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"helper"}}}`
	const delta = `{"type":"stream_event","uuid":"chunk","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"work"}}}`
	const final = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const late = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2}}}`
	for _, tc := range []struct {
		name, frame string
		output      int64
	}{
		{"higher counters", late, 2},
		{"malformed usage", strings.Replace(late, `"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2}`, `"usage":{}`, 1), 1},
		{"bad output counter", strings.Replace(late, `"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2}`, `"usage":{"output_tokens":"bad"}`, 1), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'JSONL'\n"+start+"\n"+delta+"\n"+final+"\n"+tc.frame+"\nJSONL\n")
			if err := os.WriteFile(binary+".version", []byte("2.1.276 (Claude Code)\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var session Session
			t.Cleanup(func() { _ = session.Close() })
			result, err := session.Run(t.Context(), t.TempDir(), "turn", false, "", config.ClaudeConfig{})
			if err != nil || result.Answer != "done" || result.Tokens.Output != tc.output || result.UsageComplete ||
				result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != 0.5 || result.EstimatedCostComplete {
				t.Fatalf("old per-call result hid late usage: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestSessionOldPerCallLateHelpersUseMessageMaxima(t *testing.T) {
	const start = `{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"helper"}}}`
	const delta = `{"type":"stream_event","uuid":"chunk","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"work"}}}`
	const final = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const late = `{"type":"assistant","agent_id":"worker","parent_tool_use_id":"delegate","message":{"id":"helper","content":[],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2}}}`
	revised := strings.Replace(late, `"output_tokens":2`, `"output_tokens":3`, 1)
	otherStart := strings.Replace(strings.Replace(start, `"agent_id":"worker"`, `"agent_id":"other"`, 1), `"id":"helper"`, `"id":"other-helper"`, 1)
	otherDelta := strings.Replace(strings.Replace(delta, `"agent_id":"worker"`, `"agent_id":"other"`, 1), `"uuid":"chunk"`, `"uuid":"other-chunk"`, 1)
	otherLate := strings.Replace(strings.Replace(revised, `"agent_id":"worker"`, `"agent_id":"other"`, 1), `"id":"helper"`, `"id":"other-helper"`, 1)
	coveringAttempt := strings.Replace(strings.Replace(final, `"outputTokens":1`, `"outputTokens":2`, 1), `"total_cost_usd":0.5`, `"total_cost_usd":0.75`, 1)
	for _, tc := range []struct {
		name, before, after string
		output              int64
		cost                float64
	}{
		{"revised helper", "", late + "\n" + revised, 3, 0.5},
		{"two helpers", otherStart + "\n" + otherDelta, late + "\n" + otherLate, 5, 0.5},
		{"insufficient later result", "", late + "\n" + revised + "\n" + coveringAttempt, 3, 0.75},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := start + "\n" + delta
			if tc.before != "" {
				before += "\n" + tc.before
			}
			data := numberResultEvents(before + "\n" + final + "\n" + tc.after + "\n")
			binary := fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'JSONL'\n"+data+"JSONL\n")
			if err := os.WriteFile(binary+".version", []byte("2.1.276 (Claude Code)\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var session Session
			t.Cleanup(func() { _ = session.Close() })
			result, err := session.Run(t.Context(), t.TempDir(), "turn", false, "", config.ClaudeConfig{})
			if err != nil || result.Answer != "done" || result.Tokens.Output != tc.output || result.UsageComplete ||
				result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != tc.cost || result.EstimatedCostComplete {
				t.Fatalf("old per-call helper totals were lost or certified: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestSessionLateRootCostNeedsCoveringResult(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root"}}}`
	const delta = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}}`
	const first = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const late = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","content":[{"type":"text","text":"A"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2}}}`
	const covering = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	const noncovering = `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.75,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	for _, tc := range []struct {
		name, after string
		cost        float64
		hasResult   bool
		complete    bool
	}{
		{"no later result", "", 0.5, false, false},
		{"noncovering later result", noncovering, 0.75, true, false},
		{"covering later result", covering, 0.75, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := start + "\n" + delta + "\n" + first + "\n" + late + "\n"
			if tc.after != "" {
				data += tc.after + "\n"
			}
			binary := fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'JSONL'\n"+numberResultEvents(data)+"JSONL\n")
			if err := os.WriteFile(binary+".version", []byte("2.1.277 (Claude Code)\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var session Session
			t.Cleanup(func() { _ = session.Close() })
			result, err := session.Run(t.Context(), t.TempDir(), "turn", false, "", config.ClaudeConfig{})
			if (err == nil) != tc.hasResult || result.Tokens.Output != 2 || result.UsageComplete != tc.complete ||
				result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != tc.cost || result.EstimatedCostComplete != tc.complete ||
				(tc.hasResult && result.Answer != "done") || (!tc.hasResult && result.Answer != "") {
				t.Fatalf("covering result changed cost provenance: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestSessionAssistantOverflowCannotCertifyUsageOrCost(t *testing.T) {
	first := assistantUsageEvent("first", 0, 0, 0, math.MaxInt64)
	second := assistantUsageEvent("second", 0, 0, 0, math.MaxInt64)
	result := fmt.Sprintf(`{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":%d}}}`, int64(math.MaxInt64))
	for _, version := range []string{"2.1.276", "2.1.277"} {
		t.Run(version, func(t *testing.T) {
			binary := fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'JSONL'\n"+first+"\n"+second+"\n"+result+"\nJSONL\n")
			if err := os.WriteFile(binary+".version", []byte(version+" (Claude Code)\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var session Session
			t.Cleanup(func() { _ = session.Close() })
			got, err := session.Run(t.Context(), t.TempDir(), "turn", false, "", config.ClaudeConfig{})
			if err != nil || got.Answer != "done" || got.Tokens.Output != math.MaxInt64 || got.UsageComplete ||
				got.EstimatedCostUSD == nil || *got.EstimatedCostUSD != 0.5 || got.EstimatedCostComplete {
				t.Fatalf("assistant overflow certified result: result=%+v error=%v", got, err)
			}
		})
	}
}

func TestSessionHistoricalAssistantOverflowCannotCertifyCost(t *testing.T) {
	const final = `{"type":"result","session_id":"native-id","subtype":"success","result":"done","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":9223372036854775807}}}`
	binary := fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'JSONL'\n"+final+"\nJSONL\n")
	if err := os.WriteFile(binary+".version", []byte("2.1.277 (Claude Code)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var session Session
	session.id, session.started = "native-id", true
	session.assistantHistory = map[streamIdentity]assistantSnapshot{
		{"root", "old-1"}: {tokens: usage.Tokens{Output: math.MaxInt64}, hasTokens: true},
		{"root", "old-2"}: {tokens: usage.Tokens{Output: math.MaxInt64}, hasTokens: true},
	}
	t.Cleanup(func() { _ = session.Close() })
	got, err := session.Run(t.Context(), t.TempDir(), "turn", false, "", config.ClaudeConfig{})
	if err != nil || got.Answer != "done" || got.Tokens.Output != math.MaxInt64 || got.UsageComplete ||
		got.EstimatedCostUSD == nil || *got.EstimatedCostUSD != 0.5 || got.EstimatedCostComplete {
		t.Fatalf("historical overflow certified cost: result=%+v error=%v", got, err)
	}
}

func TestSessionOriginGapCannotCertifyPartialFinalCost(t *testing.T) {
	const ambiguous = `{"type":"assistant","parent_tool_use_id":0,"message":{"id":"unknown","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":10}}}`
	const top = `"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}`
	for _, tc := range []struct{ name, fields string }{
		{"top-level only", top},
		{"malformed model usage", top + `,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":"bad"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			final := `{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,` + tc.fields + `}`
			binary := fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'JSONL'\n"+ambiguous+"\n"+final+"\nJSONL\n")
			if err := os.WriteFile(binary+".version", []byte("2.1.277 (Claude Code)\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var session Session
			t.Cleanup(func() { _ = session.Close() })
			got, err := session.Run(t.Context(), t.TempDir(), "turn", false, "", config.ClaudeConfig{})
			if err != nil || got.Answer != "done" || got.Tokens.Output != 5 || got.UsageComplete ||
				got.EstimatedCostUSD == nil || *got.EstimatedCostUSD != 0.5 || got.EstimatedCostComplete {
				t.Fatalf("partial final certified ambiguous cost: result=%+v error=%v", got, err)
			}
		})
	}
}

func TestSessionChangedRootStreamAfterStopInvalidatesResult(t *testing.T) {
	const start = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"root"}}}`
	const delta = `{"type":"stream_event","uuid":"chunk","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}}`
	const stop = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_stop"}}`
	const final = `{"type":"result","subtype":"success","result":"answer","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	changed := strings.Replace(delta, `"text":"answer"`, `"text":"new answer"`, 1)
	untyped := strings.Replace(changed, `"type":"text_delta",`, "", 1)
	unindexed := strings.Replace(delta, `"index":0,`, "", 1)
	const malformed = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":"bad","delta":{"type":"text_delta","text":"new answer"}}}`
	for _, tc := range []struct {
		name, after string
		complete    bool
	}{
		{"exact replay", start + "\n" + delta, true},
		{"changed content", start + "\n" + changed, false},
		{"untyped content", untyped, false},
		{"unindexed replay", unindexed, false},
		{"malformed nested content", malformed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeExecutable(t, "claude", "cat >/dev/null\ncat <<'JSONL'\n"+start+"\n"+delta+"\n"+stop+"\n"+final+"\n"+tc.after+"\nJSONL\n")
			var session Session
			t.Cleanup(func() { _ = session.Close() })
			result, err := session.Run(t.Context(), t.TempDir(), "turn", false, "", config.ClaudeConfig{})
			if (err == nil) != tc.complete || result.UsageComplete != tc.complete ||
				result.EstimatedCostComplete != tc.complete || result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != 0.5 {
				t.Fatalf("late root stream: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestSessionDropsTransientStreamSignatureQuotaBetweenCalls(t *testing.T) {
	const first = `{"type":"result","subtype":"success","result":"first","modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const second = `{"type":"result","subtype":"success","result":"second","modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
printf '{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_start","message":{"id":"helper-%s"}}}\n' "$n"
printf '{"type":"stream_event","uuid":"chunk-%s","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"work"}}}\n' "$n"
printf '{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool-%s","name":"Task","input":{}}}}\n' "$n"
printf '%s\n' '{"type":"stream_event","agent_id":"worker","parent_tool_use_id":"delegate","event":{"type":"message_stop"}}'
if [ "$n" -eq 1 ]; then printf '%s\n' '`+first+`'; else printf '%s\n' '`+second+`'; fi
`)
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	dir := t.TempDir()
	for call := 1; call <= 2; call++ {
		if _, err := session.Run(t.Context(), dir, "turn", false, "", config.ClaudeConfig{}); err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		messageID := fmt.Sprintf("helper-%d", call)
		id := streamIdentity{taggedStreamKey("worker", "delegate"), messageID}
		origin := (streamIdentity{"delegate", messageID}).progressKey("assistant_tagged")
		wantBytes := int64(call * (len(origin) + len(id.progressKey("stream_start")) + len(id.progressKey("assistant"))))
		if session.progressEntries != 3*call || session.progressBytes != wantBytes {
			t.Fatalf("call %d persisted transient signatures: entries=%d bytes=%d, want bytes=%d", call, session.progressEntries, session.progressBytes, wantBytes)
		}
	}
}

func TestSessionProvisionalCostWithMissingFinalIsIncomplete(t *testing.T) {
	const provisional = `{"type":"result","subtype":"success","result_index":0,"result":"early","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}`
	const final = `{"type":"result","subtype":"success","result_index":1,"result":"final","modelUsage":{"main":{"inputTokens":2,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
	fakeExecutable(t, "claude", "cat >/dev/null\nprintf '%s\\n' '"+provisional+"' '"+final+"'\n")
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.Run(t.Context(), t.TempDir(), "turn", false, "", config.ClaudeConfig{})
	if err != nil || !result.UsageComplete || result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != 0.5 || result.EstimatedCostComplete {
		t.Fatalf("partial cost was certified complete with complete tokens: %+v, %v", result, err)
	}
}

func TestSessionCrashZeroCostIsUnknown(t *testing.T) {
	fakeExecutable(t, "claude", `cat >/dev/null
printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"total_cost_usd":0,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}'
`)
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if err == nil || result.EstimatedCostUSD != nil {
		t.Fatalf("zeroed crash result claimed known cost: %+v, %v", result, err)
	}
}

func TestSessionZeroedCrashKeepsEarlierKnownCost(t *testing.T) {
	fakeExecutable(t, "claude", `cat >/dev/null
printf '%s\n' '{"type":"result","subtype":"success","total_cost_usd":0.5,"modelUsage":{"main":{"inputTokens":1,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":1}}}'
printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"total_cost_usd":0,"modelUsage":{"main":{"inputTokens":0,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}'
`)
	var session Session
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.Run(t.Context(), t.TempDir(), "task", false, "", config.ClaudeConfig{})
	if err == nil || result.EstimatedCostUSD == nil || *result.EstimatedCostUSD != 0.5 || result.UsageComplete {
		t.Fatalf("zeroed crash erased known cost or claimed completeness: %+v, %v", result, err)
	}
}

func TestSessionUncertainFailedUsageInvalidatesResumeBaseline(t *testing.T) {
	first := strings.Replace(successfulResult, `"modelUsage":`, `"total_cost_usd":0.5,"modelUsage":`, 1)
	const second = `{"type":"result","subtype":"success","result":"continued","total_cost_usd":1.25,"modelUsage":{"main":{"inputTokens":6,"cacheCreationInputTokens":10,"cacheReadInputTokens":14,"outputTokens":22}}}`
	for _, tc := range []struct{ name, diagnostic string }{
		{name: "oversized stderr", diagnostic: strings.Repeat("x", bounded.DiagnosticBytes+1)},
		{name: "matched wait ceiling", diagnostic: waitCeilingDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
if [ "$n" -eq 1 ]; then printf '%s\n' '`+first+`'; printf '%s\n' '`+tc.diagnostic+`' >&2
else printf '%s\n' '`+second+`'; fi
`)
			var session Session
			t.Cleanup(func() { _ = session.Close() })
			dir := t.TempDir()
			firstResult, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{})
			if err == nil || firstResult.EstimatedCostUSD == nil || *firstResult.EstimatedCostUSD != 0.5 {
				t.Fatalf("uncertain first call: %+v, %v", firstResult, err)
			}
			resumed, err := session.Run(t.Context(), dir, "second", false, "", config.ClaudeConfig{})
			if err != nil || resumed.EstimatedCostUSD != nil || resumed.UsageComplete ||
				!resumed.UsageCoverage.CumulativeUnattributed {
				t.Fatalf("uncertain prior cost or tokens were attributed to resumed call: %+v, %v", resumed, err)
			}
		})
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
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || first.Answer != "" || first.UsageComplete || first.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}) || first.UsageCoverage != (usage.Coverage{Scope: "known_subset", PartialReport: true}) {
		t.Fatalf("failed turn: %+v,%v", first, err)
	}
	id := session.id
	second, err := session.Run(t.Context(), dir, "continue", false, "", config.ClaudeConfig{})
	if err != nil || second.Tokens != first.Tokens || second.UsageComplete || !second.UsageCoverage.CumulativeUnattributed {
		t.Fatalf("continued failed turn: %+v,%v", second, err)
	}
	third, err := session.Run(t.Context(), dir, "continue again", false, "", config.ClaudeConfig{})
	if err == nil || !strings.Contains(err.Error(), "No conversation found") || third.Answer != "" || session.id != id || !strings.Contains(readFile(t, binary+".3.args"), "--resume\n"+id) || readFile(t, binary+".count") != "3" {
		t.Fatalf("missing resume silently replaced: %+v,%v", third, err)
	}
}

func TestSessionResumePreservesUnresolvedUnidentifiedTasks(t *testing.T) {
	for _, tc := range []struct{ name, start, finish string }{
		{"ID only", `{"type":"system","subtype":"task_started","task_id":"helper"}`,
			`{"type":"system","subtype":"task_notification","uuid":"done","task_id":"helper","status":"completed"}`},
		{"anonymous", `{"type":"system","subtype":"background_tasks_changed","uuid":"listed","tasks":[{}]}`,
			`{"type":"system","subtype":"background_tasks_changed","uuid":"cleared","tasks":[]}`},
	} {
		for _, callback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/callback=%t", tc.name, callback), func(t *testing.T) {
				fakeExecutable(t, "claude", `n=0; [ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+tc.start+`' '`+successfulResult+`' ;;
2) printf '%s\n' '`+successfulResult+`' ;;
3) printf '%s\n' '`+tc.finish+`' '`+successfulResult+`' ;;
esac
`)
				var session Session
				if callback {
					session.OnProgress = func() {}
				}
				t.Cleanup(func() {
					if err := session.Close(); err != nil {
						t.Error(err)
					}
				})
				workspace := t.TempDir()
				if _, err := session.Run(t.Context(), workspace, "first", false, "", config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "background tasks still running") {
					t.Fatalf("first call did not retain pending task: %v", err)
				}
				if _, err := session.Run(t.Context(), workspace, "resume", false, "", config.ClaudeConfig{}); err == nil || !strings.Contains(err.Error(), "background tasks still running") {
					t.Fatalf("resumed call accepted unclosed task: %v", err)
				}
				if result, err := session.Run(t.Context(), workspace, "close", false, "", config.ClaudeConfig{}); err != nil || result.Answer != "final answer" {
					t.Fatalf("closing notice did not release task: result=%+v error=%v", result, err)
				}
			})
		}
	}
}

func TestNewConversationClearsUnidentifiedTasks(t *testing.T) {
	session := Session{runlessPending: map[string]int{"helper": 1}, runlessExplicit: map[string]bool{"helper": true},
		runlessAmbiguous: map[string]bool{"helper": true}, runlessCount: 1, anonymousPending: 1}
	session.NewConversation()
	if session.runlessPending != nil || session.runlessExplicit != nil || session.runlessAmbiguous != nil ||
		session.runlessCount != 0 || session.anonymousPending != 0 {
		t.Fatal("new conversation retained pending tasks")
	}
}

func TestUnidentifiedTaskIDsRemainBoundedAcrossResume(t *testing.T) {
	for _, tc := range []struct{ name, event string }{
		{"start", `{"type":"system","subtype":"task_started","task_id":%q}`},
		{"usage", `{"type":"system","subtype":"task_progress","task_id":%q,"usage":{"total_tokens":1}}`},
		{"list", `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":%q}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := jsonlOutput{progressEntryLimit: 2}
			writeChunks(t, &first, fmt.Sprintf(tc.event, "a")+"\n"+fmt.Sprintf(tc.event, "b")+"\n")
			if first.err != nil || first.progressEntries != 2 {
				t.Fatalf("first two identities: entries=%d error=%v", first.progressEntries, first.err)
			}
			resumed := jsonlOutput{progressEntryLimit: 2, progressSeen: first.progressSeen,
				progressEntries: first.progressEntries, progressBytes: first.progressBytes,
				runlessPending: first.runlessPending, runlessExplicit: first.runlessExplicit,
				runlessAmbiguous: first.runlessAmbiguous, runlessCount: first.runlessCount}
			writeChunks(t, &resumed, fmt.Sprintf(tc.event, "c")+"\n")
			if !errors.Is(resumed.err, aicli.ErrOutputLimit) || resumed.runlessPending["c"] != 0 {
				t.Fatalf("third identity escaped bound: pending=%v entries=%d error=%v",
					resumed.runlessPending, resumed.progressEntries, resumed.err)
			}
		})
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
	known := usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}
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
		{"helper total", withHelper, known, usage.Tokens{Input: 97, CacheCreationInput: 31, CachedInput: 37, Output: 41}, true, false},
		{"repeated result snapshots", numberResultEvents(doubled + "\n" + doubled), known, known, true, false},
		{"root fallback is current turn", `{"type":"result","subtype":"success","result":"answer","usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}}`, known, known, false, false},
		{"missing usage", `{"type":"result","subtype":"success","result":"answer"}`, known, usage.Tokens{}, false, false},
		{"decreasing aggregate without current root usage", aggregateOnly, usage.Tokens{Input: 30, CacheCreationInput: 10, CachedInput: 14, Output: 22}, usage.Tokens{}, false, false},
		{"decreasing aggregate retains current assistant", assistantUsageEvent("current", 3, 5, 7, 11) + "\n" + successfulResult, usage.Tokens{Input: 30, CacheCreationInput: 10, CachedInput: 14, Output: 22}, known, false, false},
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
	var root string
	t.Cleanup(func() {
		if root != "" {
			session.runtimeRoot = root
		}
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := session.Run(t.Context(), dir, "first", false, "", config.ClaudeConfig{}); err != nil {
		t.Fatal(err)
	}
	root = session.runtimeRoot
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
	session.runtimeRoot = "invalid\x00path"
	if err := session.Close(); err == nil || session.runtimeRoot == "" {
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
	const currentStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"current"}}}`
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
			const cumulative = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":88,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":100}}}`
			const later = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":92,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":106}}}`
			const recovered = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":96,"cacheCreationInputTokens":5,"cacheReadInputTokens":7,"outputTokens":112}}}`
			fakeExecutable(t, "claude", `n=0
[ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+test.first+`' ;;
2) printf '%s\n' '`+currentStart+`' '`+assistantUsageEvent("current", 4, 0, 0, 6)+`' '`+cumulative+`' ;;
3) printf '%s\n' '`+later+`' ;;
4) printf '%s\n' '`+recovered+`' ;;
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
			if err != nil || second.UsageComplete || second.Tokens != (usage.Tokens{}) {
				t.Fatalf("prior usage leaked into next call: %+v,%v", second, err)
			}
			third, err := session.Run(t.Context(), source, "third", false, "", config.ClaudeConfig{})
			if err != nil || third.UsageComplete || third.Tokens != (usage.Tokens{}) || !third.UsageCoverage.CumulativeUnattributed {
				t.Fatalf("uncertain baseline was attributed to third call: %+v,%v", third, err)
			}
			fourth, err := session.Run(t.Context(), source, "fourth", false, "", config.ClaudeConfig{})
			if err != nil || !fourth.UsageComplete || fourth.Tokens != (usage.Tokens{Input: 4, Output: 6}) {
				t.Fatalf("fresh cumulative baseline not recovered: %+v,%v", fourth, err)
			}
		})
	}
}

func TestSessionHistoricalAssistantWithoutCountersDoesNotRebill(t *testing.T) {
	const old = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old"}}`
	const revisedOld = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","content":[{"type":"text","text":"continued"}]}}`
	const oldUsage = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"old","usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`
	const first = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":100,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`
	const second = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":120,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`
	const third = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":140,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`
	const fourth = `{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":160,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":0}}}`
	fakeExecutable(t, "claude", `n=0
[ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+old+`' '`+first+`' ;;
2) printf '%s\n' '`+revisedOld+`' '`+oldUsage+`' '`+second+`' ;;
3) printf '%s\n' '`+third+`' ;;
4) printf '%s\n' '`+fourth+`' ;;
esac
`)
	var session Session
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	source := t.TempDir()
	for call, want := range []usage.Tokens{{Input: 100}, {Input: 20}, {}, {Input: 20}} {
		result, err := session.Run(t.Context(), source, "task", false, "", config.ClaudeConfig{})
		complete := call == 0 || call == 3
		if err != nil || result.Tokens != want || result.UsageComplete != complete || session.baseline.Input != 100+20*int64(call) {
			t.Fatalf("call %d rebilled historical counters: result=%+v baseline=%+v error=%v", call+1, result, session.baseline, err)
		}
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
	const secondStart = `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"message_start","message":{"id":"second"}}}`
	native := func(input, cached, output int64) string {
		return fmt.Sprintf(`{"type":"result","subtype":"success","result":"done","modelUsage":{"main":{"inputTokens":%d,"cacheCreationInputTokens":5,"cacheReadInputTokens":%d,"outputTokens":%d}}}`, input-cached-5, cached, output)
	}
	for _, test := range []struct {
		name          string
		input, output int64
		second        usage.Tokens
	}{
		{"regressed below baseline", 14, 10, usage.Tokens{}},
		{"omitted current messages", 16, 12, usage.Tokens{Input: 1, Output: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeExecutable(t, "claude", `n=0
[ ! -f "$0.count" ] || n=$(cat "$0.count")
n=$((n+1)); printf '%s' "$n" > "$0.count"
cat >/dev/null
case "$n" in
1) printf '%s\n' '`+successfulResult+`' ;;
2) printf '%s\n' '`+secondStart+`' '`+assistantUsageEvent("second", 4, 0, 0, 6)+`' '`+native(test.input, 7, test.output)+`' ;;
3) printf '%s\n' '`+native(25, 7, 25)+`' ;;
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
			for call, want := range []usage.Tokens{{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}, test.second, {}, {Input: 2, Output: 3}} {
				result, err := session.Run(t.Context(), source, "task", false, "", config.ClaudeConfig{})
				complete := call == 0 || call == 3
				if err != nil || result.Tokens != want || result.UsageComplete != complete {
					t.Fatalf("call%d = %+v,%v want %+v complete=%v", call+1, result, err, want, complete)
				}
				if call == 2 && !result.UsageCoverage.CumulativeUnattributed {
					t.Fatalf("uncertain cumulative baseline was attributed: %+v", result)
				}
			}
		})
	}
}

func TestLateCoveredBlockRevisionCannotProveExactCumulativeBaseline(t *testing.T) {
	output := jsonlOutput{}
	writeChunks(t, &output, assistantUsageEvent("covered", 3, 5, 7, 2)+"\n"+successfulResult+"\n"+assistantUsageEvent("covered", 3, 5, 7, 11)+"\n")
	output.finish()
	if output.err != nil || output.result.UsageComplete || output.hasCumulative || !output.provenanceGap ||
		output.result.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}) {
		t.Fatalf("late revision certified exact baseline or lost known subtotal: %+v,%v", output, output.err)
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
	if err != nil || !fresh.UsageComplete || fresh.Tokens != (usage.Tokens{Input: 15, CacheCreationInput: 5, CachedInput: 7, Output: 11}) {
		t.Fatalf("old usage gap survived independent conversation: %+v, %v", fresh, err)
	}
}

func TestSessionInitializationFailureStaysBound(t *testing.T) {
	binary := fakeExecutable(t, "claude", "touch \"$0.called\"\nexit 99\n")
	testsupport.UseShortClaudeTempDir(t, "/tmp")
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
