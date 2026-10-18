package action

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunnerNativeDispatchContinuesRolesAndRefreshesFullCritic(t *testing.T) {
	for _, action := range []command.Action{command.Comment, command.Fix} {
		for _, mainProvider := range []string{"codex", "claude"} {
			t.Run(string(action)+"/"+mainProvider+"-main", func(t *testing.T) {
				installActionTestSkills(t)
				t.Setenv("CODEX_HOME", t.TempDir())
				for _, key := range []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
					t.Setenv(key, "")
				}
				receipts := t.TempDir()
				root := t.TempDir()
				job := withClaudeCritic(portableJob(action))
				if mainProvider == "claude" {
					job.Main.Provider, job.Critic.Provider = "claude", "codex"
				}
				job.CriticMaxRounds, job.CriticNonblockingRounds = 3, 0
				for _, provider := range []string{"codex", "claude"} {
					first, later := `{"approved":false,"has_blockers":true,"feedback":"Clarify the response."}`, approvedCritiqueJSON
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
				result, err := (&Runner{WorkRoot: root}).Run(context.Background(), job)
				if err != nil || result.Answer != "Second proposal." || len(result.Calls) != 8 || len(result.Refutations) != 2 || result.Validate(job) != nil {
					t.Fatalf("native role sequence: result=%+v error=%v", result, err)
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
					calls, schema := 3, mainOutputSchema
					if provider == job.Critic.Provider {
						calls, schema = 5, criticOutputSchema
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
						wantHistory := provider == job.Critic.Provider && call%2 == 0 || provider == job.Main.Provider && call >= 3
						if strings.Contains(read(provider, call, "args"), ".minion-review-history-") != wantHistory || strings.Contains(read(provider, call, "prompt"), "Application-owned refutations") != wantHistory {
							t.Fatalf("%s call%d received the wrong refutation grant/context", provider, call)
						}
					}
					if _, err := os.Lstat(read(provider, 1, "cwd")); !os.IsNotExist(err) {
						t.Fatalf("%s role scratch survived action cleanup: %v", provider, err)
					}
				}
				for _, call := range result.Calls {
					if !call.Complete || call.Tokens != (usage.Tokens{Input: 3, Output: 2}) {
						t.Fatalf("lost per-invocation accounting at the native dispatch seam: %+v", call)
					}
				}
				if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
					t.Fatalf("action scratch retained: %v, %v", entries, err)
				}
			})
		}
	}
}
