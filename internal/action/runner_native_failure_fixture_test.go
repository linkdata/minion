package action

import (
	"context"
	"errors"
	"os"
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
