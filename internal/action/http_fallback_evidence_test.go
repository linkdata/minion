package action

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func TestHTTPFailureFallbackPreservesCoreWithoutAcknowledgingRawClaims(t *testing.T) {
	for _, acknowledged := range []bool{true, false} {
		name := "acknowledged review decision"
		if !acknowledged {
			name = "unacknowledged paid critic claim"
		}
		t.Run(name, func(t *testing.T) {
			f := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
			store := f.startUsage()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			transport := newHTTPServerTest(t, ctx, f.job, f.sink, nil)
			transport.pull(t)
			base, err := url.Parse(transport.server.URL)
			if err != nil {
				t.Fatal(err)
			}
			client := &httpClientCheckpoints{transport: &httpExecutionClient{client: transport.server.Client(), base: base}, control: ctx, action: ctx, binding: httpJobBinding(f.job)}
			result := privateReviewResult(f.job)
			for i := range result.Calls {
				result.Calls[i].Tokens = usage.Tokens{Input: int64(i+1) * 10, CachedInput: int64(i % 2), Output: int64(i + 1)}
				result.Calls[i].Complete = true
			}
			if !acknowledged {
				// The raw critic claim is finite, but its After and stopping Review are never sent.
				result.Calls = result.Calls[:2]
				result.Calls[1].Approved, result.Calls[1].NoBlockers, result.Calls[1].HasBlockers = true, true, false
				result.Refutations = nil
			}
			if err := result.Validate(f.job); err != nil {
				t.Fatalf("finite core fixture is invalid before introducing the wire-only oversized answer: %v", err)
			}
			if err := client.Review(f.review(nil, nil, "")); err != nil {
				t.Fatal(err)
			}
			for i, call := range result.Calls {
				if err := client.BeforeCall(f.call(callMetadata(call))); err != nil {
					t.Fatal(err)
				}
				if !acknowledged && i == 1 {
					break
				}
				if err := client.AfterCall(f.call(call)); err != nil {
					t.Fatal(err)
				}
				var refs []Refutation
				if i >= 2 {
					refs = result.Refutations
				}
				if err := client.Review(f.review(result.Calls[:i+1], refs, "")); err != nil {
					t.Fatal(err)
				}
			}
			if acknowledged {
				if err := client.Review(f.review(result.Calls, result.Refutations, result.CriticStopReason)); err != nil {
					t.Fatal(err)
				}
			}
			usagePath := filepath.Join(store.Dir, f.job.Identity.AttemptID+".json")
			reviewPath := filepath.Join(f.owner.WorkRoot, "reviews", f.job.Identity.AttemptID+".json")
			usageBefore, err := os.ReadFile(usagePath)
			if err != nil {
				t.Fatal(err)
			}
			reviewBefore, err := os.ReadFile(reviewPath)
			if err != nil {
				t.Fatal(err)
			}
			callsBefore := slices.Clone(f.sink.calls)
			refsBefore := slices.Clone(f.sink.review.Refutations)
			reasonBefore := f.sink.review.CriticStopReason
			// Real Runner clears invalid Answer before returning; craft it only at this defensive wire boundary.
			result.Answer = strings.Repeat("x", maxOutputBytes+1)
			original := result
			original.Calls, original.Refutations = slices.Clone(result.Calls), slices.Clone(result.Refutations)
			workerErr := client.outcome(result, nil)
			got := transport.result(t)
			finalErr := f.sink.finish(got.result, got.err)
			if !errors.Is(workerErr, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(got.err, monitor.ErrExecutionOutcomeUnknown) || !got.result.matchesJob(f.job) || got.result.Answer != "" || !slices.Equal(got.result.Calls, result.Calls) || !slices.Equal(got.result.Refutations, result.Refutations) || got.result.CriticStopReason != result.CriticStopReason || !reflect.DeepEqual(result, original) {
				t.Fatalf("bounded failure lost exact raw core or mutated original: worker=%v parent=%v raw=%+v", workerErr, got.err, got.result)
			}
			if acknowledged {
				if finalErr != got.err || errors.Is(finalErr, monitor.ErrTerminalJob) {
					t.Fatalf("matching core gained a checkpoint mismatch: parent=%v finish=%v", got.err, finalErr)
				}
			} else if !errors.Is(finalErr, monitor.ErrExecutionOutcomeUnknown) || !errors.Is(finalErr, monitor.ErrTerminalJob) || !strings.Contains(finalErr.Error(), "execution checkpoint contract violated") || f.sink.pending == nil || f.sink.returned != nil {
				t.Fatalf("unacknowledged raw claim passed the publication checkpoint gate: %v", finalErr)
			}
			if finalErr == nil || got.result.Validate(f.job) == nil {
				t.Fatal("failure fallback became a publishable complete result")
			}
			usageAfter, err := os.ReadFile(usagePath)
			if err != nil {
				t.Fatal(err)
			}
			reviewAfter, err := os.ReadFile(reviewPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(usageAfter, usageBefore) || !bytes.Equal(reviewAfter, reviewBefore) || !slices.Equal(f.sink.calls, callsBefore) || !slices.Equal(f.sink.review.Refutations, refsBefore) || f.sink.review.CriticStopReason != reasonBefore {
				t.Fatal("raw fallback changed saved usage or acknowledged private evidence")
			}
			records, err := usage.ReadAll(store.Dir)
			if err != nil || len(records) != 1 {
				t.Fatalf("durable usage missing: %+v error=%v", records, err)
			}
			wantTokens, wantCodex, wantClaude := (usage.Tokens{Input: 150, CachedInput: 2, Output: 15}), 2, 3
			if !acknowledged {
				wantTokens, wantCodex, wantClaude = result.Calls[0].Tokens, 1, 1
				if len(f.sink.failureEvidence().Calls) != 1 || reasonBefore != "" {
					t.Fatal("unacknowledged raw call/decision became retained checkpoint evidence")
				}
			}
			if record := records[0]; record.Tokens != wantTokens || record.CodexCalls != wantCodex || record.ClaudeCalls != wantClaude || record.UsageComplete != acknowledged {
				t.Fatalf("fallback promoted a raw paid claim beyond durable lower bound: %+v", record)
			}
			transport.executor.mu.Lock()
			unavailable := transport.executor.unavailable
			transport.executor.mu.Unlock()
			if !unavailable {
				t.Fatal("omitted primary payload did not quarantine unknown fallback capacity")
			}
		})
	}
}
