package action

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/linkdata/minion/internal/aicli"
	"github.com/linkdata/minion/internal/command"
	"github.com/linkdata/minion/internal/monitor"
	"github.com/linkdata/minion/internal/usage"
)

func TestHTTPCheckpointsKeepActualStoreRefusalsAndDurablePrefix(t *testing.T) {
	requireActionProcessGroup(t)
	for _, stage := range []string{"before", "after", "approved decision", "capped decision", "optional usage"} {
		t.Run(stage, func(t *testing.T) {
			job := withClaudeCritic(testJob(command.Comment))
			job.CriticMaxRounds = 1
			f := newExecutionAdapterFixture(t, job)
			var store *usage.Store
			var durableUsage string
			if stage != "optional usage" {
				store = f.startUsage()
				durableUsage = store.Dir
			}
			blocked := filepath.Join(t.TempDir(), "blocked")
			if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
			remote, err := NewHTTPExecutor(f.job.Identity.ExecutorID)
			if err != nil {
				t.Fatal(err)
			}
			var cut atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/"+stage && (stage == "before" || stage == "after") {
					store.Dir = blocked
					cut.Store(true)
				}
				if r.URL.Path == "/review" && (stage == "approved decision" || stage == "capped decision") {
					var value httpReview
					if err := httpDecode(r.Body, httpReviewBytes, &value); err != nil {
						t.Error(err)
						http.Error(w, "decode fixture", 400)
						return
					}
					if value.Checkpoint.CriticStopReason != "" {
						dir := filepath.Join(f.owner.WorkRoot, "reviews")
						if err := os.Rename(dir, dir+"-durable"); err != nil {
							t.Error(err)
							http.Error(w, "rename fixture", 500)
							return
						}
						if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
							t.Error(err)
							http.Error(w, "block fixture", 500)
							return
						}
						cut.Store(true)
					}
					body, err := httpEncode(value, httpReviewBytes)
					if err != nil {
						t.Error(err)
						http.Error(w, "encode fixture", 500)
						return
					}
					_ = r.Body.Close()
					r.Body = io.NopCloser(bytes.NewReader(body))
				}
				remote.ServeHTTP(w, r)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			defer remote.Close()
			type returned struct {
				result Result
				err    error
			}
			done := make(chan returned, 1)
			go func() { result, err := remote.Execute(ctx, f.job, f.sink); done <- returned{result, err} }()
			calls := 0
			runner := Runner{WorkRoot: t.TempDir(), runModel: func(_ context.Context, request modelRun) (aicli.Result, error) {
				calls++
				answer := proposalJSON("Candidate", "")
				if request.role == roleCritic {
					answer = approvedCritiqueJSON
					if stage == "capped decision" {
						answer = criticFeedbackJSON(true, "Private blocker")
					}
				}
				return aicli.Result{Answer: answer, Tokens: usage.Tokens{Input: 7, Output: 2}, UsageComplete: true}, nil
			}}
			workerErr := RunHTTP(ctx, server.Client(), server.URL, f.job.Identity.ExecutorID, &runner)
			var got returned
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("parent did not finish acknowledged failed outcome")
			}
			finalErr := f.sink.finish(got.result, got.err)
			if stage == "optional usage" {
				if workerErr != nil || finalErr != nil || cut.Load() || calls != 2 || got.result.Validate(f.job) != nil {
					t.Fatalf("optional accounting changed HTTP execution: calls=%d worker=%v final=%v", calls, workerErr, finalErr)
				}
				return
			}
			wantCalls := 2
			if stage == "before" {
				wantCalls = 0
			} else if stage == "after" {
				wantCalls = 1
			}
			if !cut.Load() || calls != wantCalls || workerErr == nil || finalErr == nil || !errors.Is(finalErr, syscall.ENOTDIR) || errors.Is(finalErr, monitor.ErrExecutionOutcomeUnknown) || errors.Is(workerErr, monitor.ErrExecutionOutcomeUnknown) || len(got.result.Calls) != wantCalls {
				t.Fatalf("store refusal lost known paid facts/cause or continued: calls=%d raw=%+v worker=%v final=%v", calls, got.result, workerErr, finalErr)
			}
			records, err := usage.ReadAll(durableUsage)
			if err != nil || len(records) != 1 {
				t.Fatalf("durable usage missing: %+v %v", records, err)
			}
			record := records[0]
			if stage == "before" {
				if record.CodexCalls != 0 || !record.UsageComplete || record.Tokens != (usage.Tokens{}) {
					t.Fatalf("failed pre-save claimed dispatch: %+v", record)
				}
			} else if stage == "after" {
				if record.CodexCalls != 1 || record.UsageComplete || record.Tokens != (usage.Tokens{}) || f.sink.returned == nil || f.sink.returned.Tokens != got.result.Calls[0].Tokens {
					t.Fatalf("failed post-save falsified durability or lost returned facts: %+v raw=%+v", record, got.result)
				}
			} else {
				var saved retainedReview
				data, err := os.ReadFile(filepath.Join(f.owner.WorkRoot, "reviews-durable", f.job.Identity.AttemptID+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := httpDecode(bytes.NewReader(data), httpReviewBytes, &saved); err != nil {
					t.Fatal(err)
				}
				if record.Tokens != (usage.Tokens{Input: 14, Output: 4}) || !record.UsageComplete || saved.CriticStopReason != "" || !slices.Equal(saved.Calls, got.result.Calls) || f.sink.review.CriticStopReason != "" {
					t.Fatalf("failed separate decision replaced acknowledged prefix: usage=%+v saved=%+v raw=%+v", record, saved, got.result)
				}
			}
		})
	}
}

func TestHTTPRejectsMalformedCompletedCallBeforeSavingUsage(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*CallUsage)
	}{
		{"invalid tokens", func(call *CallUsage) { call.Tokens.Input = -1 }},
		{"main claims critic verdict", func(call *CallUsage) { call.Approved, call.NoBlockers, call.ReviewComplete = true, true, true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			owner := newExecutionAdapterFixture(t, testJob(command.Comment))
			store := owner.startUsage()
			f := newHTTPServerTest(t, t.Context(), owner.job, owner.sink, nil)
			f.pull(t)
			f.ack(t, "/before", f.call(1), http.StatusOK, 1)
			usagePath := filepath.Join(store.Dir, owner.job.Identity.AttemptID+".json")
			before, err := os.ReadFile(usagePath)
			if err != nil {
				t.Fatal(err)
			}
			paid := f.call(2)
			test.change(&paid.Checkpoint.Call)
			ack := f.ack(t, "/after", paid, http.StatusInternalServerError, 2)
			if ack.Failure == nil || !errors.Is(ack.Failure.err(), monitor.ErrTerminalJob) || owner.sink.returned != nil || len(owner.sink.calls) != 0 {
				t.Fatalf("malformed completed call reached saved receipts: ack=%+v returned=%+v calls=%+v", ack, owner.sink.returned, owner.sink.calls)
			}
			after, err := os.ReadFile(usagePath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("malformed completed call changed pre-dispatch usage: error=%v", err)
			}
			outcome := f.outcome(3, errors.New("worker stopped after rejected receipt"))
			f.ack(t, "/outcome", outcome, http.StatusOK, 3)
			got := f.result(t)
			if err := owner.sink.finish(got.result, got.err); !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("rejected paid callback permitted a final result: %v", err)
			}
			records, err := usage.ReadAll(store.Dir)
			if err != nil || len(records) != 1 || records[0].CodexCalls != 1 || records[0].Tokens != (usage.Tokens{}) || records[0].UsageComplete {
				t.Fatalf("rejected paid callback changed the durable lower bound: records=%+v error=%v", records, err)
			}
		})
	}
}

func TestHTTPFailedOutcomeCannotChangeAcknowledgedReview(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Result)
	}{
		{"change refutation", func(result *Result) { result.Refutations[0].Text = "FORGED_REFUTATION" }},
		{"omit refutation", func(result *Result) { result.Refutations = nil }},
		{"omit decision", func(result *Result) { result.CriticStopReason = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			owner := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
			store := owner.startUsage()
			want := privateReviewResult(owner.job)
			f := newHTTPServerTest(t, t.Context(), owner.job, owner.sink, nil)
			f.pull(t)
			sequence := uint64(1)
			f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: owner.review(nil, nil, "")}, http.StatusOK, sequence)
			sequence++
			for i, call := range want.Calls {
				f.ack(t, "/before", httpCall{Sequence: sequence, Checkpoint: owner.call(callMetadata(call))}, http.StatusOK, sequence)
				sequence++
				f.ack(t, "/after", httpCall{Sequence: sequence, Checkpoint: owner.call(call)}, http.StatusOK, sequence)
				sequence++
				var refs []Refutation
				if call.Round >= 2 {
					refs = want.Refutations
				}
				f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: owner.review(want.Calls[:i+1], refs, "")}, http.StatusOK, sequence)
				sequence++
			}
			f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: owner.review(want.Calls, want.Refutations, want.CriticStopReason)}, http.StatusOK, sequence)
			sequence++
			reviewPath := filepath.Join(owner.owner.WorkRoot, "reviews", owner.job.Identity.AttemptID+".json")
			usagePath := filepath.Join(store.Dir, owner.job.Identity.AttemptID+".json")
			beforeReview, err := os.ReadFile(reviewPath)
			if err != nil {
				t.Fatal(err)
			}
			beforeUsage, err := os.ReadFile(usagePath)
			if err != nil {
				t.Fatal(err)
			}
			outcome := f.outcome(sequence, errors.New("worker stopped after review"))
			outcome.Result = want
			outcome.Result.Calls = slices.Clone(want.Calls)
			outcome.Result.Refutations = slices.Clone(want.Refutations)
			test.change(&outcome.Result)
			if err := outcome.Result.validate(owner.job, false); err != nil {
				t.Fatalf("hostile outcome must otherwise satisfy the partial Result contract: %v", err)
			}
			f.ack(t, "/outcome", outcome, http.StatusOK, sequence)
			got := f.result(t)
			if err := owner.sink.finish(got.result, got.err); !errors.Is(err, monitor.ErrTerminalJob) {
				t.Fatalf("failed outcome changed acknowledged review: error=%v result=%+v", err, got.result)
			}
			afterReview, reviewErr := os.ReadFile(reviewPath)
			afterUsage, usageErr := os.ReadFile(usagePath)
			if reviewErr != nil || usageErr != nil || !bytes.Equal(beforeReview, afterReview) || !bytes.Equal(beforeUsage, afterUsage) || !slices.Equal(owner.sink.review.Refutations, want.Refutations) || owner.sink.review.CriticStopReason != want.CriticStopReason {
				t.Fatalf("hostile outcome replaced saved evidence: review=%v usage=%v acknowledged=%+v", reviewErr, usageErr, owner.sink.review)
			}
		})
	}
}

func TestHTTPRejectsRefutationAddedAfterLaterCriticCall(t *testing.T) {
	owner := newExecutionAdapterFixture(t, withClaudeCritic(testJob(command.Comment)))
	want := privateReviewResult(owner.job)
	f := newHTTPServerTest(t, t.Context(), owner.job, owner.sink, nil)
	f.pull(t)
	sequence := uint64(1)
	f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: owner.review(nil, nil, "")}, http.StatusOK, sequence)
	sequence++
	for i, call := range want.Calls[:4] {
		f.ack(t, "/before", httpCall{Sequence: sequence, Checkpoint: owner.call(callMetadata(call))}, http.StatusOK, sequence)
		sequence++
		f.ack(t, "/after", httpCall{Sequence: sequence, Checkpoint: owner.call(call)}, http.StatusOK, sequence)
		sequence++
		f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: owner.review(want.Calls[:i+1], nil, "")}, http.StatusOK, sequence)
		sequence++
	}
	reviewPath := filepath.Join(owner.owner.WorkRoot, "reviews", owner.job.Identity.AttemptID+".json")
	before, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	late := owner.review(want.Calls[:4], want.Refutations, "")
	if err := (Result{Version: late.Version, Identity: late.Identity, SourceRef: late.SourceRef, Calls: late.Calls, Refutations: late.Refutations}).validate(owner.job, false); err != nil {
		t.Fatalf("delayed refutation must otherwise satisfy the partial Result contract: %v", err)
	}
	ack := f.ack(t, "/review", httpReview{Sequence: sequence, Checkpoint: late}, http.StatusInternalServerError, sequence)
	if ack.Failure == nil || !errors.Is(ack.Failure.err(), monitor.ErrTerminalJob) {
		t.Fatalf("late refutation was acknowledged: %+v", ack)
	}
	after, err := os.ReadFile(reviewPath)
	if err != nil || !bytes.Equal(before, after) || len(owner.sink.review.Refutations) != 0 {
		t.Fatalf("late refutation changed acknowledged review: error=%v review=%+v", err, owner.sink.review)
	}
	sequence++
	outcome := f.outcome(sequence, errors.New("worker stopped after rejected review"))
	outcome.Result.Calls = slices.Clone(want.Calls[:4])
	f.ack(t, "/outcome", outcome, http.StatusOK, sequence)
	got := f.result(t)
	if err := owner.sink.finish(got.result, got.err); !errors.Is(err, monitor.ErrTerminalJob) {
		t.Fatalf("late refutation permitted a final result: %v", err)
	}
}

func TestHTTPFailedOutcomeWithoutCriticCannotInventReview(t *testing.T) {
	for _, field := range []string{"refutation", "decision"} {
		t.Run(field, func(t *testing.T) {
			owner := newExecutionAdapterFixture(t, testJob(command.Comment))
			store := owner.startUsage()
			f := newHTTPServerTest(t, t.Context(), owner.job, owner.sink, nil)
			f.pull(t)
			f.ack(t, "/before", f.call(1), http.StatusOK, 1)
			paid := f.call(2)
			paid.Checkpoint.Call.Tokens, paid.Checkpoint.Call.Complete = usage.Tokens{Input: 3}, true
			f.ack(t, "/after", paid, http.StatusOK, 2)
			records, err := usage.ReadAll(store.Dir)
			if err != nil || len(records) != 1 || records[0].ExecutorID != owner.job.Identity.ExecutorID || records[0].SourceRef != owner.job.Source.Head || records[0].UsageSource != "executor_reported" {
				t.Fatalf("saved usage lacks parent binding or claim provenance: records=%+v error=%v", records, err)
			}
			usagePath := filepath.Join(store.Dir, owner.job.Identity.AttemptID+".json")
			beforeUsage, err := os.ReadFile(usagePath)
			if err != nil {
				t.Fatal(err)
			}
			outcome := f.outcome(3, errors.New("worker stopped after paid call"))
			outcome.Result.Calls = []CallUsage{paid.Checkpoint.Call}
			if field == "refutation" {
				outcome.Result.Refutations = []Refutation{{Round: 2, Text: "FORGED_REFUTATION"}}
			} else {
				outcome.Result.CriticStopReason = "approved"
			}
			f.ack(t, "/outcome", outcome, http.StatusOK, 3)
			got := f.result(t)
			if err := owner.sink.finish(got.result, got.err); !errors.Is(err, monitor.ErrTerminalJob) || len(owner.sink.failureEvidence().Refutations) != 0 || owner.sink.failureEvidence().CriticStopReason != "" {
				t.Fatalf("failed outcome invented review evidence: error=%v evidence=%+v", err, owner.sink.failureEvidence())
			}
			afterUsage, err := os.ReadFile(usagePath)
			if err != nil || !bytes.Equal(beforeUsage, afterUsage) {
				t.Fatalf("failed outcome changed acknowledged usage: error=%v", err)
			}
		})
	}
}
