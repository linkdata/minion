package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunnerNativeClaudePreservesContextAndCriticSchema(t *testing.T) {
	for _, claudeRole := range []string{roleMain, roleCritic} {
		t.Run(claudeRole, func(t *testing.T) {
			installActionTestSkills(t, testReviewSkillURL)
			writeActionTestSkills(t, os.Getenv("HOME"), ".claude", testReviewSkillURL)
			root := t.TempDir()
			script := fmt.Sprintf(`#!/bin/sh
set -e
case "$*" in
 *has_blockers*) output='{"approved":true,"has_blockers":false,"feedback":""}' ;;
 *) output='{"answer":"No findings.","refutations":""}' ;;
esac
printf '%%s\n' "$@" > %s
cat > %s
for arg in "$@"; do
 case "$arg" in /*/.minion-context-*) cat "$arg/source.json" > %s ;; esac
done
printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","structured_output":'"$output"',"usage":{"input_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2},"modelUsage":{"portable-claude":{"inputTokens":3,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}'
`, shellQuote(filepath.Join(root, "args")), shellQuote(filepath.Join(root, "prompt")), shellQuote(filepath.Join(root, "source")))
			fakeOnPath(t, "claude", script)
			codexAnswer := proposalJSON("No findings.", "")
			if claudeRole == roleMain {
				codexAnswer = approvedCritiqueJSON
			}
			event, err := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": codexAnswer}})
			if err != nil {
				t.Fatal(err)
			}
			fakeOnPath(t, "codex", "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' "+shellQuote(string(event))+" '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":3,\"cached_input_tokens\":0,\"output_tokens\":2}}'\n")
			job := withClaudeCritic(portableJob(command.Comment))
			if claudeRole == roleMain {
				job.Main.Provider, job.Critic.Provider = "claude", "codex"
			}
			job.Main.Model, job.Main.Effort, job.Main.Skills = "portable-"+job.Main.Provider, "high", []string{testReviewSkillURL}
			job.Critic.Model, job.Critic.Effort, job.Critic.AlwaysOnSkills = "portable-"+job.Critic.Provider, "medium", []string{testReviewSkillURL}
			runner := Runner{WorkRoot: t.TempDir(), Claude: config.ClaudeConfig{Model: "stale-host-model"}}
			result, err := runner.Run(context.Background(), job)
			if err != nil || result.Answer != "No findings." || len(result.Calls) != 2 {
				t.Fatalf("native action: result=%+v error=%v", result, err)
			}
			argsData, err := os.ReadFile(filepath.Join(root, "args"))
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(argsData), "\n"), "\n")
			values := map[string]string{}
			contextDir := ""
			for n, arg := range args[:len(args)-1] {
				values[arg] = args[n+1]
				if arg == "--add-dir" && strings.HasPrefix(filepath.Base(args[n+1]), ".minion-context-") {
					contextDir = args[n+1]
				}
			}
			schema, role := mainOutputSchema, job.Main
			if claudeRole == roleCritic {
				schema, role = criticOutputSchema, *job.Critic
			}
			if contextDir == "" || values["--json-schema"] != schema || values["--model"] != role.Model || values["--effort"] != role.Effort {
				t.Fatalf("Claude %s lost native role/context/schema wiring: %s", claudeRole, argsData)
			}
			var policy struct {
				Sandbox struct {
					Filesystem struct {
						AllowRead []string `json:"allowRead"`
						DenyWrite []string `json:"denyWrite"`
					} `json:"filesystem"`
				} `json:"sandbox"`
			}
			if err := json.Unmarshal([]byte(values["--settings"]), &policy); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(policy.Sandbox.Filesystem.AllowRead, contextDir) || !slices.Contains(policy.Sandbox.Filesystem.DenyWrite, contextDir) {
				t.Fatalf("Claude context grant is not read-only: %+v", policy)
			}
			source, err := os.ReadFile(filepath.Join(root, "source"))
			if err != nil || !strings.Contains(string(source), job.Source.Head) {
				t.Fatalf("Claude cannot read supplied source identity: %s, %v", source, err)
			}
			prompt, err := os.ReadFile(filepath.Join(root, "prompt"))
			skill := filepath.Join(os.Getenv("HOME"), ".claude", "skills", "ponytail-review", "SKILL.md")
			if err != nil || !strings.Contains(string(prompt), skill) || !strings.Contains(string(prompt), contextDir) {
				t.Fatalf("Claude lost skill/context prompt: %s, %v", prompt, err)
			}
			// The fake Codex emits counters without a persisted session tree, so its accounting remains incomplete.
			for i, role := range []RoleSettings{job.Main, *job.Critic} {
				wantRole := []string{roleMain, roleCritic}[i]
				if got := result.Calls[i]; got.Provider != role.Provider || got.Role != wantRole || got.Model != role.Model || got.Effort != role.Effort || got.Complete != (role.Provider == "claude") || got.Tokens != (usage.Tokens{Input: 3, Output: 2}) {
					t.Fatalf("%s usage/attribution=%+v", wantRole, got)
				}
			}
			if !result.Calls[1].Approved || !result.Calls[1].NoBlockers || result.Calls[1].HasBlockers {
				t.Fatalf("native structured critic verdict lost: %+v", result.Calls[1])
			}
		})
	}
}

func TestRunnerMissingProviderIsRetryableAndRecordsZeroUsage(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			job := portableJob(command.Comment)
			job.Main.Provider = provider
			result, err := (&Runner{WorkRoot: t.TempDir()}).Run(context.Background(), job)
			if !errors.Is(err, exec.ErrNotFound) || errors.Is(err, monitor.ErrTerminalJob) || result.Answer != "" || len(result.Calls) != 1 {
				t.Fatalf("missing %s classification/accounting: result=%+v error=%v", provider, result, err)
			}
			if got := result.Calls[0]; got.Provider != provider || got.Role != roleMain || got.Round != 1 || got.Tokens != (usage.Tokens{}) || !got.Complete {
				t.Fatalf("missing %s call=%+v", provider, got)
			}
		})
	}
}
