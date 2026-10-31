package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/bounded"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
	"github.com/linkdata/minion/internal/usage"
)

func TestRunnerRecordsProviderCallFailuresWithoutClaimingCoverage(t *testing.T) {
	const secret = "fixture-call-secret"
	for _, provider := range []string{"codex", "claude"} {
		for _, test := range []struct {
			name, detail string
			cause        error
			noInput      bool
		}{
			{"rejected", "rate limit", errors.New("rejected: stderr tail: rate limit " + secret), false},
			{"malformed", "invalid event", errors.New("malformed JSONL: stderr tail: invalid event " + secret), false},
			{"failed", "native exit", errors.New("failed: stderr tail: native exit " + secret), false},
			{"cancelled", "shutdown", fmt.Errorf("cancelled: stderr tail: shutdown %s: %w", secret, context.Canceled), false},
			{"expired", "timeout", fmt.Errorf("expired: stderr tail: timeout %s: %w", secret, context.DeadlineExceeded), false},
			{"CLI unavailable", "CLI unavailable", fmt.Errorf("CLI unavailable: %w", exec.ErrNotFound), true},
		} {
			t.Run(provider+"/"+test.name, func(t *testing.T) {
				job := testJob(command.Comment)
				job.Main.Provider, job.Main.Model = provider, "fixture-"+provider
				var saved []attemptRecord
				queue := newProviderEventQueue(func(event attemptRecord) error {
					saved = append(saved, event)
					return nil
				})
				runner := Runner{WorkRoot: t.TempDir(), eventSink: queue.add,
					Codex: config.CodexConfig{APIKey: secret}, Claude: config.ClaudeConfig{APIKey: secret}}
				runner.runModel = func(_ context.Context, request modelRun) (aicli.Result, error) {
					if request.onInput == nil || request.onEvent == nil || request.provider != provider || request.model != job.Main.Model {
						t.Fatalf("provider call lost event hooks or selection: %+v", request)
					}
					if !test.noInput {
						request.onInput(aicli.PreparedInput{NativeID: "native", Mode: "fresh", Path: "/fixture/cli", Args: []string{"cli"}, Dir: request.dir, Stdin: "prompt"})
						request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant", Text: "partial content"})
					}
					return aicli.Result{}, test.cause
				}
				result, err := runner.Run(t.Context(), job)
				if !errors.Is(err, test.cause) || result.Answer != "" || len(result.Calls) != 1 || result.Calls[0].Complete {
					t.Fatalf("provider failure changed required result: %+v, %v", result, err)
				}
				if queue.finish() || len(saved) != map[bool]int{true: 1, false: 3}[test.noInput] {
					t.Fatalf("failure marker lost or falsely proved coverage: %+v", saved)
				}
				for _, event := range saved {
					observedAt, parseErr := time.Parse(time.RFC3339Nano, event.ObservedAt)
					if event.Call != "1" || event.Role != roleMain || event.Round != 1 || event.ReviewPhase != "" ||
						event.Provider != provider || event.Model != job.Main.Model || parseErr != nil ||
						observedAt.IsZero() || !strings.HasSuffix(event.ObservedAt, "Z") {
						t.Fatalf("event lost selected call labels or worker UTC time: %+v", event)
					}
				}
				failure := saved[len(saved)-1]
				if failure.Kind != attemptRecordDiagnostic || failure.Agent != "" || failure.EventSource != provider+".call.failed" ||
					!strings.Contains(failure.Text, test.detail) || !strings.Contains(failure.Text, "content may be incomplete") ||
					strings.Contains(failure.Text, secret) || len(failure.Text) > maxAttemptRecordTextBytes || !utf8.ValidString(failure.Text) {
					t.Fatalf("failure diagnostic lost detail, bound, or redaction: %+v", failure)
				}
				if !test.noInput && (!strings.Contains(failure.Text, "[redacted]") || saved[1].Text != "partial content") {
					t.Fatalf("stderr tail redaction or known partial content lost: %+v", saved)
				}
			})
		}
	}
}

func TestProviderCallFailureTextFallsBackOnUnsafeOrMissingDetail(t *testing.T) {
	for _, detail := range []string{"", " \n\t", "invalid\xff", "invalid\x00text"} {
		text := providerCallFailureText(errors.New(detail))
		if text != "" {
			t.Fatalf("unsafe or missing diagnostic was retained: %q", text)
		}
	}
	if text := providerCallFailureText(errors.New("native failed"), "provider"); text != "" {
		t.Fatalf("diagnostic label exposed a configured credential: %q", text)
	}
	const secret = "fixture-call-secret"
	text := providerCallFailureText(errors.New(strings.Repeat("x", httpFailureMessageLimit-2)+secret+" stderr tail"), secret)
	if len(text) > maxAttemptRecordTextBytes || !utf8.ValidString(text) || strings.Contains(text, secret) ||
		!strings.Contains(text, "...") || !strings.HasSuffix(text, " stderr tail") {
		t.Fatalf("long failure was not bounded after whole-error redaction: %q", text)
	}
}

func TestProviderCallFailureTextKeepsTrailingResetAfterRedactionExpansion(t *testing.T) {
	const secret, reset = "Z", " quota resets in 17 minutes"
	text := providerCallFailureText(errors.New(strings.Repeat(secret, bounded.DiagnosticBytes-len(reset))+reset), secret)
	encoded, err := json.Marshal(text)
	if err != nil || len(text) > maxAttemptRecordTextBytes || !utf8.ValidString(text) ||
		!strings.Contains(text, "...") || !strings.HasSuffix(text, reset) || containsConfiguredKey(encoded, secret) {
		t.Fatalf("fallback diagnostic lost trailing reset or exposed key: %q, %v", text, err)
	}
}

func TestRunnerMarksUnavailableCallDiagnosticWithoutCoverage(t *testing.T) {
	var saved []attemptRecord
	queue := newProviderEventQueue(func(event attemptRecord) error {
		saved = append(saved, event)
		return nil
	})
	runner := Runner{WorkRoot: t.TempDir(), eventSink: queue.add,
		runModel: func(context.Context, modelRun) (aicli.Result, error) {
			return aicli.Result{}, errors.New("invalid\xff")
		}}
	if _, err := runner.Run(t.Context(), testJob(command.Comment)); err == nil {
		t.Fatal("invalid native diagnostic did not fail the call")
	}
	if queue.finish() || len(saved) != 1 || saved[0].Kind != attemptRecordDiagnostic ||
		saved[0].EventSource != "codex.call.diagnostic_unavailable" || saved[0].Text != "" {
		t.Fatalf("unsafe diagnostic was retained or falsely proved coverage: %+v", saved)
	}
}

func TestRunnerDoesNotRecordRefusedBeforeCallAsProviderFailure(t *testing.T) {
	refusal := errors.New("before-call checkpoint refused")
	var saved []attemptRecord
	runner := Runner{WorkRoot: t.TempDir(), runModel: forbidModel(t), eventSink: func(event attemptRecord) {
		saved = append(saved, event)
	}}
	result, err := runner.Execute(t.Context(), testJob(command.Comment), runnerExecutionCheckpoints{
		before: func(CallCheckpoint) error { return refusal },
	})
	if !errors.Is(err, refusal) || len(result.Calls) != 0 || len(saved) != 0 {
		t.Fatalf("checkpoint refusal was misreported as a provider call: result=%+v records=%+v error=%v", result, saved, err)
	}
}

func TestRunnerRecordsNativeDiagnosticsWithoutChangingSuccess(t *testing.T) {
	const secret = "private-native-key"
	for _, test := range []struct {
		name, code string
		complete   bool
	}{
		{"stderr", "stderr_tail", true},
		{"unrecognized code", "forged_journal_event", false},
		{"truncated diagnostics", "diagnostics_truncated", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var saved []attemptRecord
			queue := newProviderEventQueue(func(event attemptRecord) error {
				saved = append(saved, event)
				return nil
			})
			runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: secret}, eventSink: queue.add,
				runModel: func(context.Context, modelRun) (aicli.Result, error) {
					return aicli.Result{Answer: "Done.", Diagnostics: []aicli.ProviderDiagnostic{{Code: test.code, Message: "native warning " + secret}}}, nil
				}}
			result, err := runner.Run(t.Context(), testJob(command.Comment))
			if err != nil || result.Answer != "Done." || len(result.Calls) != 1 || len(result.Diagnostics) != 0 {
				t.Fatalf("diagnostic changed result: %+v, %v", result, err)
			}
			if got := queue.finish(); got != test.complete || len(saved) != 1 {
				t.Fatalf("coverage=%t records=%+v", got, saved)
			}
			if test.code == "forged_journal_event" {
				if saved[0].Kind != attemptRecordDiscussion || saved[0].EventSource != "codex.content_missing" || saved[0].Text != "" {
					t.Fatalf("forged source reached journal: %+v", saved[0])
				}
			} else if saved[0].Kind != attemptRecordDiagnostic || saved[0].EventSource != "codex."+test.code ||
				saved[0].Call != "1" || saved[0].Role != roleMain || saved[0].Agent != "" ||
				!strings.Contains(saved[0].Text, "[redacted]") || strings.Contains(saved[0].Text, secret) {
				t.Fatalf("call-bound private diagnostic lost: %+v", saved[0])
			}
		})
	}
}

func TestRunnerMarksActionTruncatedDiagnosticsWithoutLosingNativeCode(t *testing.T) {
	const reset = " quota resets in 17 minutes"
	const marker = "..."
	head := (bounded.DiagnosticBytes - len(marker)) / 2
	tail := bounded.DiagnosticBytes - len(marker) - head
	for _, test := range []struct {
		name, secret, message, want string
	}{
		{"cross-provider redaction expands", "Z", strings.Repeat("Z", bounded.DiagnosticBytes-len(reset)) + reset, reset},
		{"head and tail join a secret", "left...right", strings.Repeat("a", head-len("left")) + "left" + strings.Repeat("b", 100) + "right" + strings.Repeat("c", tail-len("right")), "[redacted]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var saved []attemptRecord
			queue := newProviderEventQueue(func(event attemptRecord) error {
				saved = append(saved, event)
				return nil
			})
			runner := Runner{WorkRoot: t.TempDir(), Claude: config.ClaudeConfig{APIKey: test.secret}, eventSink: queue.add,
				runModel: func(context.Context, modelRun) (aicli.Result, error) {
					return aicli.Result{Answer: "Done.", Diagnostics: []aicli.ProviderDiagnostic{{Code: "stderr_tail", Message: test.message}}}, nil
				}}
			result, err := runner.Run(t.Context(), testJob(command.Comment))
			if err != nil || result.Answer != "Done." || len(result.Calls) != 1 {
				t.Fatalf("diagnostic changed result: %+v, %v", result, err)
			}
			if queue.finish() || len(saved) != 2 || saved[0].Kind != attemptRecordDiagnostic ||
				saved[0].EventSource != "codex.stderr_tail" || saved[0].Call != "1" ||
				saved[1].Kind != attemptRecordDiagnostic || saved[1].EventSource != "codex.diagnostics_truncated" ||
				saved[1].Call != "1" || saved[1].Text != "" {
				t.Fatalf("action truncation lost native code or claimed coverage: %+v", saved)
			}
			encoded, err := json.Marshal(saved[0].Text)
			if err != nil || len(saved[0].Text) > bounded.DiagnosticBytes || !utf8.ValidString(saved[0].Text) ||
				!strings.Contains(saved[0].Text, test.want) || containsConfiguredKey(encoded, test.secret) {
				t.Fatalf("diagnostic lost tail or exposed configured key: %+v, %v", saved[0], err)
			}
		})
	}
}

func TestRunnerWithholdsCrossRecordDiagnosticSecret(t *testing.T) {
	wantTokens := usage.Tokens{Input: 3, Output: 2}
	for _, provider := range []struct{ name, code string }{{"codex", "stderr_tail"}, {"claude", "result_detail"}} {
		for _, test := range []struct {
			name, secret string
			pieces       []string
		}{
			{"two pieces", "fixture-secret", []string{"fixture-", "secret"}},
			{"three pieces", "fixture-secret", []string{"fixture-", "sec", "ret"}},
			{"JSON escaped pieces", "<tag>", []string{`\u003c`, "tag", `\u003e`}},
		} {
			t.Run(provider.name+"/"+test.name, func(t *testing.T) {
				job := testJob(command.Comment)
				job.Main.Provider, job.Main.Model = provider.name, "fixture-"+provider.name
				var saved []attemptRecord
				queue := newProviderEventQueue(func(event attemptRecord) error {
					saved = append(saved, event)
					return nil
				})
				runner := Runner{WorkRoot: t.TempDir(), eventSink: queue.add,
					runModel: func(context.Context, modelRun) (aicli.Result, error) {
						diagnostics := make([]aicli.ProviderDiagnostic, len(test.pieces))
						for i, piece := range test.pieces {
							diagnostics[i] = aicli.ProviderDiagnostic{Code: provider.code, Message: piece}
						}
						return aicli.Result{Answer: "Done.", Tokens: wantTokens, UsageComplete: true, Diagnostics: diagnostics}, nil
					}}
				if provider.name == "codex" {
					runner.Claude.APIKey = test.secret
				} else {
					runner.Codex.APIKey = test.secret
				}
				result, err := runner.Run(t.Context(), job)
				if err != nil || result.Answer != "Done." || len(result.Calls) != 1 ||
					result.Calls[0].Tokens != wantTokens || !result.Calls[0].Complete {
					t.Fatalf("diagnostic gap changed answer or usage: %+v, %v", result, err)
				}
				if queue.finish() || len(saved) != len(test.pieces)+1 {
					t.Fatalf("split key claimed coverage or lost gap: %+v", saved)
				}
				var joined strings.Builder
				for i, piece := range test.pieces {
					want := piece
					if i == len(test.pieces)-1 {
						want = ""
					}
					if saved[i].Kind != attemptRecordDiagnostic || saved[i].EventSource != provider.name+"."+provider.code ||
						saved[i].Call != "1" || saved[i].Text != want {
						t.Fatalf("native diagnostic %d lost code or safe text: %+v", i, saved[i])
					}
					joined.WriteString(saved[i].Text)
				}
				gap := saved[len(saved)-1]
				if gap.Kind != attemptRecordDiagnostic || gap.EventSource != provider.name+".diagnostics_truncated" ||
					gap.Call != "1" || gap.Text != "" {
					t.Fatalf("split key lost call-bound gap: %+v", gap)
				}
				encoded, err := json.Marshal(joined.String())
				if err != nil || containsConfiguredKey([]byte(joined.String()), test.secret) || containsConfiguredKey(encoded, test.secret) {
					t.Fatalf("saved diagnostics reconstruct configured key: %q, %v", joined.String(), err)
				}
			})
		}
	}
}

func assertNoConfiguredKeyInOrderedText(t *testing.T, records []attemptRecord, secret string) {
	t.Helper()
	var joined strings.Builder
	for _, record := range records {
		joined.WriteString(record.Text)
	}
	encoded, err := json.Marshal(joined.String())
	if err != nil || containsConfiguredKey([]byte(joined.String()), secret) || containsConfiguredKey(encoded, secret) {
		t.Fatalf("ordered record text reconstructs configured key: %q, %v", joined.String(), err)
	}
}

func TestRunnerWithholdsStreamFinalDiagnosticKeyAcrossProviders(t *testing.T) {
	wantTokens := usage.Tokens{Input: 3, Output: 2}
	for _, provider := range []struct{ name, code string }{{"codex", "native_error"}, {"claude", "result_detail"}} {
		for _, test := range []struct {
			name, secret string
			pieces       [3]string
		}{
			{"plain", "fixture-secret", [3]string{"fixture-", "sec", "ret"}},
			{"JSON escaped", "<tag>", [3]string{`\u003c`, "tag", `\u003e`}},
			{"HTML numeric", "<tag>", [3]string{"&#x3c;", "tag", "&#x3e;"}},
			{"long JSON escaped", strings.Repeat("a", 2048) + "Z", [3]string{
				strings.Repeat(`\u0061`, 1024), strings.Repeat(`\u0061`, 1024), `\u005a`}},
		} {
			t.Run(provider.name+"/"+test.name, func(t *testing.T) {
				job := testJob(command.Comment)
				job.Main.Provider, job.Main.Model = provider.name, "fixture-"+provider.name
				var saved []attemptRecord
				queue := newProviderEventQueue(func(event attemptRecord) error {
					saved = append(saved, event)
					return nil
				})
				runner := Runner{WorkRoot: t.TempDir(), eventSink: queue.add,
					runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
						request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant", Text: test.pieces[0]})
						request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "final", Text: test.pieces[1]})
						return aicli.Result{Answer: "Done.", Tokens: wantTokens, UsageComplete: true,
							Diagnostics: []aicli.ProviderDiagnostic{{Code: provider.code, Message: test.pieces[2]}, {Code: provider.code, Message: "safe later diagnostic"}}}, nil
					}}
				if provider.name == "codex" {
					runner.Claude.APIKey = test.secret
				} else {
					runner.Codex.APIKey = test.secret
				}
				result, err := runner.Run(t.Context(), job)
				if err != nil || result.Answer != "Done." || len(result.Calls) != 1 ||
					result.Calls[0].Tokens != wantTokens || !result.Calls[0].Complete {
					t.Fatalf("diagnostic gap changed answer or usage: %+v, %v", result, err)
				}
				if queue.finish() || len(saved) != 5 || saved[0].EventSource != provider.name+".assistant" ||
					saved[0].Text != test.pieces[0] || saved[1].EventSource != provider.name+".final" ||
					saved[1].Text != test.pieces[1] || saved[2].Kind != attemptRecordDiagnostic ||
					saved[2].EventSource != provider.name+"."+provider.code || saved[2].Text != "" ||
					saved[3].EventSource != provider.name+".diagnostics_truncated" || saved[3].Text != "" ||
					saved[4].EventSource != provider.name+"."+provider.code || saved[4].Text != "safe later diagnostic" {
					t.Fatalf("native code, safe text, or coverage gap lost: %+v", saved)
				}
				assertNoConfiguredKeyInOrderedText(t, saved, test.secret)
			})
		}
	}
}

func TestRunnerWithholdsDiagnosticStreamFeedbackKeyAcrossCalls(t *testing.T) {
	const secret = "fixture-secret"
	mainTokens, criticTokens := usage.Tokens{Input: 2}, usage.Tokens{Input: 3, Output: 1}
	job := withClaudeCritic(testJob(command.Comment))
	var saved []attemptRecord
	queue := newProviderEventQueue(func(event attemptRecord) error {
		saved = append(saved, event)
		return nil
	})
	runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: secret}, eventSink: queue.add,
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			if request.role == roleMain {
				return aicli.Result{Answer: proposalJSON("Candidate.", ""), Tokens: mainTokens, UsageComplete: true,
					Diagnostics: []aicli.ProviderDiagnostic{{Code: "native_error", Message: "fixture-"}}}, nil
			}
			request.onEvent(aicli.StreamEvent{NativeID: "critic-native", Kind: "assistant", Text: "sec"})
			return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"review_complete":false,"feedback":"ret"}`,
				Tokens: criticTokens, UsageComplete: true}, nil
		}}
	result, err := runner.Run(t.Context(), job)
	if err == nil || !strings.Contains(err.Error(), "critic review incomplete") || result.Answer != "" ||
		len(result.Calls) != 2 || result.Calls[0].Tokens != mainTokens || result.Calls[1].Tokens != criticTokens ||
		!result.Calls[0].Complete || !result.Calls[1].Complete || !result.Calls[1].HasBlockers {
		t.Fatalf("feedback gap changed answer or usage: %+v, %v", result, err)
	}
	if queue.finish() || len(saved) != 4 || saved[0].EventSource != "codex.native_error" || saved[0].Text != "fixture-" ||
		saved[1].EventSource != "claude.assistant" || saved[1].Text != "sec" ||
		saved[2].Kind != attemptRecordReport || saved[2].EventSource != "minion.critic.feedback" || saved[2].Text != "" ||
		saved[3].Kind != attemptRecordDiscussion || saved[3].EventSource != "claude.content_missing" || saved[3].Text != "" {
		t.Fatalf("cross-call feedback gap lost: %+v", saved)
	}
	assertNoConfiguredKeyInOrderedText(t, saved, secret)
}

func TestRunnerPreservesSafeDiscussionWithConfiguredKey(t *testing.T) {
	const secret = "fixture-secret"
	job := withClaudeCritic(testJob(command.Comment))
	var saved []attemptRecord
	queue := newProviderEventQueue(func(event attemptRecord) error {
		saved = append(saved, event)
		return nil
	})
	runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: secret}, eventSink: queue.add,
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant", Text: "ordinary work"})
			if request.role == roleMain {
				return aicli.Result{Answer: proposalJSON("Candidate.", ""),
					Diagnostics: []aicli.ProviderDiagnostic{{Code: "native_error", Message: "native warning"}}}, nil
			}
			return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"review_complete":false,"feedback":"ordinary finding"}`}, nil
		}}
	_, err := runner.Run(t.Context(), job)
	if err == nil || !strings.Contains(err.Error(), "critic review incomplete") || !queue.finish() || len(saved) != 4 ||
		saved[0].Text != "ordinary work" || saved[1].EventSource != "codex.native_error" || saved[1].Text != "native warning" ||
		saved[2].Text != "ordinary work" || saved[3].EventSource != "minion.critic.feedback" || saved[3].Text != "ordinary finding" {
		t.Fatalf("safe discussion or native diagnostic was changed: %+v, %v", saved, err)
	}
}

func TestRunnerRetainsRedactedStreamDiscussionAndCoverage(t *testing.T) {
	const secret = "fixture-secret"
	wantTokens := usage.Tokens{Input: 3, Output: 2}
	var saved []attemptRecord
	queue := newProviderEventQueue(func(event attemptRecord) error {
		saved = append(saved, event)
		return nil
	})
	runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: secret}, eventSink: queue.add,
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant", Text: "before " + secret + " after"})
			request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "final", Text: "ordinary later text"})
			return aicli.Result{Answer: "Done.", Tokens: wantTokens, UsageComplete: true}, nil
		}}
	result, err := runner.Run(t.Context(), testJob(command.Comment))
	if err != nil || result.Answer != "Done." || len(result.Calls) != 1 ||
		result.Calls[0].Tokens != wantTokens || !result.Calls[0].Complete || !queue.finish() || len(saved) != 2 ||
		saved[0].EventSource != "codex.assistant" || saved[0].Text != "before [redacted] after" ||
		saved[1].EventSource != "codex.final" || saved[1].Text != "ordinary later text" {
		t.Fatalf("safe redacted stream text or coverage lost: result=%+v records=%+v error=%v", result, saved, err)
	}
	assertNoConfiguredKeyInOrderedText(t, saved, secret)
}

func TestRunnerWithholdsLongHTMLNumericKeyAcrossEvents(t *testing.T) {
	const secret = "xA"
	zeros := strings.Repeat("0", 300)
	for _, test := range []struct {
		name   string
		pieces []string
	}{
		{"entity split", []string{"x", "&#" + zeros[:100], zeros[100:200], zeros[200:] + "65;"}},
		{"introducer split", []string{"x&", "#" + zeros[:100], zeros[100:200], zeros[200:] + "65;"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var saved []attemptRecord
			queue := newProviderEventQueue(func(event attemptRecord) error {
				saved = append(saved, event)
				return nil
			})
			runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: secret}, eventSink: queue.add,
				runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					for i, piece := range test.pieces {
						kind := "assistant"
						if i == len(test.pieces)-1 {
							kind = "final"
						}
						request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: kind, Text: piece})
					}
					request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant", Text: "safe later text"})
					return aicli.Result{Answer: "Done."}, nil
				}}
			result, err := runner.Run(t.Context(), testJob(command.Comment))
			if err != nil || result.Answer != "Done." || queue.finish() || len(saved) != len(test.pieces)+2 {
				t.Fatalf("long numeric entity changed answer or coverage: result=%+v records=%+v error=%v", result, saved, err)
			}
			for i, piece := range test.pieces[:len(test.pieces)-1] {
				if saved[i].Text != piece {
					t.Fatalf("earlier safe text %d lost: %+v", i, saved)
				}
			}
			last := len(test.pieces) - 1
			if saved[last].EventSource != "codex.final" || saved[last].Text != "" ||
				saved[last+1].EventSource != "codex.content_missing" || saved[last+1].Text != "" ||
				saved[last+2].Text != "safe later text" {
				t.Fatalf("long numeric entity gap or later safe text lost: %+v", saved)
			}
			assertNoConfiguredKeyInOrderedText(t, saved, secret)
		})
	}
}

func TestRunnerPreservesSafeHTMLNumericDiscussion(t *testing.T) {
	var saved []attemptRecord
	queue := newProviderEventQueue(func(event attemptRecord) error {
		saved = append(saved, event)
		return nil
	})
	runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: "xA"}, eventSink: queue.add,
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant", Text: "ordinary &#00066; discussion"})
			request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "final", Text: " still safe"})
			return aicli.Result{Answer: "Done."}, nil
		}}
	result, err := runner.Run(t.Context(), testJob(command.Comment))
	if err != nil || result.Answer != "Done." || !queue.finish() || len(saved) != 2 ||
		saved[0].Text != "ordinary &#00066; discussion" || saved[1].Text != " still safe" {
		t.Fatalf("safe numeric discussion was lost: result=%+v records=%+v error=%v", result, saved, err)
	}
}

func TestGuardAttemptRecordTextStopsAfterHistoryOrScanExhaustion(t *testing.T) {
	for _, test := range []struct {
		name, first, second string
		historyLimit        int
		scanLimit           int64
	}{
		{"history", "x&#", strings.Repeat("0", 10), 12, 1000},
		{"scan after completed entity", "safe &#66;", "more", 100, 40},
	} {
		t.Run(test.name, func(t *testing.T) {
			var saved []attemptRecord
			queue := newProviderEventQueue(func(event attemptRecord) error {
				saved = append(saved, event)
				return nil
			})
			guard := guardAttemptRecordTextWithLimits(queue.add, []string{"xA"}, test.historyLimit, test.scanLimit)
			guard(attemptRecord{Kind: attemptRecordDiscussion, Call: "1", Provider: "codex", Agent: "native", EventSource: "codex.assistant", Text: test.first})
			guard(attemptRecord{Kind: attemptRecordDiscussion, Call: "1", Provider: "codex", Agent: "native", EventSource: "codex.final", Text: test.second})
			guard(attemptRecord{Kind: attemptRecordDiagnostic, Call: "1", Provider: "codex", EventSource: "codex.native_error", Text: "safe later diagnostic"})
			if queue.finish() || len(saved) != 4 || saved[0].Text != test.first ||
				saved[1].EventSource != "codex.final" || saved[1].Text != "" ||
				saved[2].EventSource != "codex.content_missing" || saved[2].Text != "" ||
				saved[3].EventSource != "codex.native_error" || saved[3].Text != "" {
				t.Fatalf("text resumed or native code/gap lost after %s exhaustion: %+v", test.name, saved)
			}
		})
	}
}

func TestGuardAttemptRecordTextKeepsOrdinaryEventsOutsideScanBudget(t *testing.T) {
	var saved []attemptRecord
	guard := guardAttemptRecordTextWithLimits(func(event attemptRecord) { saved = append(saved, event) },
		[]string{strings.Repeat("x", maxWorkerCredentialBytes)}, int(maxAttemptRecordBytes), 1)
	for _, value := range []string{"ordinary first", "ordinary second"} {
		guard(attemptRecord{Kind: attemptRecordDiscussion, Provider: "codex", Agent: "native", EventSource: "codex.assistant", Text: value})
	}
	if len(saved) != 2 || saved[0].Text != "ordinary first" || saved[1].Text != "ordinary second" {
		t.Fatalf("ordinary rolling-window text spent numeric scan budget: %+v", saved)
	}
}

func TestRunnerWithholdsPreparedInputAndCallFailureJoins(t *testing.T) {
	for _, test := range []struct {
		name, secret, first, second, gap string
		failed                           bool
	}{
		{"prepared input", "}secret", "se", "cret", "codex.content_missing", false},
		{"call failure", "xprovider", "x", "", "codex.diagnostics_truncated", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var saved []attemptRecord
			queue := newProviderEventQueue(func(event attemptRecord) error {
				saved = append(saved, event)
				return nil
			})
			runner := Runner{WorkRoot: t.TempDir(), Codex: config.CodexConfig{APIKey: test.secret}, eventSink: queue.add,
				runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
					if test.failed {
						request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant", Text: test.first})
						return aicli.Result{}, errors.New("network down")
					}
					request.onInput(aicli.PreparedInput{NativeID: "native", Mode: "fresh", Path: "/fixture/cli", Args: []string{"cli"}, Dir: request.dir, Stdin: "prompt"})
					request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant", Text: test.first})
					request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "final", Text: test.second})
					request.onEvent(aicli.StreamEvent{NativeID: "native", Kind: "assistant", Text: "safe later event"})
					return aicli.Result{Answer: "Done."}, nil
				}}
			result, err := runner.Run(t.Context(), testJob(command.Comment))
			if (err != nil) != test.failed || (!test.failed && result.Answer != "Done.") {
				t.Fatalf("record gap changed result: %+v, %v", result, err)
			}
			if queue.finish() {
				t.Fatalf("cross-record key claimed coverage: %+v", saved)
			}
			foundGap, foundLater := false, false
			for _, event := range saved {
				if event.EventSource == test.gap && event.Text == "" {
					foundGap = true
				}
				if event.Text == "safe later event" {
					foundLater = true
				}
			}
			if !foundGap || !test.failed && !foundLater {
				t.Fatalf("gap or later safe event missing: %+v", saved)
			}
			if test.failed && (len(saved) != 3 || saved[1].EventSource != "codex.call.failed" || saved[1].Text != "") {
				t.Fatalf("call failure code or earlier safe text lost: %+v", saved)
			}
			assertNoConfiguredKeyInOrderedText(t, saved, test.secret)
		})
	}
}

func TestRunnerRecordsValidIncompleteCriticFeedbackPrivately(t *testing.T) {
	const feedback = "PRIVATE_UNRESOLVED_COVERAGE: delegated review did not finish."
	job := withClaudeCritic(testJob(command.Comment))
	var saved []attemptRecord
	queue := newProviderEventQueue(func(event attemptRecord) error {
		saved = append(saved, event)
		return nil
	})
	runner := Runner{WorkRoot: t.TempDir(), eventSink: queue.add,
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			if request.role == roleMain {
				return aicli.Result{Answer: proposalJSON("Candidate.", "")}, nil
			}
			return aicli.Result{Answer: `{"approved":false,"has_blockers":true,"review_complete":false,"feedback":"` + feedback + `"}`}, nil
		}}
	result, err := runner.Run(t.Context(), job)
	if err == nil || !strings.Contains(err.Error(), "critic review incomplete") || result.Answer != "" ||
		len(result.Calls) != 2 || result.Calls[1].Approved || result.Calls[1].NoBlockers || !result.Calls[1].HasBlockers || result.Calls[1].ReviewComplete || len(result.Diagnostics) != 0 {
		t.Fatalf("incomplete review changed verdict or result: %+v, %v", result, err)
	}
	if !queue.finish() || len(saved) != 1 || saved[0].Kind != attemptRecordReport || saved[0].Text != feedback ||
		saved[0].EventSource != "minion.critic.feedback" || saved[0].Call != "2" || saved[0].Role != roleCritic ||
		saved[0].Round != 1 || saved[0].ReviewPhase != reviewFull || saved[0].Agent != "" {
		t.Fatalf("incomplete private feedback missing: %+v", saved)
	}
}

func TestRunnerDoesNotPromotePartialProviderJSONToCriticFeedback(t *testing.T) {
	job := withClaudeCritic(testJob(command.Comment))
	const partial = `{"approved":false,"has_blockers":true,"review_complete":false,"feedback":"PRIVATE_PARTIAL"}`
	var saved []attemptRecord
	queue := newProviderEventQueue(func(event attemptRecord) error {
		saved = append(saved, event)
		return nil
	})
	runner := Runner{WorkRoot: t.TempDir(), eventSink: queue.add,
		runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
			if request.role == roleMain {
				return aicli.Result{Answer: proposalJSON("Candidate.", "")}, nil
			}
			return aicli.Result{Answer: partial, Diagnostics: []aicli.ProviderDiagnostic{
				{Code: "result_detail", Message: "capacity exhausted"},
				{Code: "result_errors", Message: `["reset tomorrow"]`},
				{Code: "structured_output_observed", Message: partial},
			}}, errors.New("provider stopped")
		}}
	result, err := runner.Run(t.Context(), job)
	if err == nil || result.Answer != "" || len(result.Calls) != 2 || result.Calls[1].Approved ||
		result.Calls[1].NoBlockers || result.Calls[1].HasBlockers || result.Calls[1].ReviewComplete {
		t.Fatalf("partial provider data became a verdict: %+v, %v", result, err)
	}
	if queue.finish() || len(saved) != 4 || saved[0].EventSource != "claude.result_detail" ||
		saved[0].Text != "capacity exhausted" || saved[1].EventSource != "claude.result_errors" ||
		saved[1].Text != `["reset tomorrow"]` || saved[2].Kind != attemptRecordDiagnostic ||
		saved[2].EventSource != "claude.structured_output_observed" || saved[2].Text != partial ||
		saved[3].EventSource != "claude.call.failed" {
		t.Fatalf("partial provider diagnostic was promoted or lost: %+v", saved)
	}
}
