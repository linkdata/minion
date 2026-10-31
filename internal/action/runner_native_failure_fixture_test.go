package action

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
)

func nativeFailureScript(provider, kind, secret, started string) string {
	if kind == "unavailable" {
		return "#!" + started + "\n"
	}
	script := "#!/bin/sh\ncat >/dev/null\n"
	if provider == "codex" {
		script += `printf '%s\n' '{"type":"thread.started","thread_id":"native"}'` + "\n"
	} else {
		script += `minion_next=false
for minion_arg do
  if $minion_next; then
    printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$minion_arg"
    break
  fi
  [ "$minion_arg" != --session-id ] || minion_next=true
done
`
	}
	switch kind {
	case "rejected":
		if provider == "codex" {
			script += `printf '%s\n' '{"type":"turn.failed","error":{"message":"model rejected request"}}'` + "\n"
		} else {
			script += `printf '%s\n' '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}'` + "\n"
		}
	case "malformed":
		script += "printf '%s\\n' '{bad'\n"
	case "failed":
		script += "printf '%s\\n' " + shellQuote("stderr tail: "+secret) + " >&2\nexit 7\n"
	case "canceled", "expired":
		script += "printf started > " + shellQuote(started) + "\nexec sleep 2\n"
	}
	return script
}

// A distinct Done channel makes child contexts observe this fixture's deadline error.
type nativeDeadlineContext struct {
	context.Context
	done <-chan struct{}
}

func (c nativeDeadlineContext) Done() <-chan struct{} { return c.done }

func (c nativeDeadlineContext) Err() error {
	if c.Context.Err() != nil && errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

func TestRunnerNativeAdaptersRetainFailedCallRecords(t *testing.T) {
	const secret = "fixture-native-secret"
	type runOutcome struct {
		result Result
		err    error
	}
	for _, provider := range []string{"codex", "claude"} {
		for _, kind := range []string{"rejected", "malformed", "failed", "canceled", "expired", "unavailable"} {
			t.Run(provider+"/"+kind, func(t *testing.T) {
				installActionTestSkills(t)
				t.Setenv("CODEX_HOME", t.TempDir())
				for _, key := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
					t.Setenv(key, "")
				}
				started := t.TempDir() + "/started"
				fakeOnPath(t, provider, nativeFailureScript(provider, kind, secret, started))
				job := testJob(command.Comment)
				job.Main.Provider, job.Main.Model = provider, "fixture-"+provider
				var saved []attemptRecord
				queue := newProviderEventQueue(func(event attemptRecord) error {
					saved = append(saved, event)
					return nil
				})
				runner := Runner{WorkRoot: t.TempDir(), eventSink: queue.add}
				if provider == "codex" {
					runner.Codex = config.CodexConfig{APIKey: secret}
				} else {
					runner.Claude = config.ClaudeConfig{APIKey: secret}
				}
				var got runOutcome
				switch kind {
				case "canceled", "expired":
					var ctx context.Context
					var cancel func()
					if kind == "expired" {
						base, expire := context.WithCancelCause(t.Context())
						observed := make(chan struct{})
						stop := context.AfterFunc(base, func() { close(observed) })
						defer stop()
						ctx = nativeDeadlineContext{Context: base, done: observed}
						cancel = func() { expire(context.DeadlineExceeded) }
					} else {
						ctx, cancel = context.WithCancel(t.Context())
					}
					defer cancel()
					done := make(chan runOutcome, 1)
					go func() {
						result, err := runner.Run(ctx, job)
						done <- runOutcome{result, err}
					}()
					ticker := time.NewTicker(5 * time.Millisecond)
					defer ticker.Stop()
					deadline := time.NewTimer(10 * time.Second)
					defer deadline.Stop()
				waitStart:
					for {
						if _, err := os.Stat(started); err == nil {
							break waitStart
						}
						select {
						case got = <-done:
							t.Fatalf("CLI exited before %s: %v", kind, got.err)
						case <-ticker.C:
						case <-deadline.C:
							t.Fatalf("fake CLI did not start before %s", kind)
						}
					}
					cancel()
					select {
					case got = <-done:
					case <-time.After(2 * time.Second):
						t.Fatalf("%s native call did not stop", kind)
					}
				default:
					got.result, got.err = runner.Run(t.Context(), job)
				}
				if got.err == nil || got.result.Answer != "" || len(got.result.Calls) != 1 {
					t.Fatalf("native failure changed result: %+v, %v", got.result, got.err)
				}
				if kind == "canceled" && !errors.Is(got.err, context.Canceled) ||
					kind == "expired" && !errors.Is(got.err, context.DeadlineExceeded) {
					t.Fatalf("native failure lost cancellation cause: %v", got.err)
				}
				if queue.finish() || len(saved) == 0 {
					t.Fatalf("native failure lost call records or falsely proved coverage: %+v", saved)
				}
				failure := saved[len(saved)-1]
				observedAt, timeErr := time.Parse(time.RFC3339Nano, failure.ObservedAt)
				if failure.Kind != attemptRecordDiagnostic || failure.EventSource != provider+".call.failed" ||
					failure.Call != "1" || failure.Role != roleMain || failure.Round != 1 || failure.Agent != "" ||
					failure.Provider != provider || failure.Model != job.Main.Model || timeErr != nil ||
					observedAt.IsZero() || !strings.HasSuffix(failure.ObservedAt, "Z") ||
					!strings.Contains(failure.Text, "content may be incomplete") || strings.Contains(failure.Text, secret) {
					t.Fatalf("native failure diagnostic lost binding or redaction: %+v", failure)
				}
				switch kind {
				case "rejected":
					if !strings.Contains(failure.Text, map[string]string{"codex": "model rejected request", "claude": "provider usage limit reached"}[provider]) {
						t.Fatalf("rejection reason missing: %+v", failure)
					}
				case "malformed":
					if !strings.Contains(failure.Text, "invalid") {
						t.Fatalf("malformed stream reason missing: %+v", failure)
					}
				case "failed":
					if !strings.Contains(failure.Text, "stderr tail") || !strings.Contains(failure.Text, "[redacted]") {
						t.Fatalf("stderr tail or redaction missing: %+v", failure)
					}
				case "unavailable":
					if !strings.Contains(failure.Text, "no such file") {
						t.Fatalf("CLI-unavailable detail missing: %+v", failure)
					}
				}
			})
		}
	}
}

func TestRunnerNativePartialCriticVerdictStaysPrivateOnFailure(t *testing.T) {
	const partial = `{"review_complete":true,"approved":false,"has_blockers":true,"feedback":"PRIVATE_PARTIAL"}`
	const mainProposal = `{"answer":"Candidate.","refutations":""}`
	if _, _, _, _, err := parseCritique(partial); err != nil {
		t.Fatalf("fixture is not a valid critic report: %v", err)
	}
	for _, critic := range []string{"codex", "claude"} {
		t.Run(critic, func(t *testing.T) {
			installActionTestSkills(t)
			t.Setenv("CODEX_HOME", t.TempDir())
			for _, key := range []string{"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
				t.Setenv(key, "")
			}
			job := withClaudeCritic(testJob(command.Comment))
			job.Critic.Provider = critic
			if critic == "codex" {
				job.Main.Provider = "claude"
			}
			for _, provider := range []string{"codex", "claude"} {
				answer := mainProposal
				if provider == critic {
					answer = partial
				}
				script := "#!/bin/sh\ncat >/dev/null\n"
				if provider == "codex" {
					const id = "11111111-1111-4111-8111-000000000001"
					if provider != critic {
						script += `dir="$CODEX_HOME/sessions/fixture"
mkdir -p "$dir"
printf '%s\n' '{"type":"session_meta","payload":{"id":"` + id + `","session_id":"` + id + `","source":"exec"}}' '{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}' '{"type":"token_usage_record","payload":{"thread_id":"` + id + `","session_id":"` + id + `","turn_id":"turn-1","response_id":"response-1","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":2}}}' '{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1"}}' > "$dir/rollout-` + id + `.jsonl"
`
					}
					script += "printf '%s\\n' " + shellQuote(`{"type":"thread.started","thread_id":"`+id+`"}`) + " " + shellQuote(`{"type":"item.completed","item":{"type":"agent_message","text":`+strconv.Quote(answer)+`}}`) + "\n"
					if provider == critic {
						script += `printf '%s\n' '{"type":"turn.failed","error":{"message":"critic stopped"}}'
exit 7
`
					} else {
						script += `printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":0,"output_tokens":2}}'
`
					}
				} else {
					script += `next=false
for arg do
  if $next; then printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$arg"; break; fi
  [ "$arg" != --session-id ] || next=true
done
`
					result := `{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","structured_output":` + answer + `,"usage":{"input_tokens":3,"output_tokens":2},"modelUsage":{"fixture":{"inputTokens":3,"cacheCreationInputTokens":0,"cacheReadInputTokens":0,"outputTokens":2}}}`
					script += "printf '%s\\n' " + shellQuote(result) + "\n"
					if provider == critic {
						script += `printf '%s\n' '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}'
exit 7
`
					}
				}
				fakeOnPath(t, provider, script)
			}
			var saved []attemptRecord
			queue := newProviderEventQueue(func(event attemptRecord) error {
				saved = append(saved, event)
				return nil
			})
			runner := Runner{WorkRoot: t.TempDir(), eventSink: queue.add,
				Codex: config.CodexConfig{APIKey: "fixture-codex-key"}, Claude: config.ClaudeConfig{APIKey: "fixture-claude-key"}}
			result, err := runner.Run(t.Context(), job)
			if err == nil || result.Answer != "" || result.CriticStopReason != "" || len(result.Calls) != 2 ||
				!result.Calls[0].Complete || result.Calls[1].Complete || result.Calls[1].Approved || result.Calls[1].NoBlockers ||
				result.Calls[1].HasBlockers || result.Calls[1].ReviewComplete || result.Validate(job) == nil {
				t.Fatalf("partial native critic result became publishable: result=%+v error=%v", result, err)
			}
			if queue.finish() {
				t.Fatal("failed native critic falsely proved record coverage")
			}
			observed, failed := false, false
			for _, record := range saved {
				if record.Role != roleCritic {
					continue
				}
				if record.Kind == attemptRecordReport || record.EventSource == critic+".final" {
					t.Fatalf("failed critic emitted a final report: %+v", record)
				}
				if record.Text == partial && record.Call == "2" && record.Round == 1 && record.ReviewPhase == reviewFull &&
					((critic == "codex" && record.Kind == attemptRecordDiscussion && record.EventSource == "codex.assistant") ||
						(critic == "claude" && record.Kind == attemptRecordDiagnostic && record.EventSource == "claude.structured_output_observed")) {
					observed = true
				}
				if record.Kind == attemptRecordDiagnostic && record.EventSource == critic+".call.failed" && record.Call == "2" {
					failed = true
				}
			}
			if !observed || !failed {
				t.Fatalf("partial private observation or native failure missing: %+v", saved)
			}
		})
	}
}
