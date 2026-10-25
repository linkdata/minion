package action

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunnerNativeDispatchContinuesRolesAndRefreshesFullCritic(t *testing.T) {
	for _, action := range []command.Action{command.Comment, command.Fix} {
		for _, mainProvider := range []string{"codex", "claude"} {
			t.Run(string(action)+"/"+mainProvider+"-main", func(t *testing.T) {
				installActionTestSkills(t, testReviewSkillURL)
				writeActionTestSkills(t, os.Getenv("HOME"), ".claude", testReviewSkillURL)
				t.Setenv("CODEX_HOME", t.TempDir())
				for _, key := range []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
					t.Setenv(key, "")
				}
				receipts := t.TempDir()
				root := t.TempDir()
				root, err := filepath.EvalSymlinks(root)
				if err != nil {
					t.Fatal(err)
				}
				job := withClaudeCritic(testJob(action))
				if mainProvider == "claude" {
					job.Main.Provider, job.Critic.Provider = "claude", "codex"
				}
				job.CriticMaxRounds, job.CriticNonblockingRounds = 3, 0
				job.Main.Model, job.Main.Effort, job.Main.Skills = "portable-"+job.Main.Provider, "high", []string{testReviewSkillURL}
				job.Critic.Model, job.Critic.Effort, job.Critic.AlwaysOnSkills = "portable-"+job.Critic.Provider, "medium", []string{testReviewSkillURL}
				for _, provider := range []string{"codex", "claude"} {
					first, later := `{"approved":false,"has_blockers":true,"review_complete":true,"feedback":"Clarify the response."}`, approvedCritiqueJSON
					if provider == mainProvider {
						first, later = proposalJSON("First proposal.", ""), proposalJSON("Second proposal.", "Counterevidence for the earlier finding.")
					}
					message := func(answer string) string {
						data, err := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": answer}})
						if err != nil {
							t.Fatal(err)
						}
						return string(data)
					}
					script := fmt.Sprintf(`#!/bin/sh
set -eu
base=%s/%s
root=%s
provider=%s
mainProvider=%s
action=%s
n=1
[ ! -f "$base.count" ] || n=$(( $(cat "$base.count") + 1 ))
printf '%%s' "$n" > "$base.count"
printf '%%s\n' "$@" > "$base.$n.args"
printf '%%s\n' "$PWD" > "$base.$n.cwd"
if [ -f "$PWD/prior-role-note" ]; then state=retained; else state=clean; fi
printf '%%s' "$state" > "$base.$n.scratch"
printf 'prior role history' > "$PWD/prior-role-note"
cat > "$base.$n.prompt"
if [ "$action" = fix ] && [ "$provider" = "$mainProvider" ]; then
 for source in "$root"/minion-*; do
  [ -d "$source/.git" ] || continue
  printf 'candidate %%s' "$n" > "$source/source.txt"
 done
fi
mode=fresh
id="11111111-1111-4111-8111-00000000000$n"
previous=""
positional=false
for arg do
 case "$previous" in
  --session-id) id="$arg" ;;
  --resume) mode=resume; id="$arg" ;;
  --json-schema) printf '%%s' "$arg" > "$base.$n.schema" ;;
  --output-schema) cat "$arg" > "$base.$n.schema" ;;
 esac
 if $positional && [ "$arg" != - ]; then id="$arg"; fi
 if [ "$arg" = -- ]; then positional=true; fi
 previous="$arg"
done
if [ "$1" = exec ] && [ "$2" = resume ]; then mode=resume; fi
printf '%%s\n%%s\n' "$mode" "$id" > "$base.$n.identity"
if [ "$n" -eq 1 ] || { [ "$provider" != "$mainProvider" ] && [ "$n" -eq 3 ]; }; then output=%s; event=%s; else output=%s; event=%s; fi
if [ %s = codex ]; then
 dir="$CODEX_HOME/sessions/fixture"
 mkdir -p "$dir"
 rollout="$dir/rollout-$id.jsonl"
 [ -f "$rollout" ] || printf '{"type":"session_meta","payload":{"id":"%%s","session_id":"%%s","source":"exec"}}\n' "$id" "$id" > "$rollout"
 printf '{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-%%s"}}\n' "$n" >> "$rollout"
 printf '{"type":"token_usage_record","payload":{"thread_id":"%%s","session_id":"%%s","turn_id":"turn-%%s","response_id":"response-%%s","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":2}}}\n' "$id" "$id" "$n" "$n" >> "$rollout"
 printf '{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-%%s"}}\n' "$n" >> "$rollout"
 printf '{"type":"thread.started","thread_id":"%%s"}\n' "$id"
 printf '%%s\n' "$event" '{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":2}}'
else
 turns=1
 [ ! -f "$base.$id.turns" ] || turns=$(( $(cat "$base.$id.turns") + 1 ))
 printf '%%s' "$turns" > "$base.$id.turns"
 printf '{"type":"system","subtype":"init","session_id":"%%s"}\n' "$id"
 printf '{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","session_id":"%%s","structured_output":%%s,"modelUsage":{"fixture":{"inputTokens":%%s,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":%%s}}}\n' "$id" "$output" "$((turns*3))" "$((turns*2))"
fi
`, shellQuote(receipts), provider, shellQuote(root), shellQuote(provider), shellQuote(mainProvider), shellQuote(string(action)), shellQuote(first), shellQuote(message(first)), shellQuote(later), shellQuote(message(later)), provider)
					fakeOnPath(t, provider, script)
				}
				runner := Runner{WorkRoot: root,
					Codex:  config.CodexConfig{Model: "stale-host-codex", ReasoningEffort: "low"},
					Claude: config.ClaudeConfig{Model: "stale-host-claude", Effort: "low"},
				}
				result, err := runner.Run(t.Context(), job)
				if err != nil || result.Answer != "Second proposal." || len(result.Calls) != 8 || len(result.Refutations) != 2 || result.Validate(job) != nil {
					t.Fatalf("native role sequence: result=%+v error=%v", result, err)
				}
				if runner.Codex.Model != "stale-host-codex" || runner.Codex.ReasoningEffort != "low" || runner.Claude.Model != "stale-host-claude" || runner.Claude.Effort != "low" {
					t.Fatal("native dispatch changed shared host model/effort settings")
				}
				for i, refutation := range result.Refutations {
					if refutation != (Refutation{Round: i + 2, Text: "Counterevidence for the earlier finding."}) {
						t.Fatalf("native refutation lost: %+v", result.Refutations)
					}
				}
				if action == command.Fix && (len(result.Changes) != 1 || string(result.Changes[0].Data) != "candidate 3") {
					t.Fatalf("native fix export lost: %+v", result.Changes)
				}
				read := func(provider string, call int, kind string) string {
					t.Helper()
					data, err := os.ReadFile(filepath.Join(receipts, fmt.Sprintf("%s.%d.%s", provider, call, kind)))
					if err != nil {
						t.Fatal(err)
					}
					return strings.TrimSuffix(string(data), "\n")
				}
				for _, provider := range []string{job.Main.Provider, job.Critic.Provider} {
					calls, schema, role := 3, mainOutputSchema, job.Main
					if provider == job.Critic.Provider {
						calls, schema, role = 5, criticOutputSchema, *job.Critic
					}
					firstID := strings.Split(read(provider, 1, "identity"), "\n")
					if len(firstID) != 2 || firstID[0] != "fresh" || firstID[1] == "" {
						t.Fatalf("%s initial identity=%q", provider, firstID)
					}
					for call := 1; call <= calls; call++ {
						identity := strings.Split(read(provider, call, "identity"), "\n")
						state := "clean"
						resume := provider == job.Main.Provider && call > 1 || provider == job.Critic.Provider && call%2 == 0
						if resume {
							state = "retained"
							priorID := firstID[1]
							if provider == job.Critic.Provider {
								priorID = strings.Split(read(provider, call-1, "identity"), "\n")[1]
							}
							if len(identity) != 2 || identity[0] != "resume" || identity[1] != priorID {
								t.Fatalf("%s did not resume latest role conversation at call%d: %q", provider, call, identity)
							}
						} else if call > 1 {
							priorID := strings.Split(read(provider, call-1, "identity"), "\n")[1]
							if len(identity) != 2 || identity[0] != "fresh" || identity[1] == priorID {
								t.Fatalf("full critic reused previous conversation: %q", identity)
							}
						}
						if read(provider, call, "cwd") != read(provider, 1, "cwd") || read(provider, call, "scratch") != state || read(provider, call, "schema") != schema {
							t.Fatalf("%s call%d lost stable paths, scratch policy or schema", provider, call)
						}
						args := read(provider, call, "args")
						if strings.Contains(args, "stale-host-") || !strings.Contains(args, "--model\n"+role.Model+"\n") {
							t.Fatalf("%s call%d lost native role model: %s", provider, call, args)
						}
						prompt := read(provider, call, "prompt")
						if !strings.Contains(prompt, ".minion-context-") || !strings.Contains(prompt, job.Source.Head) || strings.Contains(args, ".minion-context-") {
							t.Fatalf("%s call%d lost prompt context or passed it as a writable root: args=%s prompt=%s", provider, call, args, prompt)
						}
						if provider == "codex" {
							if !strings.Contains(args, fmt.Sprintf("model_reasoning_effort=%q\n", role.Effort)) {
								t.Fatalf("Codex call%d lost native role effort: %s", call, args)
							}
							mode := `sandbox_mode="read-only"`
							if action == command.Fix && provider == job.Main.Provider {
								mode = `sandbox_mode="workspace-write"`
							}
							if !strings.Contains(args, mode) || strings.Contains(args, "permissions.minion") {
								t.Fatalf("Codex call%d lost native sandbox mode: %s", call, args)
							}
						} else {
							arguments, values := strings.Split(args, "\n"), map[string]string{}
							for n, arg := range arguments[:len(arguments)-1] {
								values[arg] = arguments[n+1]
							}
							if values["--json-schema"] != schema || values["--effort"] != role.Effort || values["--permission-mode"] != "dontAsk" || !strings.Contains(args, "--safe-mode\n") {
								t.Fatalf("Claude call%d lost native sandbox/schema/effort: %s", call, args)
							}
							var policy struct {
								Sandbox struct {
									Enabled           bool `json:"enabled"`
									FailIfUnavailable bool `json:"failIfUnavailable"`
								} `json:"sandbox"`
							}
							if err := json.Unmarshal([]byte(values["--settings"]), &policy); err != nil {
								t.Fatal(err)
							}
							grant := values["--add-dir"]
							writableMain := action == command.Fix && provider == job.Main.Provider
							if !policy.Sandbox.Enabled || !policy.Sandbox.FailIfUnavailable || (grant != "") != writableMain || grant != "" && (filepath.Dir(grant) != root || !strings.Contains(prompt, fmt.Sprintf("%q", grant))) {
								t.Fatalf("Claude call%d lost native sandbox mode: args=%s policy=%+v", call, args, policy)
							}
							skill := filepath.Join(os.Getenv("HOME"), ".claude", "skills", "ponytail-review", "SKILL.md")
							if !strings.Contains(prompt, skill) {
								t.Fatalf("Claude call%d lost skill prompt: %s", call, prompt)
							}
						}
						wantHistory := provider == job.Critic.Provider && call%2 == 0 || provider == job.Main.Provider && call >= 3
						if strings.Contains(args, ".minion-review-history-") || strings.Contains(prompt, ".minion-review-history-") != wantHistory || strings.Contains(prompt, "Application-owned refutations") != wantHistory {
							t.Fatalf("%s call%d received the wrong refutation context", provider, call)
						}
					}
					if _, err := os.Lstat(read(provider, 1, "cwd")); !os.IsNotExist(err) {
						t.Fatalf("%s role scratch survived action cleanup: %v", provider, err)
					}
				}
				for _, call := range result.Calls {
					role := job.Main
					if call.Role == roleCritic {
						role = *job.Critic
					} else if call.Role != roleMain {
						t.Fatalf("unexpected native call role: %+v", call)
					}
					if call.Provider != role.Provider || call.Model != role.Model || call.Effort != role.Effort || !call.Complete || call.ReviewComplete != (call.Role == roleCritic) || call.Tokens != (usage.Tokens{Input: 3, Output: 2}) {
						t.Fatalf("lost per-invocation accounting/attribution at the native dispatch seam: %+v", call)
					}
				}
				if last := result.Calls[len(result.Calls)-1]; !last.Approved || !last.NoBlockers || last.HasBlockers || !last.ReviewComplete {
					t.Fatalf("native structured critic verdict lost: %+v", last)
				}
				if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
					t.Fatalf("action scratch retained: %v, %v", entries, err)
				}
			})
		}
	}
}
