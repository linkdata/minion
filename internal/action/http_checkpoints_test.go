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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
