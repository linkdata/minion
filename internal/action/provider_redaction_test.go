package action

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/linkdata/minion/internal/config"
)

func TestNativeProposalRedactsEncodedConfiguredKey(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, tc := range []struct{ name, key string }{
			{"quote", `fixture"secret`},
			{"backslash", `fixture\secret`},
			{"JSON HTML escape", "fixture<secret"},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				t.Setenv("CODEX_HOME", t.TempDir())
				t.Setenv("CLAUDE_CONFIG_DIR", "")
				for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
					t.Setenv(key, "")
				}
				proposal := proposalJSON("public "+tc.key, "private "+tc.key)
				var event map[string]any
				script := "#!/bin/sh\ncat >/dev/null\n"
				if provider == "codex" {
					event = map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": proposal}}
					script += "printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"root\"}'\n"
				} else {
					event = map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": json.RawMessage(proposal), "modelUsage": map[string]any{"main": map[string]int{"inputTokens": 3, "cacheCreationInputTokens": 5, "cacheReadInputTokens": 7, "outputTokens": 11}}}
					script += `minion_session_flag=false
for minion_arg do
  if $minion_session_flag; then
    printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$minion_arg"
    break
  fi
  case "$minion_arg" in --session-id|--resume) minion_session_flag=true ;; esac
done
`
				}
				encoded, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				script += "printf '%s\\n' " + shellQuote(string(encoded)) + "\n"
				if provider == "codex" {
					script += "printf '%s\\n' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":15,\"cached_input_tokens\":7,\"output_tokens\":11}}'\n"
				}
				fakeOnPath(t, provider, script)
				var session modelSession
				t.Cleanup(func() {
					if err := session.close(); err != nil {
						t.Error(err)
					}
				})
				runner := Runner{Codex: config.CodexConfig{APIKey: tc.key}, Claude: config.ClaudeConfig{APIKey: tc.key}}
				result, err := runner.runNative(context.Background(), modelRun{session: &session, provider: provider, dir: t.TempDir(), prompt: mainPrompt("inert fixture"), outputSchema: mainOutputSchema})
				if err != nil {
					t.Fatal(err)
				}
				answer, refutations, err := parseProposal(result.Answer)
				if err != nil || answer != "public [redacted]" || refutations != "private [redacted]" {
					t.Fatalf("decoded answer=%q refutations=%q error=%v", answer, refutations, err)
				}
			})
		}
	}
}
