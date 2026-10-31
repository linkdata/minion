package action

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/config"
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
	if len(text) > maxAttemptRecordTextBytes || !utf8.ValidString(text) || strings.Contains(text, secret) || !strings.Contains(text, "[truncated]") {
		t.Fatalf("long failure was not bounded after whole-error redaction: %q", text)
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
